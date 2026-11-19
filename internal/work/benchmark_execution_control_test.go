package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestBenchmarkSkipChangesExecutionNotPlanAndKeepsDependencies(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "benchmark-execution-control.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	const actor = "user:session"
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 90000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture-node", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	cases := []BenchmarkCase{
		{Key: "first", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}},
		{Key: "dependent", Binding: binding, Targets: []string{"node-1"}, DependsOn: []string{"first"}, RequiredMetrics: map[string]string{"rate": "GB/s"}},
	}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "full", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	provider := &benchFixtureProvider{}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: "exec-control-task", Profile: "full", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || len(initial.Cases) != 2 {
		t.Fatalf("no initial cases: %+v %v", initial, err)
	}
	first := initial.Cases[0].StepID
	in := BenchmarkExecutionControlInput{Action: benchmarkSkipThisRun, ExpectedPlanRevision: 1, RequestID: "known-skip-request", Reason: "本次不安排这个测项"}
	apply := func(who, step string, opts BenchmarkExecutionControlInput) (BenchmarkExecutionControlReceipt, error) {
		return s.ApplyBenchmarkExecutionControl(ctx, who, task, step, opts)
	}
	if _, err := apply("other", first, in); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign actor: %v", err)
	}
	if _, err := apply(actor, id(), in); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown step: %v", err)
	}
	stale := in
	stale.ExpectedPlanRevision = 2
	if _, err := apply(actor, first, stale); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("old card guard: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='benchmark.execute'", now(), actor)
	if _, err := apply(actor, first, in); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked grant still controls: %v", err)
	}
	exec("UPDATE authorizations SET revoked_at=0 WHERE subject=? AND action='benchmark.execute'", actor)
	exec(`CREATE TRIGGER deny_skip_control BEFORE INSERT ON benchmark_execution_controls
BEGIN SELECT RAISE(ABORT,'receipt failure'); END`)
	if _, err := apply(actor, first, in); err == nil {
		t.Fatal("receipt write failure still reported a skip")
	}
	stillPending, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || stillPending.Cases[0].Phase != "queued" {
		t.Fatalf("failed receipt didn't roll back skip: %+v %v", stillPending, err)
	}
	exec("DROP TRIGGER deny_skip_control")
	applied, err := apply(actor, first, in)
	if err != nil || applied.State != benchmarkSkippedThisRun || applied.PlanRevision != 1 || applied.StepID != first {
		t.Fatalf("skip not applied: %+v %v", applied, err)
	}
	diff := in
	diff.Reason = "不应允许替换已持久回执"
	if _, err := apply(actor, first, diff); !errors.Is(err, ErrConflict) {
		t.Fatalf("different body reused key: %v", err)
	}
	another := in
	another.RequestID = "different-skip-id"
	if _, err := apply(actor, first, another); !errors.Is(err, ErrBenchmarkCaseNotPending) {
		t.Fatalf("new request changed an already skipped case: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	replay, err := apply(actor, first, in)
	if err != nil || replay != applied {
		t.Fatalf("unknown outcome replay lost receipt after restart: %+v %v", replay, err)
	}
	// An explicitly skipped prerequisite never unlocks its dependent.
	if worked, err := s.runBenchmarkOnce(ctx, provider); !worked || err != nil {
		t.Fatalf("cannot advance dependent: %t %v", worked, err)
	}
	report, err := s.ReadBenchmarkReport(ctx, actor, task)
	if err != nil || report.PlanRevision != 1 || len(report.Cases) != 2 || report.TaskState != "failed" {
		t.Fatalf("skip altered plan denominator or claimed completion: %+v %v", report, err)
	}
	for i, c := range report.Cases {
		if c.Phase != "skipped" || c.ToolOutcome != "not_started" || (i == 0) != (c.ExecutionControl != nil) {
			t.Fatalf("incorrect independent step disposition: %+v", c)
		}
	}
	if report.Cases[0].RequiredEvidence != "incomplete" || report.Cases[0].Reason != in.Reason || report.AssetAcceptance != "not_evaluated" {
		t.Fatalf("skip presented as approved or passed: %+v", report)
	}
	revision, err := s.ReadBenchmarkRevision(ctx, actor, task, 0)
	if err != nil || revision.Revision != 1 || len(revision.Profile.Cases) != 2 || revision.Profile.Cases[1].DependsOn[0] != "first" {
		t.Fatalf("plan or dependency was removed: %+v %v", revision, err)
	}
	if provider.started != 0 || provider.looked != 0 || provider.read != 0 {
		t.Fatalf("control dispatched provider: %+v", provider)
	}
}

func TestBenchmarkExecutionSkipVersusDispatch(t *testing.T) {
	for _, name := range []string{"dispatch_wins", "skip_wins", "concurrent"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "execution-race.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			const actor = "user:session"
			b := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
			g := DomainBenchmarkGrant{Revision: 1, Connection: b.Connection, Tool: b.Tool, ExpiresAt: now() + 90000}
			if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []DomainBenchmarkGrant{g}}}); err != nil {
				t.Fatal(err)
			}
			c := BenchmarkCase{Key: "first", Binding: b, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
			if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "race", Version: 1, Cases: []BenchmarkCase{c}}); err != nil {
				t.Fatal(err)
			}
			p := &benchFixtureProvider{}
			task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: "race-request", Profile: "race", Version: 1}, p)
			if err != nil {
				t.Fatal(err)
			}
			base, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || len(base.Cases) != 1 {
				t.Fatalf("missing case: %+v %v", base, err)
			}
			input := BenchmarkExecutionControlInput{Action: benchmarkSkipThisRun, ExpectedPlanRevision: 1, RequestID: "race-control", Reason: "当前不执行"}
			// These are the two possible serializations of the same write
			// race; the second never bypasses the first transaction's decision.
			skip := func() error {
				_, err := s.ApplyBenchmarkExecutionControl(ctx, actor, task, base.Cases[0].StepID, input)
				return err
			}
			if name == "concurrent" {
				type dispatchResult struct {
					action string
					runErr error
				}
				begin := make(chan struct{})
				dispatched, skipped := make(chan dispatchResult, 1), make(chan error, 1)
				go func() {
					<-begin
					d, _, err := s.claimBenchmark(ctx, p, now())
					dispatched <- dispatchResult{action: d.action, runErr: err}
				}()
				go func() {
					<-begin
					skipped <- skip()
				}()
				close(begin)
				result, err := <-dispatched, <-skipped
				if result.runErr != nil || result.action == "start" && !errors.Is(err, ErrBenchmarkCaseNotPending) ||
					result.action != "start" && err != nil {
					t.Fatalf("dispatch and skip were not mutually exclusive: %+v %v", result, err)
				}
				state, err := s.BenchmarkProgress(ctx, actor, task)
				if err != nil || (state.Cases[0].ExecutionControl != nil) == (state.Cases[0].Operation != nil) {
					t.Fatalf("real concurrent writes left both/neither action applied: %+v %v", state, err)
				}
				return
			}
			if name == "skip_wins" {
				if err := skip(); err != nil {
					t.Fatal(err)
				}
				if d, taken, err := s.claimBenchmark(ctx, p, now()); err != nil || taken || d.action != "" {
					t.Fatalf("skip won but dispatch was claimed: %+v %t %v", d, taken, err)
				}
			} else {
				d, taken, err := s.claimBenchmark(ctx, p, now())
				if err != nil || !taken || d.action != "start" {
					t.Fatalf("dispatch did not win: %+v %t %v", d, taken, err)
				}
				if err := skip(); !errors.Is(err, ErrBenchmarkCaseNotPending) {
					t.Fatalf("skip overrode dispatch: %v", err)
				}
				state, err := s.BenchmarkProgress(ctx, actor, task)
				if err != nil || state.Cases[0].Operation == nil || state.Cases[0].Operation.ID != d.operation || state.Cases[0].TargetHolds != 1 || state.Cases[0].ExecutionControl != nil {
					t.Fatalf("failed skip mutated held op: %+v %v", state, err)
				}
			}
		})
	}
}
