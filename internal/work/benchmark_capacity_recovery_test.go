package work

import (
	"context"
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
		if _, err := s.RecoverRuntime(ctx, recoverAt); err != nil {
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

// A fake durable across Store restarts. It rejects a second Start for the
// same key or an overlapping live target, even if Go would accept the reply.
type benchmarkRestartLoadFake struct {
	benchFixtureProvider
	byKey     map[string]string
	live      map[string]string
	finished  map[string]bool
	lostKey   string
	delay     bool
	starts    int
	lookups   int
	reads     int
	violation string
}

func (p *benchmarkRestartLoadFake) Start(_ context.Context, r BenchmarkJobRequest) (string, error) {
	if p.byKey[r.IdempotencyKey] != "" {
		p.violation = "duplicate Start: " + r.IdempotencyKey
		return "", errors.New(p.violation)
	}
	for _, target := range r.Targets {
		if p.live[target] != "" {
			p.violation = "overlapping remote work on " + target
			return "", errors.New(p.violation)
		}
	}
	p.starts++
	job := fmt.Sprintf("load-job-%d", p.starts)
	p.byKey[r.IdempotencyKey] = job
	for _, target := range r.Targets {
		p.live[target] = r.IdempotencyKey
	}
	if p.lostKey == "" {
		p.lostKey = r.IdempotencyKey
		return "", errors.New("fixture lost reply after durable remote admission")
	}
	return job, nil
}

func (p *benchmarkRestartLoadFake) Lookup(_ context.Context, r BenchmarkJobRequest) (string, error) {
	p.lookups++
	if job := p.byKey[r.IdempotencyKey]; job != "" {
		return job, nil
	}
	return "", errors.New("lookup did not reuse the original key")
}

func (p *benchmarkRestartLoadFake) Get(_ context.Context, r BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	p.reads++
	if job == "" || p.byKey[r.IdempotencyKey] != job {
		return BenchmarkJobResult{}, errors.New("query did not use the original external identity")
	}
	if r.IdempotencyKey == p.lostKey && p.delay {
		return BenchmarkJobResult{JobID: job, State: "running"}, nil
	}
	var measurements []BenchmarkMeasurement
	for _, target := range r.Targets {
		if p.live[target] != r.IdempotencyKey {
			return BenchmarkJobResult{}, errors.New("query for a job that no longer owns its target")
		}
		delete(p.live, target)
		measurements = append(measurements, BenchmarkMeasurement{Target: target, Outcome: "measured",
			Metrics: map[string]BenchmarkMeasuredMetric{"rate": {Value: 100, Unit: "GB/s"}}})
	}
	p.finished[r.IdempotencyKey] = true
	return BenchmarkJobResult{JobID: job, State: "completed", Measurements: measurements}, nil
}

func TestBenchmarkProductionTicksDrainSaturatedBatchesAfterRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "benchmark-drain.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	p := &benchmarkRestartLoadFake{byKey: map[string]string{}, live: map[string]string{}, finished: map[string]bool{}, delay: true}
	stamp := now()
	const actor = "user:session"
	const shared, independent = "test-shared", "test-independent"
	bind := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: bind.Revision, Connection: bind.Connection, Tool: bind.Tool, ExpiresAt: stamp + 900000}
	for _, ref := range []string{shared, independent} {
		if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: ref, Kind: "node", Identity: ref,
			Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
			t.Fatal(err)
		}
		c := BenchmarkCase{Key: "initial", Binding: bind, Targets: []string{ref}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		c2 := c
		c2.Key, c2.DependsOn = "after-initial", []string{c.Key}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: ref, Version: 1, Cases: []BenchmarkCase{c, c2}}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(key, ref string) {
		t.Helper()
		if _, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{key, ref, 1}, p); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() bool {
		t.Helper()
		worked, err := s.runBenchmarkOnce(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return worked
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	enqueue("first-in-flight", shared)
	if !tick() || p.starts != 1 || p.live[shared] != p.lostKey {
		t.Fatal("first request was not accepted by the fake with a lost reply")
	}
	const sharedBacklog, independentTasks = 6, 3
	for i := 0; i < sharedBacklog; i++ {
		enqueue(fmt.Sprintf("shared-queue-%d", i), shared)
	}
	for i := 0; i < independentTasks; i++ {
		enqueue(fmt.Sprintf("independent-queue-%d", i), independent)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverRuntime(ctx, now()); err != nil {
		t.Fatal(err)
	}
	firstStep := p.lostKey[len("benchmark:"):]
	// Trigger the in-flight retry at its logical due point without sleeping.
	exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp-1, firstStep)
	if !tick() || p.lookups != 1 || p.starts != 1 {
		t.Fatalf("restart did not only reconcile the original job: starts=%d lookups=%d", p.starts, p.lookups)
	}
	exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp-1, firstStep)
	if !tick() || p.reads != 1 || p.finished[p.lostKey] {
		t.Fatal("fake's nonterminal observation did not keep the original job pending")
	}
	exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", now()+60000, firstStep)
	// Distinguish due timestamps of the older overlap backlog even if
	// sequential admission happened within the same millisecond.
	exec(`UPDATE benchmark_cases SET next_at=? WHERE phase='queued' AND case_key='initial'
 AND task_id IN (SELECT task_id FROM benchmark_runs WHERE profile_key=?)`, stamp-1, shared)

	if !tick() {
		t.Fatal("overlapping backpressure must still leave work progressed")
	}
	if n := count(`SELECT count(*) FROM benchmark_cases c JOIN benchmark_runs r ON r.task_id=c.task_id
WHERE r.profile_key=? AND c.phase='queued' AND c.next_at>?`, shared, stamp); n != sharedBacklog {
		t.Fatalf("expected %d overlapping cases deferred, got %d", sharedBacklog, n)
	}
	if p.live[independent] == "" || count("SELECT count(*) FROM benchmark_target_holds") != 2 ||
		count("SELECT count(*) FROM benchmark_target_holds WHERE step_id=? AND target_ref=?", firstStep, shared) != 1 {
		t.Fatal("independent work stalled or shared hold released before terminal proof")
	}
	// Process terminal data from both scopes, then keep advancing only
	// persisted retry deadlines until the finite queue has drained.
	p.delay = false
	total := 2 * (1 + sharedBacklog + independentTasks)
	const maxTicks = 100
	var ticks int
	for ; ticks < maxTicks && len(p.finished) < total; ticks++ {
		exec("UPDATE benchmark_cases SET next_at=? WHERE next_at>? AND phase IN ('queued','following','submitting')", now(), now())
		tick()
	}
	if len(p.finished) != total || p.starts != total || p.lookups != 1 || len(p.live) != 0 {
		t.Fatalf("finite queue failed to drain in %d ticks: starts=%d lookups=%d completed=%d live=%v", ticks, p.starts, p.lookups, len(p.finished), p.live)
	}
	if p.violation != "" {
		t.Fatal(p.violation)
	}
	if holds := count("SELECT count(*) FROM benchmark_target_holds"); holds != 0 {
		t.Fatalf("terminal cleanup leaked %d holds", holds)
	}
	if n := count("SELECT count(*) FROM benchmark_cases WHERE phase='verified'"); n != total ||
		count("SELECT count(*) FROM operations WHERE state='succeeded' AND attempt_count=1") != total ||
		count("SELECT count(*) FROM task_states WHERE state='succeeded'") != total/2 {
		t.Fatalf("durable task/case/attempt results inconsistent: verified=%d expected=%d", n, total)
	}
	if tick() {
		t.Fatal("empty queue must not dispatch again")
	}
}
