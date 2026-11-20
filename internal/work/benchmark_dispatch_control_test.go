package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

const dispatchGateFixtureActor = "user:session"

func newDispatchGateFixture(t *testing.T, keys ...string) (*Store, string, string, []BenchmarkCase, *benchFixtureProvider) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "dispatch-gate.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1}
	g := DomainBenchmarkGrant{Revision: 1, Connection: b.Connection, Tool: b.Tool, ExpiresAt: now() + 90000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []DomainBenchmarkGrant{g}}}); err != nil {
		t.Fatal(err)
	}
	cases := make([]BenchmarkCase, 0, len(keys))
	for _, k := range keys {
		c := BenchmarkCase{Key: k, Binding: b, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if k == "dependent" {
			c.DependsOn = []string{"first"}
		}
		cases = append(cases, c)
	}
	if err := s.PublishBenchmarkProfile(ctx, dispatchGateFixtureActor, BenchmarkProfile{Key: "control", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	p := &benchFixtureProvider{}
	task, err := s.SubmitBenchmarkRun(ctx, dispatchGateFixtureActor, BenchmarkRunRequest{RequestKey: "dispatch-gate-fixture", Profile: "control", Version: 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, task, cases, p
}

func dispatchGateInput(action, request string, plan, gate int) BenchmarkExecutionControlInput {
	return BenchmarkExecutionControlInput{Action: action, ExpectedPlanRevision: plan, ExpectedControlVersion: &gate,
		RequestID: request, Reason: "操作员明确调整待派发测项"}
}

func TestBenchmarkDispatchGateKeepsIndependentWorkAndStopDrain(t *testing.T) {
	ctx := t.Context()
	s, path, task, _, provider := newDispatchGateFixture(t, "first", "dependent", "sibling")
	read := func() BenchmarkRunProgress {
		t.Helper()
		p, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	b := read()
	first := b.Cases[0].StepID
	apply := func(in BenchmarkExecutionControlInput) (BenchmarkExecutionControlReceipt, error) {
		t.Helper()
		return s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, in)
	}
	hold := dispatchGateInput(benchmarkHoldBeforeDispatch, "hold-first-case", 1, 0)
	if _, err := s.ApplyBenchmarkExecutionControl(ctx, "foreign", task, first, hold); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign control: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER deny_gate_receipt BEFORE INSERT ON benchmark_execution_controls
BEGIN SELECT RAISE(ABORT,'receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(hold); err == nil || read().Cases[0].DispatchControl != nil {
		t.Fatalf("receipt failure left an apparent gate: %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER deny_gate_receipt"); err != nil {
		t.Fatal(err)
	}
	r, err := apply(hold)
	if err != nil || r.State != benchmarkDispatchHeld || r.ControlVersion != 1 {
		t.Fatalf("hold receipt: %+v %v", r, err)
	}
	if got, err := apply(hold); err != nil || got != r {
		t.Fatalf("hold replay: %+v %v", got, err)
	}
	legacy := BenchmarkExecutionControlInput{Action: benchmarkSkipThisRun, ExpectedPlanRevision: 1, RequestID: "legacy-override", Reason: "不应绕过暂缓修订"}
	if _, err := apply(legacy); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale old client skip: %v", err)
	}
	report, err := s.ReadBenchmarkReport(ctx, dispatchGateFixtureActor, task)
	if err != nil || len(report.Cases) != 3 || report.Cases[0].Phase != "queued" ||
		report.Cases[0].ToolOutcome != "not_started" || report.Cases[0].RequiredEvidence != "not_evaluated" ||
		report.Cases[0].DispatchControl == nil || report.Cases[0].DispatchControl.State != benchmarkDispatchHeld {
		t.Fatalf("incorrect held report: %+v %v", report, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if advanced, err := s.runBenchmarkOnce(ctx, provider); err != nil || !advanced {
		t.Fatalf("independent case did not start: %t %v", advanced, err)
	}
	b = read()
	c, dependent, sibling := b.Cases[0], b.Cases[1], b.Cases[2]
	if c.Phase != "queued" || c.DispatchControl == nil || c.Operation != nil || c.Wait != nil || c.TargetHolds != 0 ||
		dependent.Phase != "queued" || dependent.Operation != nil ||
		sibling.Operation == nil || sibling.Wait == nil || sibling.TargetHolds != 1 || provider.started != 1 {
		t.Fatalf("hold took or released resources, or blocked independent case: %+v", b)
	}
	// The gate generation prevents an old Resume from releasing a later hold.
	release := dispatchGateInput(benchmarkResumeBeforeDispatch, "resume-first-case", 1, 1)
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='benchmark.execute'", now(), dispatchGateFixtureActor); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(release); !errors.Is(err, ErrAuthorizationDenied) || read().Cases[0].DispatchControl.ControlVersion != 1 {
		t.Fatalf("resume bypassed frozen grant: %v", err)
	}
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=0 WHERE subject=? AND action='benchmark.execute'", dispatchGateFixtureActor); err != nil {
		t.Fatal(err)
	}
	if out, err := apply(release); err != nil || out.State != benchmarkDispatchEligible || out.ControlVersion != 2 {
		t.Fatalf("resume: %+v %v", out, err)
	}
	if c := read().Cases[0]; c.DispatchControl == nil || c.DispatchControl.ControlVersion != 2 || c.Operation != nil {
		t.Fatalf("resume must only restore eligibility: %+v", c)
	}
	again := dispatchGateInput(benchmarkHoldBeforeDispatch, "second-hold-case", 1, 2)
	if _, err := apply(again); err != nil {
		t.Fatal(err)
	}
	if out, err := apply(release); err != nil || out.ControlVersion != 2 || read().Cases[0].DispatchControl.ControlVersion != 3 {
		t.Fatalf("original receipt failed to replay inertly: %+v %v", out, err)
	}
	release.RequestID = "stale-other-resume"
	if _, err := apply(release); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version changed held case: %v", err)
	}
	var taskRevision int
	if err := s.db.QueryRow("SELECT revision FROM tasks WHERE id=?", task).Scan(&taskRevision); err != nil {
		t.Fatal(err)
	}
	if err := s.Control(ctx, task, "stop", taskRevision); err != nil {
		t.Fatal(err)
	}
	// A stopped task may clean up the held queue but cannot release an
	// already registered sibling as a side effect of that gate.
	if still := read().Cases[2]; still.Operation == nil || still.Wait == nil || still.Operation.ID != sibling.Operation.ID || still.Wait.ID != sibling.Wait.ID || still.TargetHolds != 1 {
		t.Fatalf("stop released or replaced the in-flight sibling: %+v", still)
	}
	if c := read().Cases[0]; c.DispatchControl != nil {
		t.Fatalf("stop made an obsolete action look available: %+v", c)
	}
	if _, err := apply(dispatchGateInput(benchmarkResumeBeforeDispatch, "stop-forbids-resume", 1, 3)); !errors.Is(err, ErrBenchmarkDispatchNotPending) {
		t.Fatalf("stopping allowed resume: %v", err)
	}
	if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := s.runBenchmarkOnce(ctx, provider); err != nil {
			t.Fatal(err)
		}
	}
	b = read()
	if b.State != "cancelled" || provider.started != 1 || b.Cases[0].Phase != "skipped" ||
		b.Cases[0].Wait != nil || b.Cases[2].TargetHolds != 0 {
		t.Fatalf("stop did not drain held queue and settle original remote: provider=%+v progress=%+v", provider, b)
	}
}

func TestBenchmarkDispatchGateTravelsOnlyWithSameStep(t *testing.T) {
	ctx := t.Context()
	s, _, task, cases, provider := newDispatchGateFixture(t, "first", "side")
	prog, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil {
		t.Fatal(err)
	}
	first := prog.Cases[0].StepID
	hold := dispatchGateInput(benchmarkHoldBeforeDispatch, "hold-during-revision", 1, 0)
	if _, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, hold); err != nil {
		t.Fatal(err)
	}
	revise := func(plan int, key string, updated []BenchmarkCase) {
		t.Helper()
		out, err := s.ReviseBenchmark(ctx, dispatchGateFixtureActor, task, BenchmarkRevisionInput{ExpectedRevision: plan, RequestKey: key, Cases: updated}, provider, true)
		if err != nil || out.Revision != plan+1 {
			t.Fatalf("revision %d: %+v %v", plan, out, err)
		}
	}
	cases[0].NeedsRawEvidence = true
	revise(1, "revise-held-case", cases)
	prog, err = s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil || prog.Cases[0].StepID != first || prog.Cases[0].DispatchControl == nil {
		t.Fatalf("revision lost scheduling gate: %+v %v", prog, err)
	}
	stale := dispatchGateInput(benchmarkResumeBeforeDispatch, "stale-plan-version", 1, 1)
	if _, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, stale); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("old plan released revised case: %v", err)
	}
	revise(2, "replace-held-case", cases[1:])
	revise(3, "add-back-same-key", cases)
	prog, err = s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil || prog.Cases[0].StepID == first || prog.Cases[0].DispatchControl != nil ||
		!slices.ContainsFunc(prog.Cases, func(c BenchmarkCaseProgress) bool { return c.Key == "first" }) {
		t.Fatalf("new step inherited old gate: %+v %v", prog, err)
	}
	if out, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, hold); err != nil || out.StepID != first || out.ControlVersion != 1 {
		t.Fatalf("original receipt was lost after step removal: %+v %v", out, err)
	}
}

func TestBenchmarkDispatchGateVersusClaim(t *testing.T) {
	ctx := t.Context()
	s, _, task, _, provider := newDispatchGateFixture(t, "first")
	before, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil {
		t.Fatal(err)
	}
	first := before.Cases[0].StepID
	in := dispatchGateInput(benchmarkHoldBeforeDispatch, "race-with-claim", 1, 0)
	type claimResult struct {
		work benchmarkDispatch
		err  error
	}
	begin := make(chan struct{})
	claims, controls := make(chan claimResult, 1), make(chan error, 1)
	go func() {
		<-begin
		d, _, err := s.claimBenchmark(ctx, provider, now())
		claims <- claimResult{d, err}
	}()
	go func() {
		<-begin
		_, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, in)
		controls <- err
	}()
	close(begin)
	claim, err := <-claims, <-controls
	if claim.err != nil || claim.work.action == "start" && !errors.Is(err, ErrBenchmarkDispatchNotPending) ||
		claim.work.action != "start" && err != nil {
		t.Fatalf("gate and registration are not mutually exclusive: %+v %v", claim, err)
	}
	final, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil || (final.Cases[0].DispatchControl != nil) == (final.Cases[0].Operation != nil) {
		t.Fatalf("both/neither durable paths present: %+v %v", final, err)
	}
	if final.Cases[0].Operation != nil {
		if _, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first,
			dispatchGateInput(benchmarkResumeBeforeDispatch, "resume-after-claim", 1, 0)); !errors.Is(err, ErrBenchmarkDispatchNotPending) {
			t.Fatalf("already registered operation allowed resume: %v", err)
		}
	}
}

func TestBenchmarkDispatchGateUpgradeReplaysLegacySkip(t *testing.T) {
	ctx := t.Context()
	s, path, task, _, _ := newDispatchGateFixture(t, "first", "next")
	before, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil {
		t.Fatal(err)
	}
	legacy := BenchmarkExecutionControlInput{Action: benchmarkSkipThisRun, ExpectedPlanRevision: 1,
		RequestID: "legacy-preserved-receipt", Reason: "本次跳过此前测项"}
	first, next := before.Cases[0].StepID, before.Cases[1].StepID
	receipt, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, legacy)
	if err != nil || receipt.ControlVersion != 0 {
		t.Fatalf("legacy receipt = %+v %v", receipt, err)
	}
	// Physically recreate the immediate predecessor schema, including a
	// historical receipt in its original column and digest format.
	for _, stmt := range []string{"DROP TABLE benchmark_dispatch_gates",
		"ALTER TABLE benchmark_execution_controls DROP COLUMN control_version", "PRAGMA user_version=120"} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	after, err := upgraded.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, legacy)
	if err != nil || after != receipt {
		t.Fatalf("upgrade did not preserve exact old receipt: %+v %v", after, err)
	}
	held, err := upgraded.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, next,
		dispatchGateInput(benchmarkHoldBeforeDispatch, "first-after-upgrade", 1, 0))
	if err != nil || held.ControlVersion != 1 || held.State != benchmarkDispatchHeld {
		t.Fatalf("migration did not install gate at version 0: %+v %v", held, err)
	}
}

func TestBenchmarkDispatchGateRejectsCrossStoreStaleClaim(t *testing.T) {
	ctx := t.Context()
	s, path, task, _, provider := newDispatchGateFixture(t, "first")
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	p, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil {
		t.Fatal(err)
	}
	first := p.Cases[0].StepID
	entered, release := make(chan struct{}, 1), make(chan struct{})
	releaseClaim := sync.OnceFunc(func() { close(release) })
	defer releaseClaim()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.claimBenchmark(ctx, &benchmarkClaimBarrier{entered: entered, release: release}, now())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not reach its snapshot barrier")
	}
	if _, err := second.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first,
		dispatchGateInput(benchmarkHoldBeforeDispatch, "cross-store-gate-race", 1, 0)); err != nil {
		t.Fatal(err)
	}
	releaseClaim()
	select {
	case err := <-done:
		var sqlite interface{ Code() int }
		if !errors.As(err, &sqlite) || sqlite.Code()&0xff != 5 {
			t.Fatalf("SQLite did not reject stale write snapshot: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not return")
	}
	if d, ok, err := s.claimBenchmark(ctx, provider, now()); err != nil || ok || d.operation != "" {
		t.Fatalf("fresh claim bypassed committed gate: %+v %t %v", d, ok, err)
	}
	p, err = s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
	if err != nil || p.Cases[0].DispatchControl == nil || p.Cases[0].Operation != nil ||
		p.Cases[0].Wait != nil || p.Cases[0].TargetHolds != 0 {
		t.Fatalf("gate and stale claim both persisted: %+v %v", p, err)
	}
}
