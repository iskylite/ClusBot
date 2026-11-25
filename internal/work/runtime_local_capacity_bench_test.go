package work

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

// BenchmarkRuntimeLocalMixedCapacity measures a bounded, reproducible sample
// through production SQLite loops, not the real external provider, maximum
// capacity, or a group-fairness guarantee. Run with -benchtime=1x.
func BenchmarkRuntimeLocalMixedCapacity(b *testing.B) {
	if b.N != 1 {
		b.Skip("fixed-size sample; use -benchtime=1x and -count")
	}
	b.StopTimer()
	ctx, cancel := context.WithTimeout(b.Context(), 27*time.Second)
	defer cancel()
	must := func(err error) {
		b.Helper()
		if err != nil {
			b.Fatal(err)
		}
	}
	f, err := os.CreateTemp("", "clusbot-runtime-capacity-*.sqlite")
	must(err)
	path := f.Name()
	b.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				b.Errorf("remove sampled database: %v", err)
			}
		}
	})
	must(f.Close())
	s, err := Open(path)
	must(err)
	defer func() { s.Close() }()
	const actor, benchAsset = "user:session", "capacity-benchmark"
	const perGroup = 32
	groups := []string{"capacity-read-a", "capacity-read-b"}
	for _, ref := range groups {
		must(s.SeedAssetForTest(ref, AssetStateInService))
	}
	p := &benchmarkRestartLoadFake{byKey: map[string]string{}, live: map[string]string{}, finished: map[string]bool{}}
	bind := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: bind.Connection, Tool: bind.Tool, Revision: bind.Revision, ExpiresAt: now() + 600000}
	must(s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: benchAsset, Kind: "node", Identity: benchAsset, Benchmark: []DomainBenchmarkGrant{grant}}}))
	must(s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "capacity-profile", Version: 1, Cases: []BenchmarkCase{{
		Key: "measure", Binding: bind, Targets: []string{benchAsset}, RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}))
	groupLoops := func(fs ...func(context.Context)) func() {
		loopCtx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, f := range fs {
			wg.Go(func() { f(loopCtx) })
		}
		return func() { stop(); wg.Wait() }
	}
	wait := func(name string, cond func() (bool, error)) {
		b.Helper()
		const cadence = 20 * time.Millisecond
		poll := time.NewTicker(cadence)
		defer poll.Stop()
		for {
			ok, err := cond()
			must(err)
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				b.Fatalf("%s: %v", name, ctx.Err())
			case <-poll.C:
			}
		}
	}
	b.StartTimer()
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"capacity-bench", "capacity-profile", 1}, p)
	must(err)
	initial := s
	stop := groupLoops(func(c context.Context) { initial.RunBenchmarks(c, p) })
	defer stop()
	var originalOp string
	wait("first provider response lost", func() (bool, error) {
		var state string
		err := initial.db.QueryRowContext(ctx, `SELECT coalesce(o.operation_id,''),coalesce(o.state,'')
 FROM benchmark_cases c LEFT JOIN operations o ON o.operation_id=c.operation_id WHERE c.task_id=?`, task).
			Scan(&originalOp, &state)
		return state == string(RuntimeOperationReconcile), err
	})
	unknownObserved := time.Now()
	stop()
	if p.starts != 1 || p.live[benchAsset] != p.lostKey || originalOp == "" {
		b.Fatal("did not retain a remote fixture job after losing its response")
	}
	intakeStart := time.Now()
	for i := range perGroup {
		for _, ref := range groups {
			_, err := initial.SubmitAssetAcceptanceQuery(ctx, fmt.Sprintf("%s-%d", ref, i), ref, actor)
			must(err)
		}
	}
	intakeDone := time.Now()
	restartStart := time.Now()
	must(initial.Close())
	reopened, err := Open(path)
	must(err)
	s = reopened
	_, err = s.RecoverRuntime(ctx, now())
	must(err)
	executor, err := s.EnableAssetAcceptanceQuery(ctx)
	must(err)
	runtime := NewRuntimeLoop(s, func() bool { return false })
	restartDone := time.Now()
	stopAfterRestart := groupLoops(
		func(c context.Context) { runtime.Run(c, 250*time.Millisecond, 0) },
		func(c context.Context) { executor.Run(c, 100*time.Millisecond) },
		func(c context.Context) { reopened.RunBenchmarks(c, p) },
	)
	defer stopAfterRestart()
	wait("applied read outcomes", func() (bool, error) {
		var completed int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM result_receipts r
 JOIN work_units w ON w.unit_id=r.unit_id AND w.lease_generation=r.lease_generation
 JOIN domain_query_tasks d ON d.task_id=w.task_id WHERE r.state='applied'`).Scan(&completed)
		return completed == perGroup*len(groups), err
	})
	wait("verified original benchmark", func() (bool, error) {
		var state string
		err := s.db.QueryRowContext(ctx, `SELECT phase FROM benchmark_cases WHERE task_id=?`, task).Scan(&state)
		return state == "verified", err
	})
	verifiedObserved := time.Now()
	// Sample live runtime health before deliberate cancellation: an in-flight
	// tick can count canceled DB work as failed while shutting down.
	rounds, failures := runtime.Stats()
	stopAfterRestart()
	b.StopTimer()
	if rounds == 0 || failures != 0 {
		b.Fatalf("runtime ticks: rounds=%d failures=%d", rounds, failures)
	}
	if p.starts != 1 || p.lookups != 1 || p.reads != 1 || !p.finished[p.lostKey] || len(p.live) != 0 || p.violation != "" {
		b.Fatalf("operation recovery did not use reselection by original key only: %+v", p)
	}
	var op string
	var terminalAt int64
	var holds, attempts int
	must(s.db.QueryRow(`SELECT o.operation_id,o.updated_at,o.attempt_count,
 (SELECT count(*) FROM benchmark_target_holds WHERE step_id=c.step_id) FROM benchmark_cases c
 JOIN operations o ON o.operation_id=c.operation_id WHERE c.task_id=? AND o.state='succeeded'`, task).
		Scan(&op, &terminalAt, &attempts, &holds))
	if op != originalOp || holds != 0 || attempts != 1 {
		b.Fatalf("original operation receipt/hold mismatch: %s attempts=%d holds=%d", op, attempts, holds)
	}

	rows, err := s.db.Query(`SELECT d.object_ref,w.ready_at,l.issued_at,r.applied_at
 FROM domain_query_tasks d JOIN work_units w ON w.task_id=d.task_id
 JOIN work_leases l ON w.unit_id=l.unit_id AND w.lease_generation=l.lease_generation
 JOIN result_receipts r ON w.unit_id=r.unit_id AND w.lease_generation=r.lease_generation
 JOIN steps st ON st.step_id=w.step_id WHERE r.state='applied' AND st.state='completed'
 ORDER BY r.applied_at,r.rowid`)
	must(err)
	defer rows.Close()
	queue, finish := map[string][]int64{}, map[string][]int64{}
	firstHalf := map[string]int{}
	var finished int
	var minReady, lastFinish int64
	for rows.Next() {
		var group string
		var ready, claim, completed int64
		must(rows.Scan(&group, &ready, &claim, &completed))
		if ready > claim || claim > completed {
			b.Fatalf("invalid persisted timing %s ready=%d claim=%d completed=%d", group, ready, claim, completed)
		}
		if finished < perGroup {
			firstHalf[group]++
		}
		finished++
		queue[group] = append(queue[group], claim-ready)
		finish[group] = append(finish[group], completed-ready)
		if minReady == 0 || ready < minReady {
			minReady = ready
		}
		lastFinish = completed
	}
	must(rows.Err())
	if finished != len(groups)*perGroup || lastFinish <= minReady || lastFinish >= terminalAt {
		b.Fatalf("expected all %d local reads to finish during unresolved remote recovery: reads=%d earliest-ready=%d last-read=%d remote-end=%d",
			len(groups)*perGroup, finished, minReady, lastFinish, terminalAt)
	}
	for i, ref := range groups {
		if len(queue[ref]) != perGroup || firstHalf[ref] == 0 {
			b.Fatalf("sample group %s has %d reads, %d in first half", ref, len(queue[ref]), firstHalf[ref])
		}
		g := string('A' + rune(i))
		for _, label := range []string{"ready-to-claim", "ready-to-complete"} {
			x := queue[ref]
			if label == "ready-to-complete" {
				x = finish[ref]
			}
			sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
			b.ReportMetric(float64(x[(50*len(x)+99)/100-1]), g+"-"+label+"-p50-ms")
			b.ReportMetric(float64(x[(95*len(x)+99)/100-1]), g+"-"+label+"-p95-ms")
			b.ReportMetric(float64(x[len(x)-1]), g+"-"+label+"-max-ms")
		}
		b.ReportMetric(float64(firstHalf[ref]), g+"-first-half-reads")
	}
	b.ReportMetric(float64(finished), "local-reads")
	b.ReportMetric(float64(finished)*1000/float64(lastFinish-minReady), "local-reads/sec")
	b.ReportMetric(float64(intakeDone.Sub(intakeStart).Milliseconds()), "read-intake-ms")
	b.ReportMetric(float64(restartDone.Sub(restartStart).Milliseconds()), "reopen-recover-register-ms")
	b.ReportMetric(float64(verifiedObserved.Sub(restartStart).Milliseconds()), "reopen-through-verified-ms")
	b.ReportMetric(float64(verifiedObserved.Sub(unknownObserved).Milliseconds()), "unknown-through-verified-ms")
}
