package work

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
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
