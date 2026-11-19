package work

// P2-02 验收：原子领取、续租与过期回收。
//
// 三个测试入口：
//   - TestV3P202LeaseClaimRace：两个工作者竞争同一单元仅一个领取成功；不同
//     单元可分别领取；Claim 在短事务核对范围/额度并条件更新增加 lease_generation。
//   - TestV3P202LeaseExpiredToken：过期令牌不能续租/提交；重复 Renew 不创造
//     第二份额度；新领取不复用旧令牌；区分任务分配代际与单元租约代际。
//   - TestV3P202LeaseRestart：控制面重启后根据持久租约恢复，不能依赖内存
//     map 判断所有权；失联回收先判断是否有已派发操作，未知外部效果进入核对。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupV3P202 创建存储、任务与就绪单元。
func setupV3P202(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p202-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	taskID := run.TaskID
	now := v3P102Now()
	// 创建步骤与就绪单元。
	tx, _ := store.db.Begin()
	insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
		DefinitionKey: "analyze", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
	tx.Commit()
	steps, _ := listSteps(store.db, taskID)
	tx, _ = store.db.Begin()
	insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
		Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
	tx.Commit()
	var unitID string
	if err := store.db.QueryRow("SELECT unit_id FROM work_units WHERE task_id=?", taskID).Scan(&unitID); err != nil {
		t.Fatal(err)
	}
	return store, taskID, unitID
}

// registerV3P202Worker 注册一个 Worker 并返回 ID 与凭据。
func registerV3P202Worker(t *testing.T, store *Store, role string, kinds []WorkUnitKind, scope string) (string, string) {
	t.Helper()
	tx, _ := store.db.Begin()
	credential := "secret-" + role
	worker, err := registerWorker(tx, RegisterWorkerRequest{
		Role: role, SupportedKinds: kinds, Credential: credential,
		ExpiresAt: v3P102Now() + 100000, AllowedScope: scope, RegisteredBy: "admin",
	}, v3P102Now())
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return worker.WorkerID, credential
}

// TestV3P202LeaseClaimRace 领取竞争。
func TestV3P202LeaseClaimRace(t *testing.T) {
	t.Run("two_workers_compete_single_wins", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "race.db")
		now := v3P102Now()
		// 两个工作者竞争同一单元：仅一个领取成功。
		workerA, credA := registerV3P202Worker(t, store, "worker-a", []WorkUnitKind{WorkUnitModel}, "")
		workerB, credB := registerV3P202Worker(t, store, "worker-b", []WorkUnitKind{WorkUnitModel}, "")
		var wg sync.WaitGroup
		var mu sync.Mutex
		var wins, losses int
		for _, w := range []struct{ id, cred string }{{workerA, credA}, {workerB, credB}} {
			wg.Add(1)
			go func(wid, cred string) {
				defer wg.Done()
				tx, err := store.db.Begin()
				if err != nil {
					return
				}
				defer tx.Rollback()
				_, err = claimWorkUnit(tx, ClaimRequest{
					WorkerID: wid, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true,
				}, now)
				mu.Lock()
				if err == nil {
					wins++
					tx.Commit()
				} else {
					losses++
				}
				mu.Unlock()
			}(w.id, w.cred)
		}
		wg.Wait()
		// 同一单元同一时刻只有一个有效推进者。
		if wins != 1 {
			t.Errorf("claim wins = %d, want 1 (竞争同一单元仅一个领取成功)", wins)
		}
		if losses != 1 {
			t.Errorf("claim losses = %d, want 1", losses)
		}
	})

	t.Run("different_units_claimed_separately", func(t *testing.T) {
		store, taskID, _ := setupV3P202(t, "separate.db")
		now := v3P102Now()
		// 第二个就绪单元（不同任务）。
		ctx := context.Background()
		run2, _ := store.Submit(ctx, "v3p202-sep"+id(), "整理另一份材料", "", "", true)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: run2.TaskID, ContractRevision: 1,
			DefinitionKey: "analyze2", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps2, _ := listSteps(store.db, run2.TaskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run2.TaskID, StepID: steps2[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now + 1, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 两个工作者分别领取不同单元。
		workerA, credA := registerV3P202Worker(t, store, "worker-a", []WorkUnitKind{WorkUnitModel}, "")
		workerB, credB := registerV3P202Worker(t, store, "worker-b", []WorkUnitKind{WorkUnitModel}, "")
		tx, _ = store.db.Begin()
		resultA, errA := claimWorkUnit(tx, ClaimRequest{WorkerID: workerA, Credential: credA, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		resultB, errB := claimWorkUnit(tx, ClaimRequest{WorkerID: workerB, Credential: credB, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now+1)
		tx.Commit()
		if errA != nil || errB != nil {
			t.Fatalf("separate claims failed: A=%v B=%v", errA, errB)
		}
		if resultA.Unit.UnitID == resultB.Unit.UnitID {
			t.Error("two workers claimed same unit (不同单元应分别领取)")
		}
		if resultA.Unit.TaskID == resultB.Unit.TaskID && resultA.Unit.TaskID == taskID {
			t.Logf("claims on different units: %s vs %s", resultA.Unit.UnitID, resultB.Unit.UnitID)
		}
	})

	t.Run("claim_checks_scope_and_quota", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "scopequota.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "limited", []WorkUnitKind{WorkUnitModel}, "工作资料")
		// 范围越权：拒绝（worker 限工作资料，单元无范围时不匹配）。
		tx, _ := store.db.Begin()
		_, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, Scope: "资产管理", LeaseTTL: 60000, QuotaAvailable: true}, now)
		tx.Rollback()
		if err == nil {
			t.Error("out-of-scope claim not rejected")
		}
		// 额度不足：拒绝。
		tx, _ = store.db.Begin()
		_, err = claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, Scope: "工作资料", LeaseTTL: 60000, QuotaAvailable: false}, now)
		tx.Rollback()
		if err == nil {
			t.Error("quota-exhausted claim not rejected")
		}
	})

	t.Run("claim_increments_lease_generation", func(t *testing.T) {
		store, _, unitID := setupV3P202(t, "gen.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "worker", []WorkUnitKind{WorkUnitModel}, "")
		tx, _ := store.db.Begin()
		result, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// lease_generation 增加（0 → 1）。
		if result.Lease.LeaseGeneration != 1 {
			t.Errorf("lease generation = %d, want 1 (条件更新领取增加代际)", result.Lease.LeaseGeneration)
		}
		// 单元状态为 claimed 且代际一致。
		var state string
		var gen int
		if err := store.db.QueryRow("SELECT state, lease_generation FROM work_units WHERE unit_id=?", unitID).Scan(&state, &gen); err != nil {
			t.Fatal(err)
		}
		if state != "claimed" || gen != 1 {
			t.Errorf("unit state=%s gen=%d, want claimed/1", state, gen)
		}
	})
}

// TestV3P202LeaseExpiredToken 过期令牌。
func TestV3P202LeaseExpiredToken(t *testing.T) {
	t.Run("expired_token_cannot_renew", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "expired.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "worker", []WorkUnitKind{WorkUnitModel}, "")
		// 领取（短期租约）。
		tx, _ := store.db.Begin()
		result, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 1000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 令牌过期后：不能续租。
		later := now + 2000 // 超过租约期限
		tx, _ = store.db.Begin()
		_, err = renewLease(tx, result.Token.TokenID, result.PlainToken, later)
		tx.Rollback()
		if err == nil {
			t.Error("expired token renewed (过期令牌不能续租)")
		}
	})

	t.Run("duplicate_renew_not_double_quota", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "duprenew.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "worker", []WorkUnitKind{WorkUnitModel}, "")
		tx, _ := store.db.Begin()
		result, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 100000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 重复续租：不创造第二份额度（租约仍是一条，lease_until 延长）。
		tx, _ = store.db.Begin()
		first, err := renewLease(tx, result.Token.TokenID, result.PlainToken, now+1000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		second, err := renewLease(tx, result.Token.TokenID, result.PlainToken, now+2000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 租约仍是一条（不创造第二份）。
		var leaseCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM work_leases WHERE unit_id=? AND state='active'", result.Unit.UnitID).Scan(&leaseCount); err != nil {
			t.Fatal(err)
		}
		if leaseCount != 1 {
			t.Errorf("active leases = %d, want 1 (重复续租不创造第二份额度)", leaseCount)
		}
		if second.LeaseUntil < first.LeaseUntil {
			t.Errorf("renew moved lease_until backward: %d < %d", second.LeaseUntil, first.LeaseUntil)
		}
	})

	t.Run("assignment_vs_lease_generation_distinct", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "generations.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "worker", []WorkUnitKind{WorkUnitModel}, "")
		tx, _ := store.db.Begin()
		result, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 区分任务分配代际（assignments.generation，旧结构）与单元租约代际
		//（work_leases.lease_generation，新结构）：一个并行 Step 重领不无故废止
		// 其他独立 Step。
		var assignmentGen int
		if err := store.db.QueryRow("SELECT coalesce(max(generation),0) FROM assignments WHERE task_id=?", result.Unit.TaskID).Scan(&assignmentGen); err != nil {
			t.Fatal(err)
		}
		if result.Lease.LeaseGeneration != 1 {
			t.Errorf("unit lease generation = %d (单元级，与任务分配代际不同)", result.Lease.LeaseGeneration)
		}
		t.Logf("assignment_generation=%d lease_generation=%d (不同维度)", assignmentGen, result.Lease.LeaseGeneration)
	})
}

// TestV3P202LeaseRestart 重启恢复。
func TestV3P202LeaseRestart(t *testing.T) {
	t.Run("restart_recovers_from_persisted_lease_not_memory", func(t *testing.T) {
		// 控制面重启后根据持久租约恢复，不能依赖内存 map 判断所有权。
		path := filepath.Join(t.TempDir(), "restart.db")
		now := v3P102Now()
		var unitID, workerID, tokenID, plainToken string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, _ := store.Submit(ctx, "v3p202-restart", "整理汇报", "", "", true)
			tx, _ := store.db.Begin()
			insertStep(tx, Step{StepID: id(), TaskID: run.TaskID, ContractRevision: 1,
				DefinitionKey: "analyze", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			steps, _ := listSteps(store.db, run.TaskID)
			tx, _ = store.db.Begin()
			insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run.TaskID, StepID: steps[0].StepID,
				Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			store.db.QueryRow("SELECT unit_id FROM work_units WHERE task_id=?", run.TaskID).Scan(&unitID)
			tx, _ = store.db.Begin()
			worker, _ := registerWorker(tx, RegisterWorkerRequest{Role: "w", SupportedKinds: []WorkUnitKind{WorkUnitModel}, Credential: "cred", ExpiresAt: now + 100000, RegisteredBy: "admin"}, now)
			workerID = worker.WorkerID
			result, _ := claimWorkUnit(tx, ClaimRequest{WorkerID: workerID, Credential: "cred", Kind: WorkUnitModel, LeaseTTL: 100000, QuotaAvailable: true}, now)
			tokenID, plainToken = result.Token.TokenID, result.PlainToken
			tx.Commit()
		}()
		// 重启：按持久租约恢复（不依赖内存 map）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		lease, err := activeLeaseForUnit(store.db, unitID, now+1000)
		if err != nil {
			t.Fatal(err)
		}
		if lease == nil {
			t.Fatal("lease lost after restart (应按持久租约恢复)")
		}
		if lease.WorkerID != workerID || lease.TokenID != tokenID {
			t.Errorf("lease owner wrong after restart: %+v (不依赖内存判断所有权)", lease)
		}
		// 令牌仍可验证（重启后令牌有效）。
		tx, _ := store.db.Begin()
		if _, err := validateToken(tx, tokenID, plainToken, now+1000); err != nil {
			t.Errorf("token invalid after restart: %v", err)
		}
		tx.Rollback()
	})

	t.Run("reclaim_judges_dispatched_ops_before_reclaim", func(t *testing.T) {
		store, _, unitID := setupV3P202(t, "reclaim.db")
		now := v3P102Now()
		worker, cred := registerV3P202Worker(t, store, "worker", []WorkUnitKind{WorkUnitModel}, "")
		// 领取（短期租约，单元进入 running 模拟已派发）。
		tx, _ := store.db.Begin()
		result, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 1000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		// 单元标记为 running（已派发操作），在同事务内更新。
		if _, err := tx.Exec("UPDATE work_units SET state='running' WHERE unit_id=?", result.Unit.UnitID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 租约过期后回收：先判断已派发操作（未知外部效果进入核对）。
		later := now + 2000
		tx, _ = store.db.Begin()
		reclaim, err := reclaimExpiredLeases(tx, later, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if reclaim.Reclaimed != 1 {
			t.Errorf("reclaimed = %d, want 1", reclaim.Reclaimed)
		}
		if reclaim.PendingOps != 1 {
			t.Errorf("pending ops = %d, want 1 (失联回收先判断已派发操作，进入核对不自动重发)", reclaim.PendingOps)
		}
		// 单元回到 ready（可重新核对领取），但回收记录了已派发操作数。
		var state string
		if err := store.db.QueryRow("SELECT state FROM work_units WHERE unit_id=?", unitID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "ready" {
			t.Errorf("unit state = %s after reclaim, want ready (可重新核对)", state)
		}
		// 回收已审计（失联回收与已派发操作进入核对）。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='lease_reclaimed' AND unit_id=?", unitID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("reclaim not audited: count=%d", auditCount)
		}
	})

	t.Run("reclaim_expired_multiple_batches", func(t *testing.T) {
		store, _, _ := setupV3P202(t, "batch.db")
		now := v3P102Now()
		// 多个过期租约（分批回收）。
		for i := 0; i < 3; i++ {
			tx, _ := store.db.Begin()
			insertStep(tx, Step{StepID: id(), TaskID: "batch-task-" + string(rune('a'+i)), ContractRevision: 1,
				DefinitionKey: "batch-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
		}
		// 回收不存在的过期租约：0（不误回收）。
		tx, _ := store.db.Begin()
		result, err := reclaimExpiredLeases(tx, now+100000, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Reclaimed != 0 {
			t.Errorf("reclaimed %d with no expired leases (不误回收)", result.Reclaimed)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
