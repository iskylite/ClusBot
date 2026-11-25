package work

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

type mixedLoadBenchmark struct {
	*benchmarkRestartLoadFake
	lookupEntered chan string
	lookupRelease chan struct{}
}

func (p *mixedLoadBenchmark) Lookup(ctx context.Context, r BenchmarkJobRequest) (string, error) {
	select {
	case p.lookupEntered <- r.IdempotencyKey:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-p.lookupRelease:
		return p.benchmarkRestartLoadFake.Lookup(ctx, r)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type mixedLoadDiscovery struct{ discoveryFixture }

func (mixedLoadDiscovery) ScanDiscovery(context.Context, string, string) ([]DiscoveryObservation, error) {
	return []DiscoveryObservation{{Serial: "mixed-scan-serial", Address: "192.0.2.44"}}, nil
}

// The production entry point starts separate loops over one Store. Drive their
// real one-tick paths with an explicit provider barrier, rather than assuming
// ticker timing or claiming that these workloads share a single queue.
func TestRuntimeMixedLoadProgressWhileRecoveryLookupIsBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f, err := os.CreateTemp("", "clusbot-mixed-load-*.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test database: %v", err)
			}
		}
	})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	must(err)
	t.Cleanup(func() { s.Close() })
	p := &mixedLoadBenchmark{
		benchmarkRestartLoadFake: &benchmarkRestartLoadFake{
			byKey: map[string]string{}, live: map[string]string{}, finished: map[string]bool{}, delay: true,
		},
		lookupEntered: make(chan string), lookupRelease: make(chan struct{}),
	}
	source := mixedLoadDiscovery{}
	const actor, benchAsset, readAsset = "user:session", "mixed-benchmark-node", "mixed-read-node"
	bind := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: bind.Connection, Tool: bind.Tool, Revision: bind.Revision, ExpiresAt: now() + 600000}
	must(s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: benchAsset, Kind: "node", Identity: benchAsset, Benchmark: []DomainBenchmarkGrant{grant}}}))
	must(s.SeedAssetForTest(readAsset, AssetStateInService))
	must(s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "mixed-profile", Version: 1, Cases: []BenchmarkCase{{
		Key: "measure", Binding: bind, Targets: []string{benchAsset}, RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}))
	enqueue := func(key string) {
		t.Helper()
		_, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{key, "mixed-profile", 1}, p)
		must(err)
	}
	tick := func() {
		t.Helper()
		worked, err := s.runBenchmarkOnce(ctx, p)
		must(err)
		if !worked {
			t.Fatal("expected a benchmark action")
		}
	}
	enqueue("original")
	tick()
	if p.starts != 1 || p.lostKey == "" || p.live[benchAsset] != p.lostKey {
		t.Fatal("fixture did not lose the reply after accepting the original action")
	}
	enqueue("same-asset-waiter")
	query, err := s.SubmitAssetAcceptanceQuery(ctx, "mixed-read", readAsset, actor)
	must(err)
	due := now() + 2000
	_, err = s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: "inventory-1", Scope: "POD7", ExpiresAt: due + 600000}, source)
	must(err)
	must(s.PublishDiscoveryPlan(ctx, actor, DiscoveryPlanRequest{
		ID: "mixed-discovery", Source: "inventory-1", Scope: "POD7", PeriodSeconds: 60, FirstAt: due,
	}, source))
	firstStep := p.lostKey[len("benchmark:"):]
	var originalOperation string
	must(s.db.QueryRowContext(ctx, "SELECT operation_id FROM benchmark_cases WHERE step_id=?", firstStep).Scan(&originalOperation))
	must(s.Close())
	reopened, err := Open(path)
	must(err)
	s = reopened
	_, err = s.RecoverRuntime(ctx, now())
	must(err)
	executor, err := s.EnableAssetAcceptanceQuery(ctx)
	must(err)
	loop := NewRuntimeLoop(s, func() bool { return false })

	setDue := func(step string, timestamp int64) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, "UPDATE benchmark_cases SET next_at=? WHERE step_id=?", timestamp, step)
		must(err)
	}
	var waiter string
	must(s.db.QueryRowContext(ctx, "SELECT step_id FROM benchmark_cases WHERE step_id<>?", firstStep).Scan(&waiter))
	// Force the persisted retry due while keeping the other case queued.
	setDue(firstStep, now()-1)
	setDue(waiter, now()+60000)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	type tickResult struct {
		worked bool
		err    error
	}
	recoveryDone := make(chan tickResult, 1)
	wg.Go(func() {
		worked, err := s.runBenchmarkOnce(ctx, p)
		recoveryDone <- tickResult{worked, err}
	})
	select {
	case key := <-p.lookupEntered:
		if key != p.lostKey {
			t.Fatalf("restart lookup key %q, want %q", key, p.lostKey)
		}
	case result := <-recoveryDone:
		t.Fatalf("restart finished without querying the original job: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	holdCount := func() int {
		t.Helper()
		var n int
		must(s.db.QueryRowContext(ctx, "SELECT count(*) FROM benchmark_target_holds WHERE step_id=? AND target_ref=?", firstStep, benchAsset).Scan(&n))
		return n
	}
	scanDone := make(chan tickResult, 1)
	readDone := make(chan tickResult, 1)
	runtimeDone := make(chan struct{})
	wg.Go(func() {
		worked, err := s.runDiscoveryOnce(ctx, source, due)
		scanDone <- tickResult{worked, err}
	})
	wg.Go(func() {
		worked, err := executor.RunOnce(ctx)
		readDone <- tickResult{worked, err}
	})
	wg.Go(func() {
		loop.tick(ctx)
		close(runtimeDone)
	})
	for _, step := range []struct {
		name string
		done <-chan tickResult
	}{{"discovery", scanDone}, {"read-only work", readDone}} {
		select {
		case result := <-step.done:
			if !result.worked || result.err != nil {
				t.Fatalf("%s did not progress while lookup blocked: %+v", step.name, result)
			}
		case result := <-recoveryDone:
			t.Fatalf("lookup ended before %s progressed: %+v", step.name, result)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case <-runtimeDone:
		if rounds, failures := loop.Stats(); rounds != 1 || failures != 0 {
			t.Fatalf("production runtime tick: rounds=%d failures=%d", rounds, failures)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	progress, err := s.ReadAssetAcceptanceQuery(ctx, query.TaskID, actor)
	must(err)
	if !progress.Done || progress.StepState != string(RuntimeStepCompleted) ||
		progress.Result == nil || progress.Result.ObjectRef != readAsset {
		t.Fatalf("read-only work did not commit its result: %+v", progress)
	}
	items, err := s.ListDiscoveryCandidates(ctx, actor)
	must(err)
	if len(items) != 1 || items[0].Serial != "mixed-scan-serial" || holdCount() != 1 {
		t.Fatalf("discovery did not commit independently or unknown benchmark lost its hold: %+v", items)
	}
	var state string
	var attempts int
	must(s.db.QueryRowContext(ctx, "SELECT state,attempt_count FROM operations WHERE operation_id=?", originalOperation).Scan(&state, &attempts))
	if state != string(RuntimeOperationReconcile) || attempts != 1 {
		t.Fatalf("blocked lookup lost the original reconciliation: state=%s attempts=%d", state, attempts)
	}
	select {
	case result := <-recoveryDone:
		t.Fatalf("lookup unexpectedly finished before the barrier opened: %+v", result)
	default:
	}
	close(p.lookupRelease)
	select {
	case result := <-recoveryDone:
		must(result.err)
		if !result.worked {
			t.Fatal("lookup did not settle")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	setDue(firstStep, now()-1)
	tick() // A nonterminal provider response must not release the hold.
	setDue(waiter, now()-1)
	setDue(firstStep, now()+60000)
	tick() // A competing case on the same asset is deferred.
	if p.starts != 1 || p.lookups != 1 || p.reads != 1 || holdCount() != 1 {
		t.Fatalf("unknown/nonterminal action was duplicated or hold released: starts=%d lookups=%d reads=%d", p.starts, p.lookups, p.reads)
	}
	p.delay = false
	setDue(firstStep, now()-1)
	tick()
	if !p.finished[p.lostKey] || holdCount() != 0 {
		t.Fatal("terminal evidence did not release the original hold")
	}
	setDue(waiter, now()-1)
	tick()
	setDue(waiter, now()-1)
	tick()
	if p.starts != 2 || p.lookups != 1 || len(p.finished) != 2 || len(p.live) != 0 || p.violation != "" {
		t.Fatalf("same-asset waiter did not complete exactly once: %+v", p.benchmarkRestartLoadFake)
	}
	must(s.db.QueryRowContext(ctx, "SELECT state,attempt_count FROM operations WHERE operation_id=?", originalOperation).Scan(&state, &attempts))
	if state != string(RuntimeOperationSucceeded) || attempts != 1 {
		t.Fatalf("terminal receipt did not settle the original operation: state=%s attempts=%d", state, attempts)
	}
	worked, err := s.runDiscoveryOnce(ctx, source, due)
	must(err)
	if worked {
		t.Fatal("same discovery occurrence ran again")
	}
}
