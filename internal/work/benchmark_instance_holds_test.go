package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type benchmarkInstanceHoldFixtureProvider struct{ benchFixtureProvider }

func (*benchmarkInstanceHoldFixtureProvider) benchmarkJobSource() string {
	return "physical-hold-fixture"
}

func TestBenchmarkInstanceHoldFromIndependentAccounts(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := Open(filepath.Join(dir, "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	admin, err := instance.BootstrapAccount(ctx, "alice", "Alice", "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateAccountSession(ctx, admin.PrincipalID, "fixture-hash", "fixture-session", now()+60000); err != nil {
		t.Fatal(err)
	}
	member, err := instance.CreateAccount(ctx, "fixture-session", "bob", "Bob", "fixture-member-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		store                      *Store
		provider                   *benchmarkInstanceHoldFixtureProvider
		path, principal, task, ref string
	}
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 60000}
	newFixture := func(path, principal, target, serial string) *fixture {
		t.Helper()
		f := &fixture{path: filepath.Join(dir, path), principal: principal, ref: target,
			provider: &benchmarkInstanceHoldFixtureProvider{benchFixtureProvider{loseReply: true, jobOverride: "job-" + target}}}
		f.store, err = OpenAccountBusinessStore(f.path, instance)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.store.Close() })
		if err := f.store.BindAccountBusinessStore(ctx, principal); err != nil {
			t.Fatal(err)
		}
		entries := []DomainAssetEntry{{ObjectRef: target, Kind: "node", Identity: "scope-specific:" + target, PhysicalSerial: serial, Benchmark: []DomainBenchmarkGrant{grant}}}
		if err := f.store.ImportDomainAssetsForAccount(ctx, principal, entries); err != nil {
			t.Fatal(err)
		}
		changed := append([]DomainAssetEntry{}, entries...)
		changed[0].PhysicalSerial = "OTHER-001"
		if err := f.store.ImportDomainAssetsForAccount(ctx, principal, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("physical serial mutability check: %v", err)
		}
		caseSpec := BenchmarkCase{Key: "single", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if err := f.store.PublishBenchmarkProfile(ctx, principal, BenchmarkProfile{Key: "profile", Version: 1, Cases: []BenchmarkCase{caseSpec}}); err != nil {
			t.Fatal(err)
		}
		f.task, err = f.store.SubmitBenchmarkRun(ctx, principal, BenchmarkRunRequest{RequestKey: "fixture", Profile: "profile", Version: 1}, f.provider)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := newFixture("alice.sqlite", admin.PrincipalID, "node-1", " serial-1 ")
	b := newFixture("bob.sqlite", member.PrincipalID, "node-b-alt-name", "SERIAL-1")
	// The worker entry point goes through normal operation attempts, waits,
	// transport dispatch, conservative unknown outcomes, and evidence settlement.
	type outcome struct {
		f   *fixture
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, f := range []*fixture{a, b} {
		go func() {
			<-start
			_, err := f.store.runBenchmarkOnce(ctx, f.provider)
			results <- outcome{f, err}
		}()
	}
	close(start)
	for range 2 {
		if out := <-results; out.err != nil {
			t.Fatalf("%s claim: %v", out.f.ref, out.err)
		}
	}
	if a.provider.started+b.provider.started != 1 {
		t.Fatalf("same serial across private files dispatched %d times", a.provider.started+b.provider.started)
	}
	winner, loser := a, b
	if b.provider.started == 1 {
		winner, loser = b, a
	}
	getProgress := func(f *fixture) BenchmarkCaseProgress {
		t.Helper()
		p, err := f.store.BenchmarkProgress(ctx, f.principal, f.task)
		if err != nil || len(p.Cases) != 1 {
			t.Fatalf("progress %s: %+v %v", f.ref, p, err)
		}
		return p.Cases[0]
	}
	inFlight := getProgress(winner)
	waiting := getProgress(loser)
	if inFlight.Phase != "submitting" || inFlight.Operation == nil || inFlight.TargetHolds != 1 ||
		waiting.Phase != "queued" || waiting.Operation != nil || waiting.TargetHolds != 0 {
		t.Fatalf("unexpected state after response loss: winner=%+v loser=%+v", inFlight, waiting)
	}
	bump := func(f *fixture) {
		t.Helper()
		if _, err := f.store.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", f.task); err != nil {
			t.Fatal(err)
		}
	}
	tick := func(f *fixture) {
		t.Helper()
		if _, err := f.store.runBenchmarkOnce(ctx, f.provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := winner.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Join(dir, "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := loser.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fixture{winner, loser} {
		f.store, err = OpenAccountBusinessStore(f.path, reopened)
		if err != nil {
			t.Fatal(err)
		}
	}
	bump(loser)
	tick(loser)
	if loser.provider.started != 0 || getProgress(loser).Phase != "queued" {
		t.Fatal("restart released an unresolved remote effect")
	}
	bump(winner)
	tick(winner)
	if winner.provider.started != 1 || winner.provider.looked != 1 || getProgress(winner).Phase != "following" {
		t.Fatal("recovery did not look up the original job")
	}
	bump(loser)
	tick(loser)
	if loser.provider.started != 0 {
		t.Fatal("lookup receipt incorrectly released occupancy")
	}
	bump(winner)
	tick(winner)
	terminal := getProgress(winner)
	if (terminal.Phase != "verified" && terminal.Phase != "incomplete") || terminal.TargetHolds != 0 {
		t.Fatalf("not terminal: %+v", terminal)
	}
	tick(winner)
	bump(loser)
	tick(loser)
	if loser.provider.started != 1 {
		t.Fatal("distinct account did not start after verified terminal")
	}

	// A hold committed centrally for a prepared operation that rolled back
	// before the private attempt commit did not reach Start. Recover it only
	// after reading the winning private store under its writer lock.
	next, err := winner.store.SubmitBenchmarkRun(ctx, winner.principal, BenchmarkRunRequest{RequestKey: "crash-before-private-commit", Profile: "profile", Version: 1}, winner.provider)
	if err != nil {
		t.Fatal(err)
	}
	nextProgress, err := winner.store.BenchmarkProgress(ctx, winner.principal, next)
	if err != nil || len(nextProgress.Cases) != 1 {
		t.Fatalf("prepare orphan fixture %+v: %v", nextProgress, err)
	}
	orphanStep := nextProgress.Cases[0].StepID
	if _, err := reopened.db.Exec("INSERT INTO benchmark_instance_holds VALUES(?,?,?,?)", "SERIAL-ORPHAN", winner.principal, orphanStep, "rolled-back-op"); err != nil {
		t.Fatal(err)
	}
	tick(winner)
	var orphanCount int
	if err := reopened.db.QueryRow("SELECT count(*) FROM benchmark_instance_holds WHERE physical_serial='SERIAL-ORPHAN'").Scan(&orphanCount); err != nil || orphanCount != 0 {
		t.Fatalf("orphan hold=%d err=%v", orphanCount, err)
	}
	if winner.provider.started != 1 {
		t.Fatal("orphan recovery did not honor the other account's occupancy")
	}

	// A locally authorized alias is not evidence of physical identity.
	unboundEntry := DomainAssetEntry{ObjectRef: "unbound-node", Kind: "node", Identity: "alias",
		Benchmark: []DomainBenchmarkGrant{grant}}
	if err := winner.store.ImportDomainAssetsForAccount(ctx, winner.principal, []DomainAssetEntry{unboundEntry}); err != nil {
		t.Fatal(err)
	}
	unboundCase := BenchmarkCase{Key: "unbound", Binding: binding, Targets: []string{unboundEntry.ObjectRef},
		RequiredMetrics: map[string]string{"rate": "GB/s"}}
	if err := winner.store.PublishBenchmarkProfile(ctx, winner.principal, BenchmarkProfile{Key: "unbound", Version: 1, Cases: []BenchmarkCase{unboundCase}}); err != nil {
		t.Fatal(err)
	}
	unbound, err := winner.store.SubmitBenchmarkRun(ctx, winner.principal, BenchmarkRunRequest{RequestKey: "unbound", Profile: "unbound", Version: 1}, winner.provider)
	if err != nil {
		t.Fatal(err)
	}
	tick(winner)
	unboundProgress, err := winner.store.BenchmarkProgress(ctx, winner.principal, unbound)
	if err != nil || len(unboundProgress.Cases) != 1 || unboundProgress.Cases[0].Phase != "skipped" || winner.provider.started != 1 {
		t.Fatalf("unbound alias reached provider: %+v %v", unboundProgress, err)
	}
}

func TestBenchmarkInstanceHoldIsAllOrNothing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
	defer cancel()
	if ok, err := reserveInstanceBenchmarkHolds(ctx, s.db, []string{"SERIAL-2"}, "owner", "step", "operation"); !ok || err != nil {
		t.Fatalf("initial reserve: %v %v", ok, err)
	}
	if ok, err := reserveInstanceBenchmarkHolds(ctx, s.db, []string{"SERIAL-1", "SERIAL-2"}, "other", "step", "operation"); ok || err != nil {
		t.Fatalf("conflict reserve: %v %v", ok, err)
	}
	var owner string
	if err := s.db.QueryRow("SELECT principal_id FROM benchmark_instance_holds WHERE physical_serial='SERIAL-1'").Scan(&owner); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial allocation to %q: %v", owner, err)
	}
}
