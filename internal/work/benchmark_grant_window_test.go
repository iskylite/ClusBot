package work

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestImportedBenchmarkGrantEnforcesWindowWithoutScheduling(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-grant-window.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, profile = "user:session", "time-window"
	binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	start := now() + 60000
	end := start + 60000
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ValidFrom: &start, ExpiresAt: end}
	entry := DomainAssetEntry{ObjectRef: "node-1", Kind: "node", Identity: "fixtures-only", Benchmark: []DomainBenchmarkGrant{grant}}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{entry}); err != nil {
		t.Fatal(err)
	}
	measurement := BenchmarkCase{Key: "probe", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: profile, Version: 1, Cases: []BenchmarkCase{measurement}}); err != nil {
		t.Fatal(err)
	}
	p := &benchFixtureProvider{}
	ref := BenchmarkProfileRef{Profile: profile, Version: 1}
	if out, err := s.PreflightBenchmark(ctx, actor, ref, p); err != nil || out.CanQueue || out.Cases[0].Targets[0].Authorization != "denied" {
		t.Fatalf("future grant preflight: %+v %v", out, err)
	}
	if _, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"no-future-scheduling", profile, 1}, p); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("future grant allowed early submission: %v", err)
	}
	var runs int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&runs); err != nil || runs != 0 || p.started != 0 {
		t.Fatalf("future grant created work: runs=%d starts=%d err=%v", runs, p.started, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct {
		stamp   int64
		allowed bool
	}{{start - 1, false}, {start, true}, {end - 1, true}, {end, false}} {
		auth, err := benchmarkAuthorization(tx, actor, "node-1", measurement, probe.stamp)
		if probe.allowed && (err != nil || auth == "") || !probe.allowed && !errors.Is(err, ErrAuthorizationDenied) {
			tx.Rollback()
			t.Fatalf("at %d expected allowed=%t, got grant=%q %v", probe.stamp, probe.allowed, auth, err)
		}
	}
	tx.Rollback()

	// These are the exact production transaction and scheduler paths,
	// driven by explicit timestamps instead of real sleeps.
	admitAt := func(name string, stamp int64) string {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		task, err := submitBenchmarkRunTx(ctx, tx, actor, BenchmarkRunRequest{name, profile, 1}, p, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return task
	}
	inFlight := admitAt("within-window", start)
	queued := admitAt("waiting-past-window", start+1)
	first, ok, err := s.claimBenchmark(ctx, p, start)
	if err != nil || !ok || first.task != inFlight || first.action != "start" {
		t.Fatalf("window start claim: %+v %t %v", first, ok, err)
	}
	job, err := p.Start(ctx, first.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.settleBenchmark(ctx, first, job, BenchmarkJobResult{}, nil, start); err != nil {
		t.Fatal(err)
	}
	old, err := s.BenchmarkProgress(ctx, actor, inFlight)
	if err != nil || old.Cases[0].TargetHolds != 1 || old.Cases[0].Wait.State != "open" {
		t.Fatalf("in-flight hold: %+v %v", old, err)
	}
	second, ok, err := s.claimBenchmark(ctx, p, end)
	if err != nil || !ok || second.task != inFlight || second.action != "get" || second.operation != first.operation || second.job != job {
		t.Fatalf("expired grant prevented original reconciliation: %+v %t %v", second, ok, err)
	}
	result, err := p.Get(ctx, second.request, second.job)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.settleBenchmark(ctx, second, "", result, nil, end); err != nil {
		t.Fatal(err)
	}
	third, ok, err := s.claimBenchmark(ctx, p, end)
	if err != nil || !ok || third.task != queued || third.action != "" {
		t.Fatalf("expired grant dispatched new work: %+v %t %v", third, ok, err)
	}
	stale, err := s.BenchmarkProgress(ctx, actor, queued)
	if err != nil || stale.State != "failed" || stale.Cases[0].Phase != "skipped" || stale.Cases[0].Operation != nil || p.started != 1 {
		t.Fatalf("expired queued case: %+v starts=%d %v", stale, p.started, err)
	}
	complete, err := s.BenchmarkProgress(ctx, actor, inFlight)
	if err != nil || complete.State != "succeeded" || complete.Cases[0].Wait.State != "closed" ||
		complete.Cases[0].TargetHolds != 0 || p.read != 1 {
		t.Fatalf("original job outcome: %+v reads=%d %v", complete, p.read, err)
	}
}

func TestImportedBenchmarkGrantWindowIdentityAndLegacyDedupe(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-grant-dedupe.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start1, start2, expiry := now()+10000, now()+20000, now()+100000
	base := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: expiry}
	importGrant := func(grant DomainBenchmarkGrant) error {
		return s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixtures-only", Benchmark: []DomainBenchmarkGrant{grant}}})
	}
	for _, start := range []int64{-1, 0, expiry, expiry + 1} {
		g := base
		g.ValidFrom = &start
		if err := importGrant(g); !errors.Is(err, ErrBenchmarkInput) {
			t.Fatalf("invalid start %d: %v", start, err)
		}
	}
	for _, start := range []*int64{&start1, &start1, &start2, nil, nil} {
		g := base
		g.ValidFrom = start
		if err := importGrant(g); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.Query("SELECT valid_from,created_at,revoked_at FROM authorizations WHERE action='benchmark.execute' ORDER BY valid_from")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var begins []int64
	for rows.Next() {
		var start, created, revoked int64
		if err := rows.Scan(&start, &created, &revoked); err != nil {
			t.Fatal(err)
		}
		if len(begins) == 0 && start != created || revoked != 0 {
			t.Fatalf("unexpected replacement or legacy begin time: %d %d %d", start, created, revoked)
		}
		begins = append(begins, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(begins) != 3 || begins[1] != start1 || begins[2] != start2 {
		t.Fatalf("explicit windows and omitted legacy dedupe: %v", begins)
	}
}
