package work

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestBenchmarkCatalogVersionAndBinding(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	c := BenchmarkCase{Key: "test", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	p := BenchmarkProfile{Key: "a", Version: 1, Cases: []BenchmarkCase{c}}
	if err := s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	p.Version = 2
	p.Cases[0].Targets = []string{"node-2"}
	if err := s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	p.Key, p.Version = "b", 1
	if err := s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListBenchmarkProfiles(ctx, actor, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0] != (BenchmarkProfileItem{"a", 2}) || page.NextAfter != "a" {
		t.Fatalf("unexpected catalog: %+v %v", page, err)
	}
	page, err = s.ListBenchmarkProfiles(ctx, actor, page.NextAfter, 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != "b" || page.NextAfter != "" {
		t.Fatalf("unexpected next page: %+v %v", page, err)
	}
	versions, err := s.ListBenchmarkVersions(ctx, actor, "a", 0, 1)
	if err != nil || len(versions.Items) != 1 || versions.Items[0].Version != 2 || versions.NextBefore != 2 {
		t.Fatalf("unexpected versions %+v %v", versions, err)
	}
	versions, err = s.ListBenchmarkVersions(ctx, actor, "a", versions.NextBefore, 1)
	if err != nil || len(versions.Items) != 1 || versions.Items[0].Version != 1 || versions.NextBefore != 0 {
		t.Fatalf("unexpected next version %+v %v", versions, err)
	}
	details, err := s.ReadBenchmarkProfileDetails(ctx, actor, "a", 2, 1)
	if err != nil || details.Changes == nil || len(details.Changes.Changed) != 1 || details.Changes.Changed[0] != "test" {
		t.Fatalf("unexpected delta: %+v %v", details, err)
	}
	bound, err := s.ReadBenchmarkChatTarget(ctx, actor, "pod7")
	if err != nil || bound != (BenchmarkProfileRef{Profile: "a", Version: 1}) {
		t.Fatalf("unexpected binding: %+v %v", bound, err)
	}
	if _, err := s.ListBenchmarkProfiles(ctx, "", "", 1); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("no auth: %v", err)
	}
}

func TestBenchmarkPreflightNoProviderCallOrReservation(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "preflight.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	c := BenchmarkCase{Key: "run", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "plan", Version: 1, Cases: []BenchmarkCase{c}}); err != nil {
		t.Fatal(err)
	}
	ref := BenchmarkProfileRef{Profile: "plan", Version: 1}
	r, err := s.PreflightBenchmark(ctx, actor, ref, nil)
	if err != nil || r.CanQueue || r.FacilityStatus != "not_verified" || len(r.Cases) != 1 || len(r.Cases[0].Blockers) != 3 {
		t.Fatalf("missing blockers: %+v %v", r, err)
	}
	node := DomainAssetEntry{ObjectRef: "node-1", Kind: "node", Identity: "serial-1", Benchmark: []DomainBenchmarkGrant{{
		Connection: "bench-test", Revision: 1, Tool: "test", ExpiresAt: now() + 120000,
	}}}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{node}); err != nil {
		t.Fatal(err)
	}
	provider := &benchFixtureProvider{}
	r, err = s.PreflightBenchmark(ctx, actor, ref, provider)
	if err != nil || !r.CanQueue || r.FacilityStatus != "not_verified" || len(r.Cases[0].Warnings) != 2 ||
		r.Cases[0].Targets[0].Occupancy != "no_local_benchmark_hold" {
		t.Fatalf("incorrect readiness: %+v %v", r, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&count); err != nil || count != 0 ||
		provider.started != 0 || provider.looked != 0 || provider.read != 0 {
		t.Fatalf("preflight changed state or called external provider: %d %+v %v", count, provider, err)
	}
}
