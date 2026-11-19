package work

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestBenchmarkCycleScansBusyQueueWithinBudget(t *testing.T) {
	for _, n := range []int{3, benchmarkScanBudget + 1} {
		t.Run(fmt.Sprintf("%d-busy-cases", n), func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(filepath.Join(t.TempDir(), "benchmark-scan.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p := &benchFixtureProvider{}
			const actor = "user:session"
			binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
			grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 60000}
			for _, target := range []string{"busy", "free-1", "free-2"} {
				if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: target, Kind: "node", Identity: target, Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
					t.Fatal(err)
				}
				spec := BenchmarkCase{Key: "case", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
				if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: target, Version: 1, Cases: []BenchmarkCase{spec}}); err != nil {
					t.Fatal(err)
				}
				count := 1
				if target == "busy" {
					count = n
				}
				for i := 0; i < count; i++ {
					task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{fmt.Sprintf("%s-%d", target, i), target, 1}, p)
					if err != nil {
						t.Fatal(err)
					}
					at := int64(1)
					if target != "busy" {
						at = 2
						if target == "free-2" {
							at = 3 // Free targets still follow persistent queue order.
						}
					}
					if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=? WHERE task_id=?", at, task); err != nil {
						t.Fatal(err)
					}
				}
			}
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := holdTarget(tx, "busy", "local-worker", "other-task", now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			tick := func() {
				t.Helper()
				if worked, err := s.runBenchmarkOnce(ctx, p); !worked || err != nil {
					t.Fatalf("tick worked=%t err=%v", worked, err)
				}
			}
			tick()
			if n >= benchmarkScanBudget {
				if p.started != 0 || p.looked != 0 || p.read != 0 {
					t.Fatalf("scan beyond budget: %+v", p)
				}
				var remaining int
				err := s.db.QueryRow(`SELECT count(*) FROM benchmark_cases c JOIN benchmark_runs r ON r.task_id=c.task_id
WHERE r.profile_key='busy' AND c.next_at=1`).Scan(&remaining)
				if err != nil || remaining != n-benchmarkScanBudget {
					t.Fatalf("scan budget not enforced: remaining=%d err=%v", remaining, err)
				}
				tick()
			}
			if p.started != 1 || p.looked != 0 || p.read != 0 {
				t.Fatalf("disjoint target didn't advance in available tick, or too many remote calls: %+v", p)
			}
			var target string
			var occupied int
			if err := s.db.QueryRow("SELECT count(*),coalesce(min(target_ref),'') FROM benchmark_target_holds").Scan(&occupied, &target); err != nil ||
				occupied != 1 || target != "free-1" {
				t.Fatalf("wrong queue order or unsafe acquisition: %d %s %v", occupied, target, err)
			}
		})
	}
}

func TestBenchmarkDueReconciliationIsNotStarvedByNewAdmissions(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-reconcile-order.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &benchFixtureProvider{loseReply: true}
	const actor, oldTarget, freshTarget = "user:session", "node-1", "node-2"
	binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 60000}
	for _, target := range []string{oldTarget, freshTarget} {
		if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: target, Kind: "node", Identity: target, Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
			t.Fatal(err)
		}
		spec := BenchmarkCase{Key: "case", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: target, Version: 1, Cases: []BenchmarkCase{spec}}); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(key, profile string) string {
		t.Helper()
		task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{key, profile, 1}, p)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	tick := func() {
		t.Helper()
		if worked, err := s.runBenchmarkOnce(ctx, p); !worked || err != nil {
			t.Fatalf("benchmark tick: worked=%t err=%v", worked, err)
		}
	}
	// Create an actual unknown Start receipt, not a fabricated operation.
	old := admit("old-operation", oldTarget)
	tick()
	if p.started != 1 || p.looked != 0 {
		t.Fatalf("missing original attempt: %+v", p)
	}
	p.loseReply = false
	mockDue := func(at int64) {
		t.Helper()
		// Move the persisted retry clock without waiting through real backoff.
		if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=? WHERE task_id=?", at, old); err != nil {
			t.Fatal(err)
		}
	}
	mockDue(now() + 10000)
	if worked, err := s.runBenchmarkOnce(ctx, p); worked || err != nil {
		t.Fatalf("not-yet-due operation was selected: %t %v", worked, err)
	}
	for i := 0; i < 2; i++ {
		mockDue(now() - 1000)
		admit(fmt.Sprintf("new-operation-%d", i), freshTarget)
		tick()
		if p.started != 1 || p.looked != 1 || p.read != i {
			t.Fatalf("old operation starved or restarted at tick %d: starts=%d lookup=%d read=%d", i, p.started, p.looked, p.read)
		}
	}
	admit("new-operation-2", freshTarget)
	tick()
	if p.started != 2 || p.looked != 1 || p.read != 1 {
		t.Fatalf("new work did not proceed: starts=%d lookup=%d read=%d", p.started, p.looked, p.read)
	}
	var held int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_target_holds WHERE target_ref=?", oldTarget).Scan(&held); err != nil || held != 0 {
		t.Fatalf("old reconciled target still held: %d %v", held, err)
	}
}
