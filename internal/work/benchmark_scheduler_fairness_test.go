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
