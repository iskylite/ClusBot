package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type revisionFixtureProvider struct {
	benchFixtureProvider
	launches []BenchmarkJobRequest
}

func (p *revisionFixtureProvider) Start(ctx context.Context, r BenchmarkJobRequest) (string, error) {
	p.launches = append(p.launches, r)
	p.jobOverride = "job-" + r.Case
	return p.benchFixtureProvider.Start(ctx, r)
}

func (p *revisionFixtureProvider) Get(ctx context.Context, r BenchmarkJobRequest, id string) (BenchmarkJobResult, error) {
	result, err := p.benchFixtureProvider.Get(ctx, r, id)
	if err == nil {
		result.Measurements[0].ActualConfig = r.Parameters
	}
	return result, err
}

func TestBenchmarkRevisionPreservesDispatchedCase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "revision.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const actor = "user:session"
	provider := &revisionFixtureProvider{}
	required := map[string]string{"rate": "GB/s"}
	binding := BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1}
	caseFor := func(key, size string, deps ...string) BenchmarkCase {
		params := map[string]string{"gpu_count": size}
		return BenchmarkCase{Key: key, Binding: binding, Targets: []string{"node-1"}, Parameters: params, ActualConfig: params,
			RequiredMetrics: required, DependsOn: deps}
	}
	first, second, third := caseFor("first", "40"), caseFor("second", "40", "first"), caseFor("third", "40", "first")
	base := BenchmarkProfile{Key: "full", Version: 1, Cases: []BenchmarkCase{first, second, third}}
	if err := s.PublishBenchmarkProfile(ctx, actor, base); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "serial-1", Benchmark: []DomainBenchmarkGrant{{
		Connection: "bench-test", Revision: 1, Tool: "test", Allowed: map[string][]string{"gpu_count": {"40", "32"}}, ExpiresAt: now() + 120000,
	}}}}); err != nil {
		t.Fatal(err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"launch-key", "full", 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if ok, err := s.runBenchmarkOnce(ctx, provider); err != nil || !ok {
			t.Fatalf("advance: %v %v", ok, err)
		}
	}
	advanceClock := func() {
		t.Helper()
		if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
			t.Fatal(err)
		}
	}
	run() // first case is now "following"
	propose := func(text string) (BenchmarkRevisionProposal, error) {
		t.Helper()
		return s.ProposeBenchmarkRevision(ctx, actor, task, BenchmarkRevisionProposalInput{ExpectedRevision: 1, Text: text}, provider)
	}
	check, err := propose("请将测试项 second 的参数 gpu_count 改为 32 并同步实测要求。")
	if err != nil || check.Method != "explicit_shortcut" || check.State != "preview_only" || check.Preview.Revision != 2 ||
		check.Preview.Profile.Cases[1].Parameters["gpu_count"] != "32" || check.Preview.Profile.Cases[1].ActualConfig["gpu_count"] != "32" ||
		len(check.Preview.Frozen) != 1 || check.Preview.Frozen[0] != "first" {
		t.Fatalf("wrong shortcut draft: %+v %v", check, err)
	}
	remove, err := propose("移除测试项 third")
	if err != nil || len(remove.Preview.Changes.Removed) != 1 || remove.Preview.Changes.Removed[0] != "third" {
		t.Fatalf("remove instruction: %+v %v", remove, err)
	}
	for _, test := range []struct {
		text string
		want error
	}{
		{"移除测试项 third，然后换成机外测试", ErrBenchmarkInput},
		{"移除测试项 unknown", ErrBenchmarkInput},
		{"将测试项 second 的参数 ‘gpu_count’ 改为 32", ErrBenchmarkInput},
		{"将测试项 second 的参数 gpu_count 改为 32", ErrBenchmarkInput},
		{"将测试项 second 的参数 gpu_count 改为 72 并同步实测要求", ErrAuthorizationDenied},
		{"移除测试项 first", ErrBenchmarkInput},
		{"将测试项 first 的参数 gpu_count 改为 32 并同步实测要求", ErrConflict},
	} {
		if _, err := propose(test.text); !errors.Is(err, test.want) {
			t.Fatalf("unsafe or ambiguous proposal %q: %v expected %v", test.text, err, test.want)
		}
	}
	if _, err := s.ProposeBenchmarkRevision(ctx, "other", task, BenchmarkRevisionProposalInput{ExpectedRevision: 1, Text: "移除测试项 third"}, provider); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign proposal accessed task: %v", err)
	}
	if _, err := s.ProposeBenchmarkRevision(ctx, actor, task, BenchmarkRevisionProposalInput{ExpectedRevision: 7, Text: "移除测试项 third"}, provider); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale proposal base accepted: %v", err)
	}
	const spoken = "第二项的卡数降到32，实际验收也按这个数"
	modelInput := BenchmarkRevisionAIInput{BenchmarkRevisionProposalInput: BenchmarkRevisionProposalInput{ExpectedRevision: 1, Text: spoken}, AllowExpectedActualChange: true}
	var modelCalls int
	interpret := func(raw string) BenchmarkRevisionModel {
		return func(_ context.Context, input BenchmarkRevisionModelInput, limit int) (string, error) {
			modelCalls++
			if input.Text != spoken || limit < 32 || limit > 512 || len(input.Cases) != 2 ||
				input.Cases[0].Key != "second" || input.Cases[1].Key != "third" || len(input.Cases[0].Parameters) != 1 ||
				input.Cases[0].Parameters[0].Key != "gpu_count" {
				t.Fatalf("unscoped model input: %+v, tokens=%d", input, limit)
			}
			return raw, nil
		}
	}
	const answer = `{"action":"set_parameter","case_key":"second","parameter":"gpu_count","value":"32","change_expected_actual_config":true,"quote":"第二项的卡数降到32，实际验收也按这个数"}`
	ai, err := s.ProposeBenchmarkRevisionAI(ctx, actor, task, modelInput, provider, interpret(answer))
	if err != nil || ai.Method != "model_interpreted" || ai.State != "preview_only" ||
		ai.Preview.Profile.Cases[1].Parameters["gpu_count"] != "32" || ai.Preview.Profile.Cases[1].ActualConfig["gpu_count"] != "32" {
		t.Fatalf("unstructured language did not produce validated candidate: %+v %v", ai, err)
	}
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"[" + answer + "]", ErrBenchmarkRevisionModel},
		{answer + answer, ErrBenchmarkRevisionModel},
		{`{"action":"clarify","action":"clarify"}`, ErrBenchmarkRevisionModel},
		{answer[:len(answer)-1] + `,"target":"outside"}`, ErrBenchmarkRevisionModel},
		{strings.Replace(answer, spoken, "模型添加的内容", 1), ErrBenchmarkRevisionModel},
		{`{"action":"clarify"}`, ErrBenchmarkInput},
		{strings.Replace(answer, "second", "first", 1), ErrBenchmarkInput},
		{strings.Replace(answer, `"value":"32"`, `"value":"2"`, 1), ErrBenchmarkRevisionModel},
	} {
		if _, err := s.ProposeBenchmarkRevisionAI(ctx, actor, task, modelInput, provider, interpret(tc.raw)); !errors.Is(err, tc.want) {
			t.Fatalf("unsafe model output %q: %v want %v", tc.raw, err, tc.want)
		}
	}
	unauthorized := modelInput
	unauthorized.Text = "第二项的卡数降到72，实际验收也按这个数"
	if _, err := s.ProposeBenchmarkRevisionAI(ctx, actor, task, unauthorized, provider,
		func(_ context.Context, _ BenchmarkRevisionModelInput, _ int) (string, error) {
			return strings.ReplaceAll(answer, "32", "72"), nil
		}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("model bypassed authorization: %v", err)
	}

	unconfirmed := modelInput
	unconfirmed.AllowExpectedActualChange = false
	if _, err := s.ProposeBenchmarkRevisionAI(ctx, actor, task, unconfirmed, provider, interpret(answer)); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("model self-authorized changing required actual configuration: %v", err)
	}
	callsBefore := modelCalls
	if _, err := s.ProposeBenchmarkRevisionAI(ctx, "foreign", task, modelInput, provider, interpret(answer)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign AI proposal: %v", err)
	}
	stale := modelInput
	stale.ExpectedRevision = 20
	if _, err := s.ProposeBenchmarkRevisionAI(ctx, actor, task, stale, provider, interpret(answer)); !errors.Is(err, ErrTaskRevision) || modelCalls != callsBefore {
		t.Fatalf("stale/foreign sent to model: %v (%d calls vs %d)", err, modelCalls, callsBefore)
	}

	var savedRevisions int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_run_revisions WHERE task_id=?", task).Scan(&savedRevisions); err != nil || savedRevisions != 0 {
		t.Fatalf("preview persisted a revision: %d %v", savedRevisions, err)
	}

	changed := caseFor("second", "32", "first")
	req := BenchmarkRevisionInput{ExpectedRevision: 1, RequestKey: "change-key", Cases: []BenchmarkCase{first, changed}}
	preview, err := s.ReviseBenchmark(ctx, actor, task, req, provider, false)
	if err != nil || preview.Revision != 2 || len(preview.Frozen) != 1 || preview.Frozen[0] != "first" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	before, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || before.PlanRevision != 1 || len(before.Cases) != 3 || len(provider.launches) != 1 {
		t.Fatalf("preview changed state: %+v %v", before, err)
	}
	try := func(cases ...BenchmarkCase) error {
		t.Helper()
		_, err := s.ReviseBenchmark(ctx, actor, task, BenchmarkRevisionInput{ExpectedRevision: 1, RequestKey: "rejected-edit", Cases: cases}, provider, true)
		return err
	}
	if err := try(caseFor("first", "32"), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("edited frozen case: %v", err)
	}
	if err := try(first, caseFor("second", "32", "third")); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("deleted dependency: %v", err)
	}
	if err := try(first, caseFor("second", "72", "first")); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("allowed unapproved parameters: %v", err)
	}
	other := changed
	other.Targets = []string{"node-outside"}
	if err := try(first, other); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("allowed scope expansion: %v", err)
	}
	var saved BenchmarkRevision
	_, err = s.ProposeBenchmarkRevisionAI(ctx, actor, task, modelInput, provider,
		func(_ context.Context, _ BenchmarkRevisionModelInput, _ int) (string, error) {
			var applyErr error
			saved, applyErr = s.ReviseBenchmark(ctx, actor, task, req, provider, true)
			if applyErr != nil || saved.Revision != 2 {
				t.Fatalf("independent revision applied while model active: %+v %v", saved, applyErr)
			}
			return answer, nil
		})
	if !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("model answer survived a changed durable revision: %v", err)
	}

	if _, err := s.ReviseBenchmark(ctx, actor, task, BenchmarkRevisionInput{ExpectedRevision: 1, RequestKey: "other-edit", Cases: req.Cases}, provider, true); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("accepted concurrent stale edit: %v", err)
	}
	if _, err := s.ReadBenchmarkRevision(ctx, "other", task, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("exposed another actor's plan: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ReviseBenchmark(ctx, actor, task, req, provider, true)
	if err != nil || toJSON(again) != toJSON(saved) {
		t.Fatalf("replay changed version: %+v %v", again, err)
	}
	baseRevision, err := s.ReadBenchmarkRevision(ctx, actor, task, 1)
	if err != nil || toJSON(baseRevision.Profile) != toJSON(base) {
		t.Fatalf("old version changed: %+v %v", baseRevision, err)
	}
	advanceClock()
	run() // completes the original job against its original parameters
	run() // starts revised case with newly frozen parameters
	advanceClock()
	run()
	got, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || got.State != "succeeded" || got.PlanRevision != 2 || len(got.Cases) != 2 ||
		len(provider.launches) != 2 || provider.launches[0].Parameters["gpu_count"] != "40" || provider.launches[1].Parameters["gpu_count"] != "32" {
		t.Fatalf("bad settled state %+v %+v %v", got, provider.launches, err)
	}
}
