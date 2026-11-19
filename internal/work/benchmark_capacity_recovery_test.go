package work

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBenchmarkConcurrentCapacityAndDurableHoldRecovery(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "capacity-recovery.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	const actor = "user:session"
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: now() + 900000}
	bind := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	p := &benchFixtureProvider{} // Supports only; no remote jobs are started.
	profile := func(name, node string) {
		t.Helper()
		asset := DomainAssetEntry{ObjectRef: node, Kind: "node", Identity: node, Benchmark: []DomainBenchmarkGrant{grant}}
		if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{asset}); err != nil {
			t.Fatal(err)
		}
		spec := BenchmarkCase{Key: "case", Binding: bind, Targets: []string{node}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: name, Version: 1, Cases: []BenchmarkCase{spec}}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(name string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{fmt.Sprintf("%s-%d", name, i), name, 1}, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	profile("shared", "node-shared")
	profile("disjoint", "node-disjoint")
	profile("generic", "node-generic")
	enqueue("shared", 24)
	enqueue("disjoint", 8)
	enqueue("generic", 1)
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := holdTarget(tx, "node-generic", "local-worker", "generic-task", stamp); err != nil {
		t.Fatal(err)
	}
	// A stale scheduling/expiry hint is not proof of external settlement.
	if _, err := tx.Exec("UPDATE quota_reservations SET expires_at=? WHERE target_ref='node-generic'", stamp+1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		d      benchmarkDispatch
		worked bool
		err    error
	}
	const attempts = 33
	channel := make(chan claimResult, attempts)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < attempts; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			d, worked, err := s.claimBenchmark(ctx, p, stamp)
			channel <- claimResult{d, worked, err}
		}()
	}
	close(start)
	workers.Wait()
	close(channel)

	owners := map[string]benchmarkDispatch{}
	deferred := 0
	for result := range channel {
		if result.err != nil || !result.worked {
			t.Fatalf("unexpected claim result: %+v", result)
		}
		d := result.d
		switch d.action {
		case "":
			deferred++
		case "start":
			target := d.request.Targets[0]
			if _, occupied := owners[target]; occupied {
				t.Fatalf("same target double admitted: %s", target)
			}
			owners[target] = d
		default:
			t.Fatalf("unexpected first action: %s", d.action)
		}
	}
	if len(owners) != 2 || owners["node-shared"].operation == "" || owners["node-disjoint"].operation == "" || deferred != attempts-2 {
		t.Fatalf("unfair or excess dispatch: %v deferred=%d", owners, deferred)
	}
	shared := owners["node-shared"]
	if err := s.settleBenchmark(ctx, shared, "", BenchmarkJobResult{}, errors.New("lost reply"), stamp); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recoverAt := stamp + benchmarkWaitRecheckMS + benchmarkClaimMS
	for i := 0; i < 2; i++ {
		if _, err := s.RecoverV3(ctx, recoverAt); err != nil {
			t.Fatal(err)
		}
		if err := s.applyBenchmarkTimeouts(ctx, recoverAt); err != nil {
			t.Fatal(err)
		}
	}

	for target, original := range owners {
		var step, state, wait string
		err := s.db.QueryRow(`SELECT h.step_id,o.state,w.state FROM benchmark_target_holds h
 JOIN benchmark_cases c ON c.step_id=h.step_id JOIN operations o ON o.operation_id=c.operation_id
 JOIN waits w ON w.wait_id='benchmark:'||h.step_id WHERE h.target_ref=?`, target).Scan(&step, &state, &wait)
		if err != nil || step != original.step || wait != "open" || (state != "reconcile" && state != "dispatched") {
			t.Fatalf("hold released or terminal invented for %s: %s %s %s %v", target, step, state, wait, err)
		}
	}
	for _, target := range []string{"node-generic", "node-shared", "node-disjoint"} {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, collision := holdTarget(tx, target, "other-worker", "other-task", recoverAt)
		tx.Rollback()
		if !errors.Is(collision, ErrQuotaExhausted) {
			t.Fatalf("recovery released %s without settlement: %v", target, collision)
		}
	}
	var nextQueried bool
	for i := 0; i < attempts; i++ {
		d, worked, err := s.claimBenchmark(ctx, p, recoverAt)
		if err != nil || !worked || d.action == "start" {
			t.Fatalf("recovery dispatch can only defer or query original: %+v %t %v", d, worked, err)
		}
		if d.task == shared.task {
			nextQueried = d.action == "lookup" && d.operation == shared.operation && d.request.IdempotencyKey == shared.request.IdempotencyKey
			break
		}
	}
	if !nextQueried {
		t.Fatal("original job was not reclaimed via the same identity after lease expiry")
	}
	if p.started != 0 || p.looked != 0 || p.read != 0 {
		t.Fatalf("test must not execute provider actions: %+v", p)
	}
}

// This fake pauses after the claim transaction has read its candidate but
// before it takes ownership. It never executes a provider operation.
type benchmarkClaimBarrier struct {
	benchFixtureProvider
	entered chan<- struct{}
	release <-chan struct{}
}

func (p *benchmarkClaimBarrier) Supports(binding BenchmarkBinding) bool {
	p.entered <- struct{}{}
	<-p.release
	return p.benchFixtureProvider.Supports(binding)
}

func TestBenchmarkMultiStoreSameSnapshotDoesNotDoubleDispatch(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "shared-connection.sqlite")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const actor = "user:session"
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 60000}
	fixture := &benchFixtureProvider{}
	for i, target := range []string{"node-shared", "node-free"} {
		if err := a.ImportDomainAssets(ctx, []DomainAssetEntry{
			{ObjectRef: target, Kind: "node", Identity: target, Benchmark: []DomainBenchmarkGrant{grant}},
		}); err != nil {
			t.Fatal(err)
		}
		spec := BenchmarkCase{Key: "case", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if err := a.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: target, Version: 1, Cases: []BenchmarkCase{spec}}); err != nil {
			t.Fatal(err)
		}
		count := 1
		if i == 0 {
			count = 2
		}
		for j := 0; j < count; j++ {
			task, err := a.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{fmt.Sprintf("%s-%d", target, j), target, 1}, fixture)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec("UPDATE benchmark_cases SET next_at=? WHERE task_id=?", i+1, task); err != nil {
				t.Fatal(err)
			}
		}
	}
	type result struct {
		s   *Store
		d   benchmarkDispatch
		ok  bool
		err error
	}
	entered, release, outcomes := make(chan struct{}, 2), make(chan struct{}), make(chan result, 2)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	stamp := now()
	for _, store := range []*Store{a, b} {
		go func() {
			d, ok, err := store.claimBenchmark(ctx, &benchmarkClaimBarrier{entered: entered, release: release}, stamp)
			outcomes <- result{store, d, ok, err}
		}()
	}
	deadline := time.After(5 * time.Second)
	for range 2 {
		select {
		case <-entered:
		case <-deadline:
			t.Fatal("both stores must read the same unclaimed SQLite snapshot")
		}
	}
	releaseOnce()
	var winner, loser result
	for range 2 {
		got := <-outcomes
		if got.err == nil {
			if winner.s != nil || !got.ok || got.d.action != "start" || got.d.request.Targets[0] != "node-shared" {
				t.Fatalf("more than one winner or out-of-order dispatch: %+v", got)
			}
			winner = got
		} else {
			// SQLite rejects promotion of the other reader's stale snapshot.
			// No provider operation was returned from the failed transaction.
			var sqlite interface{ Code() int }
			if !errors.As(got.err, &sqlite) || sqlite.Code()&0xff != 5 || loser.s != nil {
				t.Fatalf("expected one SQLITE_BUSY-family contention rejection: %+v", got)
			}
			loser = got
		}
	}
	if winner.s == nil || loser.s == nil {
		t.Fatal("expected exactly one durable winner and one blocked writer")
	}
	var holds, ops int
	if err := loser.s.db.QueryRow("SELECT (SELECT count(*) FROM benchmark_target_holds),(SELECT count(*) FROM operations)").Scan(&holds, &ops); err != nil ||
		holds != 1 || ops != 1 {
		t.Fatalf("other connection observed duplicate/partial transaction: holds=%d ops=%d err=%v", holds, ops, err)
	}
	d, ok, err := loser.s.claimBenchmark(ctx, fixture, stamp)
	if err != nil || !ok || !d.deferredForHold || d.action != "" {
		t.Fatalf("fresh snapshot must defer same target after winner: %+v %t %v", d, ok, err)
	}
	d, ok, err = loser.s.claimBenchmark(ctx, fixture, stamp)
	if err != nil || !ok || d.action != "start" || d.operation == winner.d.operation || d.request.Targets[0] != "node-free" {
		t.Fatalf("contended connection failed to move disjoint target: %+v %t %v", d, ok, err)
	}
	if fixture.started != 0 || fixture.looked != 0 || fixture.read != 0 {
		t.Fatal("the test must not run provider actions")
	}
}
