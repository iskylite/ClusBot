package work

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestBenchmarkSharedTargetHolds(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "shared-holds.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: now() + 120000}
	binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	p := &benchFixtureProvider{loseReply: true}
	add := func(name, node string) string {
		t.Helper()
		if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: node, Kind: "node", Identity: node, Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: name, Version: 1, Cases: []BenchmarkCase{{Key: "one", Binding: binding, Targets: []string{node}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}); err != nil {
			t.Fatal(err)
		}
		task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{name, name, 1}, p)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	same := add("same", "node-1")
	disjoint := add("disjoint", "node-2")
	update := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := holdTarget(tx, "node-1", "generic", same, now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	pre, err := s.PreflightBenchmark(ctx, actor, BenchmarkProfileRef{"same", 1}, p)
	if err != nil || !pre.CanQueue || pre.Cases[0].Targets[0].Occupancy != "held" {
		t.Fatalf("preflight must show local conflict without forbidding queue: %+v %v", pre, err)
	}
	held, err := s.BenchmarkProgress(ctx, actor, same)
	if err != nil || len(held.Cases) != 1 || held.Cases[0].TargetHolds != 0 || len(held.Cases[0].Targets) != 1 || held.Cases[0].Targets[0].LocalHold != "held_elsewhere" {
		t.Fatalf("foreign generic holder not distinguished from this case: %+v %v", held, err)
	}

	stamp := now()
	update("UPDATE benchmark_cases SET next_at=1 WHERE task_id=?", same)
	update("UPDATE benchmark_cases SET next_at=2 WHERE task_id=?", disjoint)
	d, worked, err := s.claimBenchmark(ctx, p, stamp)
	if err != nil || !worked || d.task != same || d.action != "" {
		t.Fatalf("held target must not be dispatched: %+v %t %v", d, worked, err)
	}
	d, worked, err = s.claimBenchmark(ctx, p, stamp)
	if err != nil || !worked || d.task != disjoint || d.action != "start" {
		t.Fatalf("other target must still proceed: %+v %t %v", d, worked, err)
	}
	tx2, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if _, err := holdTarget(tx2, "node-2", "generic", same, stamp); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("generic target acquisition ignored benchmark hold: %v", err)
	}
	if err := releaseTargetHold(tx2, "node-1", false, stamp); err == nil {
		t.Fatal("released a target of unknown effect")
	}
	if err := releaseTargetHold(tx2, "node-1", true, stamp); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	update("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", same)
	d, worked, err = s.claimBenchmark(ctx, p, stamp)
	if err != nil || !worked || d.task != same || d.action != "start" {
		t.Fatalf("verified release should allow new dispatch: %+v %t %v", d, worked, err)
	}
}
