package work

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestBenchmarkComparesOnlyFrozenIdenticalConditions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "comparison.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:session"
	grant := DomainBenchmarkGrant{Connection: "bench-test", Revision: 1, Tool: "test", ExpiresAt: now() + 120000}
	if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	p := BenchmarkProfile{Key: "compare", Version: 1, Cases: []BenchmarkCase{{Key: "case", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, ActualConfig: map[string]string{"gpu_count": "40"}, ComparisonKeys: []string{"gpu_count"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	execute := func(key string, value float64) (string, benchReportJSON) {
		t.Helper()
		provider := &benchFixtureProvider{jobOverride: key, valueOverride: value}
		task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: key, Profile: p.Key, Version: p.Version}, provider)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil {
				t.Fatalf("drive %v %v", ok, err)
			}
		}
		progress, err := s.BenchmarkProgress(ctx, actor, task)
		if err != nil {
			t.Fatal(err)
		}
		var report benchReportJSON
		if err = json.Unmarshal(progress.Cases[0].Report, &report); err != nil {
			t.Fatal(err)
		}
		return task, report
	}
	previous, r1 := execute("job-1", 100)
	if r1.Comparison != nil {
		t.Fatal("invented baseline")
	}
	_, r2 := execute("job-2", 110)
	if r2.Comparison == nil || r2.Comparison.PreviousTask != previous || r2.Comparison.Delta["node-1"]["rate"].Value != 10 {
		t.Fatalf("wrong baseline %+v", r2.Comparison)
	}
	if _, err = s.db.Exec("UPDATE ops_assets SET revision=revision+1 WHERE object_ref='node-1'"); err != nil {
		t.Fatal(err)
	}
	_, r3 := execute("job-3", 120)
	if r3.Comparison != nil {
		t.Fatalf("compared changed inventory %+v", r3.Comparison)
	}
}
