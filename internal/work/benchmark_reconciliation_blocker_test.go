package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

type benchmarkBindingOnly struct {
	BenchmarkProvider // Panics if the read ever calls an external method.
	binding           BenchmarkBinding
}

func (p benchmarkBindingOnly) Supports(binding BenchmarkBinding) bool { return p.binding == binding }

func TestBenchmarkReconciliationBlockerUsesExactLocalAdapter(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "adapter-blocker.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: now() + 120000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	const actor = "user:session"
	p := BenchmarkProfile{Key: "plan", Version: 1, Cases: []BenchmarkCase{{Key: "single", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err := s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	worker := &benchFixtureProvider{loseReply: true}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"adapter-fixture", "plan", 1}, worker)
	if err != nil {
		t.Fatal(err)
	}
	matching := benchmarkBindingOnly{binding: binding}
	changed := binding
	changed.Revision++
	mismatch := benchmarkBindingOnly{binding: changed}
	check := func(provider BenchmarkProvider, wantPhase string, blocked bool, holds int) BenchmarkCaseProgress {
		t.Helper()
		status, err := s.BenchmarkProgressWithProvider(ctx, actor, task, provider)
		if err != nil || len(status.Cases) != 1 {
			t.Fatalf("status: %+v (%v)", status, err)
		}
		c := status.Cases[0]
		if c.Phase != wantPhase || c.TargetHolds != holds || (c.ReconciliationBlocker != nil) != blocked {
			t.Fatalf("phase=%q case=%+v", wantPhase, c)
		}
		if blocked && (c.ReconciliationBlocker.Code != "adapter_revision_unavailable" || c.ReconciliationBlocker.Reason == "" ||
			c.Operation == nil || c.Wait == nil || c.Wait.State != "open" || c.Targets[0].LocalHold != "held_by_case") {
			t.Fatalf("incorrect blocker or lost in-flight state: %+v", c)
		}
		return c
	}
	tick := func() {
		t.Helper()
		if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
			t.Fatal(err)
		}
		if ran, err := s.runBenchmarkOnce(ctx, worker); !ran || err != nil {
			t.Fatalf("fixture tick: %t (%v)", ran, err)
		}
	}
	check(nil, "queued", false, 0)
	tick()
	u := check(nil, "submitting", true, 1)
	check(mismatch, "submitting", true, 1)
	check(matching, "submitting", false, 1)
	if out, err := s.BenchmarkProgressWithProvider(ctx, "other-actor", task, nil); !errors.Is(err, sql.ErrNoRows) || len(out.Cases) != 0 {
		t.Fatalf("other actor observed task: %+v (%v)", out, err)
	}
	tick()
	f := check(mismatch, "following", true, 1)
	if f.Operation.ID != u.Operation.ID || f.JobID != "job-ID" || worker.started != 1 || worker.looked != 1 || worker.read != 0 {
		t.Fatalf("did not preserve original operation: %+v %+v %+v", u, f, worker)
	}
	// A stop intent is not a confirmed termination; original effects still need reconciliation.
	if _, err := s.db.Exec("UPDATE benchmark_runs SET stop_requested=1 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	check(nil, "following", true, 1)
	tick()
	terminal, err := s.BenchmarkProgressWithProvider(ctx, actor, task, nil)
	if err != nil || len(terminal.Cases) != 1 {
		t.Fatalf("terminal status: %+v (%v)", terminal, err)
	}
	if c := terminal.Cases[0]; c.TargetHolds != 0 || c.ReconciliationBlocker != nil {
		t.Fatalf("terminal run still blocked: %+v", c)
	}
}
