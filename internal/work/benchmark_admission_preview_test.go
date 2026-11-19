package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBenchmarkAdmissionAIIsOnlyAuthorizedPreview(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "admission-preview.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &benchFixtureProvider{}
	const actor = "user:session"
	spec := BenchmarkCase{Key: "case", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1}, Targets: []string{"node-1"},
		Parameters: map[string]string{"gpu_count": "32"}, ActualConfig: map[string]string{"gpu_count": "32"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "private-serial", Benchmark: []DomainBenchmarkGrant{{
		Connection: "bench-test", Revision: 1, Tool: "test", Allowed: map[string][]string{"gpu_count": {"32"}}, ExpiresAt: now() + 60000,
	}}}}); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []int{1, 2} {
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "full", Version: rev, Cases: []BenchmarkCase{spec}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"POD7", "POD8"} {
		if err := s.SetBenchmarkChatTarget(ctx, actor, target, &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	const spoken = "POD7这边的基准全都测一遍，交给你安排"
	const yes = `{"action":"preview_admission","target":"POD7","profile":"full","version":1,"quote":"POD7这边的基准全都测一遍，交给你安排"}`
	calls := 0
	model := func(answer string) BenchmarkAdmissionModel {
		return func(_ context.Context, input BenchmarkAdmissionModelInput, limit int) (string, error) {
			calls++
			body, err := json.Marshal(input)
			if err != nil || strings.Contains(string(body), "private-serial") || strings.Contains(string(body), "gpu_count") {
				t.Fatalf("sensitive raw plan or identity reached model: %s %v", body, err)
			}
			if input.Text != spoken || limit < 32 || limit > 384 || len(input.Candidates) != 1 ||
				input.Candidates[0].Target != "POD7" || input.Candidates[0].Profile.Version != 1 ||
				len(input.Candidates[0].AssetRefs) != 1 || input.Candidates[0].AssetRefs[0] != "node-1" {
				t.Fatalf("wrong authorized bounded model input: %+v", input)
			}
			return answer, nil
		}
	}
	preview := func(who, text string, f BenchmarkAdmissionModel) (BenchmarkAdmissionPreview, error) {
		t.Helper()
		return s.PreviewBenchmarkAdmissionAI(ctx, who, BenchmarkAdmissionPreviewInput{Text: text}, p, f)
	}
	if benchmarkOrderTargetFromText(spoken) != "" {
		t.Fatal("fixture must be outside the old fixed phrase route")
	}
	out, err := preview(actor, spoken, model(yes))
	if err != nil || out.PreviewID == "" || out.State != "preview_only" || out.ExecutionStarted ||
		out.Selection.Profile != (BenchmarkProfileRef{"full", 1}) || out.Preflight.FacilityStatus != "not_verified" || !out.Preflight.CanQueue {
		t.Fatalf("wrong local admission preview: %+v %v", out, err)
	}
	for _, tc := range []struct {
		text string
		want error
	}{
		{"查进度 POD7", ErrBenchmarkAdmissionClarify},
		{"先别跑 POD7 基准", ErrBenchmarkAdmissionClarify},
		{"POD7 和 POD8 都测一下", ErrBenchmarkAdmissionClarify},
		{"POD7 两个任选其一也行或者 POD8", ErrBenchmarkAdmissionClarify},
		{"把 POD7 上的 40 卡都跑一遍", ErrBenchmarkAdmissionScale},
		{"POD7 用四十张卡跑", ErrBenchmarkAdmissionScale},
		{"POD7 改到 40 个目标以后启动", ErrBenchmarkAdmissionScale},
	} {
		previousCalls := calls
		if _, err := preview(actor, tc.text, model(yes)); !errors.Is(err, tc.want) || calls != previousCalls {
			t.Fatalf("rejected text %q reached a model or mismatched error: %v", tc.text, err)
		}
	}
	if _, err := preview("other", spoken, model(yes)); !errors.Is(err, ErrAuthorizationDenied) || calls != 1 {
		t.Fatalf("other actor saw published/asset candidate: %v", err)
	}
	for _, answer := range []string{
		strings.Replace(yes, `"target":"POD7"`, `"target":"POD8"`, 1),
		strings.Replace(yes, `"version":1`, `"version":2`, 1), yes + yes, "[" + yes + "]",
		yes[:len(yes)-1] + `,"version":1}`, yes[:len(yes)-1] + `,"card_count":40}`,
	} {
		if _, err := preview(actor, spoken, model(answer)); !errors.Is(err, ErrBenchmarkAdmissionModel) {
			t.Fatalf("untrusted response interpreted: %q %v", answer, err)
		}
	}
	if _, err := preview(actor, spoken, model(`{"action":"clarify"}`)); !errors.Is(err, ErrBenchmarkAdmissionClarify) {
		t.Fatalf("model uncertainty promoted: %v", err)
	}
	if _, err := preview(actor, spoken, func(call context.Context, input BenchmarkAdmissionModelInput, limit int) (string, error) {
		if err := setBenchmarkTargetAtCurrent(t, s, call, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 2}); err != nil {
			t.Fatal(err)
		}
		return model(yes)(call, input, limit)
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("binding changed while model active not rejected: %v", err)
	}
	var sideEffects int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM benchmark_runs)+(SELECT count(*) FROM operations)+(SELECT count(*) FROM benchmark_target_holds)").Scan(&sideEffects); err != nil ||
		sideEffects != 0 || p.started != 0 || p.looked != 0 || p.read != 0 {
		t.Fatalf("preview caused dispatch or reservations: %d %v", sideEffects, err)
	}
	confirm := BenchmarkAdmissionConfirmationInput{Action: "accept_benchmark_admission"}
	accept := func(who, ref string, in BenchmarkAdmissionConfirmationInput, provider BenchmarkProvider) (BenchmarkAdmissionConfirmation, error) {
		return s.ConfirmBenchmarkAdmission(ctx, who, ref, in, provider)
	}
	if _, err := accept("other", out.PreviewID, confirm, p); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign actor could confirm: %v", err)
	}
	if _, err := accept(actor, out.PreviewID, BenchmarkAdmissionConfirmationInput{Action: "preview"}, p); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("invalid action can queue: %v", err)
	}
	if _, err := accept(actor, out.PreviewID, confirm, p); !errors.Is(err, ErrBenchmarkAdmissionPreviewStale) {
		t.Fatalf("moved published binding did not invalidate preview: %v", err)
	}
	if err := setBenchmarkTargetAtCurrent(t, s, ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := accept(actor, out.PreviewID, confirm, nil); !errors.Is(err, ErrBenchmarkAdmissionPreviewStale) {
		t.Fatalf("missing current capability could queue: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE ops_assets SET revision=revision+1 WHERE object_ref='node-1'")
	if _, err := accept(actor, out.PreviewID, confirm, p); !errors.Is(err, ErrBenchmarkAdmissionPreviewStale) {
		t.Fatalf("changed asset identity/revision could queue: %v", err)
	}
	exec("UPDATE ops_assets SET revision=revision-1 WHERE object_ref='node-1'")
	exec("UPDATE authorizations SET revoked_at=? WHERE action='benchmark.execute' AND subject=?", now(), actor)
	if _, err := accept(actor, out.PreviewID, confirm, p); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked grant could queue: %v", err)
	}
	exec("UPDATE authorizations SET revoked_at=0 WHERE action='benchmark.execute' AND subject=?", actor)
	if _, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{benchmarkAdmissionConfirmPrefix + out.PreviewID, "full", 1}, p); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("raw submission could claim server reference key: %v", err)
	}
	results, errs := make(chan BenchmarkAdmissionConfirmation, 2), make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			receipt, err := accept(actor, out.PreviewID, confirm, p)
			results <- receipt
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	var task string
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.State != "accepted" || result.TaskID == "" || task != "" && task != result.TaskID {
			t.Fatalf("confirmation not idempotent: %+v previous=%s", result, task)
		}
		task = result.TaskID
	}
	if again, err := accept(actor, out.PreviewID, confirm, nil); err != nil || again.TaskID != task {
		t.Fatalf("stable replay requires no provider: %+v %v", again, err)
	}
	var runs, ops, audit int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM benchmark_runs),(SELECT count(*) FROM operations),(SELECT count(*) FROM runtime_audit WHERE event='benchmark_admission_confirmed')").
		Scan(&runs, &ops, &audit); err != nil || runs != 1 || ops != 0 || audit != 1 {
		t.Fatalf("confirmation effects wrong: runs=%d operations=%d audit=%d error=%v", runs, ops, audit, err)
	}
	var chatReceipts int
	if err := s.db.QueryRow("SELECT count(*) FROM run_events WHERE kind=?", benchmarkAdmissionQueuedEvent).Scan(&chatReceipts); err != nil || chatReceipts != 0 {
		t.Fatalf("standalone admission acquired a fabricated conversation source: %d %v", chatReceipts, err)
	}

	if p.started != 0 || p.looked != 0 || p.read != 0 {
		t.Fatal("confirmation dispatched a supplier operation")
	}

}
