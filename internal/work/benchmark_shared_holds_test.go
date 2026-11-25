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

func TestLoanAndBenchmarkUseSameTargetOwnership(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "loan-benchmark.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	const actor = "user:session"
	b := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	p := &benchFixtureProvider{}
	stamp := now()
	for _, node := range []string{"node-held", "node-disjoint"} {
		grant := DomainBenchmarkGrant{Revision: 1, Connection: b.Connection, Tool: b.Tool, ExpiresAt: stamp + 900000}
		asset := DomainAssetEntry{ObjectRef: node, Kind: "node", Identity: node, Benchmark: []DomainBenchmarkGrant{grant}}
		if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{asset}); err != nil {
			t.Fatal(err)
		}
		c := BenchmarkCase{Key: "case", Binding: b, Targets: []string{node}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: node, Version: 1, Cases: []BenchmarkCase{c}}); err != nil {
			t.Fatal(err)
		}
	}
	queue := func(node string) string {
		t.Helper()
		task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: node, Profile: node, Version: 1}, p)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	lend := func(node string) (Loan, error) {
		return s.LendAsset(ctx, node, actor, "borrower", stamp+900000, "")
	}
	queue("node-held")
	stamp = now()
	d, worked, err := s.claimBenchmark(ctx, p, stamp)
	if err != nil || !worked || d.action != "start" {
		t.Fatalf("could not claim target: %+v %t %v", d, worked, err)
	}
	if _, err := lend("node-held"); !errors.Is(err, ErrConflict) {
		t.Fatalf("dispatched target was lendable: %v", err)
	}
	disjointTask := queue("node-disjoint")
	if loan, err := lend("node-disjoint"); err != nil || loan.State != "active" {
		t.Fatalf("disjoint loan blocked by active benchmark: %+v %v", loan, err)
	}
	skipped, worked, err := s.claimBenchmark(ctx, p, now())
	if err != nil || !worked || skipped.task != disjointTask || skipped.action != "" {
		t.Fatalf("queued benchmark dispatched against now-lent asset: %+v %t %v", skipped, worked, err)
	}
	var phase string
	if err := s.db.QueryRow("SELECT phase FROM benchmark_cases WHERE task_id=?", disjointTask).Scan(&phase); err != nil || phase != "skipped" {
		t.Fatalf("disjoint queued case should be stopped by changed asset epoch: %s %v", phase, err)
	}
	if err := s.settleBenchmark(ctx, d, "", BenchmarkJobResult{}, errors.New("lost response"), stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	if _, err := s.RecoverRuntime(ctx, stamp+benchmarkWaitRecheckMS+benchmarkClaimMS); err != nil {
		t.Fatal(err)
	}
	if _, err := lend("node-held"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown benchmark after restart did not block loan: %v", err)
	}
	asset, err := readAsset(s.db, "node-held")
	if err != nil || asset.FrozenAt != 0 || asset.State == AssetStateLent {
		t.Fatalf("rejected loan mutated asset: %+v %v", asset, err)
	}
	// Reproduce the historical overlap (before the production lend guard)
	// using the old unchecked handover path, without affecting a real asset.
	legacy, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Rollback()
	h, err := initiateLoanHandover(legacy, LoanHandover{ObjectRef: "node-held", Intent: "lend"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	loan, err := confirmLoanHandover(legacy, h.HandoverID, actor, "borrower", stamp+900000, "", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Commit(); err != nil {
		t.Fatal(err)
	}
	verified := ReturnVerification{IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: true, OperationsReconciled: true, ResponsibilityTransferred: true}
	if err := s.ReturnAsset(ctx, "node-held", verified); !errors.Is(err, ErrConflict) {
		t.Fatalf("attested return bypassed an unresolved operation: %v", err)
	}
	var loanState string
	if err := s.db.QueryRow("SELECT state FROM loans WHERE loan_id=?", loan.LoanID).Scan(&loanState); err != nil || loanState != "active" {
		t.Fatalf("conflicting return changed loan: %s %v", loanState, err)
	}
	if err := s.ReturnAsset(ctx, "node-disjoint", verified); err != nil {
		t.Fatalf("verified disjoint return: %v", err)
	}
	returned, err := readAsset(s.db, "node-disjoint")
	if err != nil || returned.State != AssetStateRecheck || returned.FrozenAt != 0 || returned.FreezeReason != "" {
		t.Fatalf("verified return retained its stale loan freeze or did not require recheck: %+v %v", returned, err)
	}
	if _, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: "after-return", Profile: "node-disjoint", Version: 1}, p); err != nil {
		t.Fatalf("verified return did not permit fresh scoped precheck and enqueue: %v", err)
	}

	if p.started != 0 || p.looked != 0 || p.read != 0 {
		t.Fatalf("unexpected provider calls: %+v", p)
	}
}

func TestReturnPreservesIndependentAssetFreeze(t *testing.T) {
	s, _ := setupReturnsStore(t, "independent-return-freeze.db")
	asset, err := readAsset(s.db, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE ops_assets SET frozen_by='admin',freeze_reason='external review' WHERE object_ref='node-1'"); err != nil {
		t.Fatal(err)
	}
	v := ReturnVerification{IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: true, OperationsReconciled: true, ResponsibilityTransferred: true}
	if err := s.ReturnAsset(t.Context(), "node-1", v); err != nil {
		t.Fatal(err)
	}
	fresh, err := readAsset(s.db, "node-1")
	if err != nil || fresh.State != AssetStateRecheck || fresh.FrozenAt != asset.FrozenAt || fresh.FrozenBy != "admin" || fresh.FreezeReason != "external review" || fresh.OwnershipEpoch != asset.OwnershipEpoch+1 {
		t.Fatalf("verified return overwrote another freeze or reused authority: %+v %v", fresh, err)
	}
}
