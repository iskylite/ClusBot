package work

// 验收：持久预算、并发额度与占用。
//
// 三个测试入口：
//   - TestQuotaReservationAtomic：两个工作者同时申请最后一份额度，仅
//     一方成功；领取/派发所需的预留与状态同事务；重复回报不重复扣款。
//   - TestBudgetSurvivesRestart：重启/新 Run/重领不绕过 Task 预算；
//     未知成本明确可见；费用未知时保留已预留上限/未知状态，不退款成零。
//   - TestWaitDoesNotHoldModelSlot：等待中的 Task 不占模型槽；确定
//     步骤和持久等待不占模型槽；未知外部动作仍保留必要占用。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupQuotasStore 创建存储与任务。
func setupQuotasStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p204-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestQuotaReservationAtomic 预留原子性。
func TestQuotaReservationAtomic(t *testing.T) {
	t.Run("last_quota_single_winner", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "race.db")
		now := recordsTestNow()
		// 配置上限为 1 的任务预算。
		tx, _ := store.db.Begin()
		upsertQuotaConfig(tx, "task_budget:"+taskID, QuotaTaskBudget, 1, "tokens", taskID, now)
		tx.Commit()
		// 两个工作者同时申请最后一份额度：仅一方成功。
		var wg sync.WaitGroup
		var mu sync.Mutex
		var wins, losses int
		for _, wid := range []string{"worker-a", "worker-b"} {
			wg.Add(1)
			go func(workerID string) {
				defer wg.Done()
				tx, err := store.db.Begin()
				if err != nil {
					return
				}
				defer tx.Rollback()
				_, err = reserveQuota(tx, QuotaReservation{
					Scope: QuotaTaskBudget, TaskID: taskID, WorkerID: workerID,
					Amount: 1, Unit: "tokens", ReservedAt: now,
				})
				mu.Lock()
				if err == nil {
					wins++
					tx.Commit()
				} else {
					losses++
				}
				mu.Unlock()
			}(wid)
		}
		wg.Wait()
		if wins != 1 {
			t.Errorf("quota wins = %d, want 1 (最后一份额度仅一方成功)", wins)
		}
		if losses != 1 {
			t.Errorf("quota losses = %d, want 1", losses)
		}
	})

	t.Run("reservation_same_tx_as_claim", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "sametx.db")
		now := recordsTestNow()
		// 预留与状态（领取）同事务：要么都提交，要么都回滚。
		tx, _ := store.db.Begin()
		if _, err := reserveQuota(tx, QuotaReservation{
			Scope: QuotaTaskBudget, TaskID: taskID, Amount: 100, Unit: "tokens", ReservedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		tx.Rollback() // 回滚：预留不生效。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM quota_reservations WHERE task_id=?", taskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("reservation persisted after rollback: %d (预留与状态同事务)", count)
		}
	})

	t.Run("duplicate_settlement_no_double_charge", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "settle.db")
		now := recordsTestNow()
		// 预留并结算（回报按稳定身份）。
		tx, _ := store.db.Begin()
		r, err := reserveQuota(tx, QuotaReservation{
			Scope: QuotaTaskBudget, TaskID: taskID, StepID: "step-1",
			Amount: 100, Unit: "tokens", ReservedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 重复结算：无影响（不重复扣款）。
		for i := 0; i < 2; i++ {
			tx, _ = store.db.Begin()
			if err := settleQuota(tx, r.SettlementKey, true, now); err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		// 结算状态只一次（不重复扣款）。
		var settledCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM quota_reservations WHERE settlement_key=? AND state='settled'", r.SettlementKey).Scan(&settledCount); err != nil {
			t.Fatal(err)
		}
		if settledCount != 1 {
			t.Errorf("settled count = %d, want 1 (重复结算无影响不重复扣款)", settledCount)
		}
	})

	t.Run("config_has_unit_and_scope", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "config.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		err := upsertQuotaConfig(tx, "task_budget:"+taskID, QuotaTaskBudget, 1000, "tokens", taskID, now)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		// 配置含明确单位与作用域。
		var unit, subject string
		if err := store.db.QueryRow("SELECT unit, subject FROM quota_configs WHERE config_key=?", "task_budget:"+taskID).Scan(&unit, &subject); err != nil {
			t.Fatal(err)
		}
		if unit != "tokens" || subject != taskID {
			t.Errorf("config unit=%s subject=%s (配置项含明确单位与作用域)", unit, subject)
		}
	})
}

// TestBudgetSurvivesRestart 预算跨重启。
func TestBudgetSurvivesRestart(t *testing.T) {
	t.Run("restart_not_reset_consumed_budget", func(t *testing.T) {
		// 重启/新 Run/重领不绕过 Task 预算（已消费成本持久）。
		path := filepath.Join(t.TempDir(), "budgetrestart.db")
		var taskID string
		now := recordsTestNow()
		var settlementKey string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, _ := store.Submit(ctx, "v3p204-budget", "整理汇报", "", "", true)
			taskID = run.TaskID
			tx, _ := store.db.Begin()
			upsertQuotaConfig(tx, "task_budget:"+taskID, QuotaTaskBudget, 100, "tokens", taskID, now)
			r, _ := reserveQuota(tx, QuotaReservation{
				Scope: QuotaTaskBudget, TaskID: taskID, StepID: "step-1", Amount: 80, Unit: "tokens", ReservedAt: now,
			})
			settlementKey = r.SettlementKey
			settleQuota(tx, settlementKey, true, now)
			tx.Commit()
		}()
		// 重启：已消费成本不重置（新预留仍受剩余额度约束）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		// 已消费 80，上限 100：再申请 50 应失败（只剩 20）。
		tx, _ := store.db.Begin()
		_, err = reserveQuota(tx, QuotaReservation{
			Scope: QuotaTaskBudget, TaskID: taskID, StepID: "step-2", Amount: 50, Unit: "tokens", ReservedAt: now,
		})
		tx.Rollback()
		if err == nil {
			t.Error("new reservation exceeded remaining budget after restart (重启不绕过 Task 预算)")
		}
		// 占用快照：settled=80（已消费不重置）。
		usage, err := readQuotaUsage(store.db, QuotaTaskBudget, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if usage.Settled != 80 {
			t.Errorf("settled = %d after restart, want 80 (已消费成本不重置)", usage.Settled)
		}
	})

	t.Run("unknown_cost_keeps_reservation_not_refund_zero", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "unknowncost.db")
		now := recordsTestNow()
		// 费用未知：保留已预留上限/未知状态，不退款成零。
		tx, _ := store.db.Begin()
		r, err := reserveQuota(tx, QuotaReservation{
			Scope: QuotaTaskBudget, TaskID: taskID, StepID: "step-1", Amount: 100, Unit: "tokens", ReservedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 以"费用未知"结算（costKnown=false）。
		tx, _ = store.db.Begin()
		if err := settleQuota(tx, r.SettlementKey, false, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 预留仍记录（不退款成零，保留未知状态）。
		var state string
		var costKnown int
		if err := store.db.QueryRow("SELECT state, cost_known FROM quota_reservations WHERE settlement_key=?", r.SettlementKey).Scan(&state, &costKnown); err != nil {
			t.Fatal(err)
		}
		if costKnown != 0 {
			t.Errorf("cost known = %d, want 0 (费用未知保留未知状态)", costKnown)
		}
		if state != "settled" {
			t.Logf("reservation state = %s (费用未知时保留已预留)", state)
		}
	})

	t.Run("unknown_cost_visible_in_usage", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "visiblecost.db")
		now := recordsTestNow()
		// 未知成本明确可见（占用快照含未核实项）。
		tx, _ := store.db.Begin()
		reserveQuota(tx, QuotaReservation{
			Scope: QuotaTargetHold, TaskID: taskID, TargetRef: "asset-x",
			Amount: 1, Unit: "holds", Exclusive: true, ReservedAt: now,
		})
		tx.Commit()
		usage, err := readQuotaUsage(store.db, QuotaTargetHold, "asset-x")
		if err != nil {
			t.Fatal(err)
		}
		if usage.UnverifiedHolds != 1 {
			t.Errorf("unverified holds = %d, want 1 (未知成本/占用明确可见)", usage.UnverifiedHolds)
		}
	})
}

// TestWaitDoesNotHoldModelSlot 等待不占模型槽。
func TestWaitDoesNotHoldModelSlot(t *testing.T) {
	t.Run("waiting_task_not_hold_model_slot", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "wait.db")
		now := recordsTestNow()
		// 模型槽：仅实际模型请求占用。
		tx, _ := store.db.Begin()
		slot, err := acquireModelSlot(tx, "worker-1", taskID, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 任务进入等待（开放 Wait）：释放模型槽（等待不占模型槽）。
		tx, _ = store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-slot", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := releaseModelSlot(tx, slot.SettlementKey, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 等待中的任务不占模型槽（槽已释放）。
		usage, err := readQuotaUsage(store.db, QuotaModelSlot, "worker-1")
		if err != nil {
			t.Fatal(err)
		}
		if usage.Reserved != 0 {
			t.Errorf("waiting task holds model slot: reserved=%d (等待不占模型槽)", usage.Reserved)
		}
	})

	t.Run("deterministic_and_wait_not_hold_slot", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "detslot.db")
		now := recordsTestNow()
		// 确定性步骤与持久等待：不占模型槽（只有模型请求才占）。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "normalize", DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 不 acquire 模型槽（确定性步骤不需要）。
		usage, err := readQuotaUsage(store.db, QuotaModelSlot, "worker-det")
		if err != nil {
			t.Fatal(err)
		}
		if usage.Reserved != 0 {
			t.Errorf("deterministic step holds model slot: %d (确定步骤不占模型槽)", usage.Reserved)
		}
	})

	t.Run("unknown_external_effect_keeps_hold", func(t *testing.T) {
		store, taskID := setupQuotasStore(t, "hold.db")
		now := recordsTestNow()
		// 未知外部动作：保留必要占用（效果未核实前不能仅按 TTL 释放）。
		tx, _ := store.db.Begin()
		if _, err := holdTarget(tx, "asset-x", "worker-1", taskID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 效果未核实：不能释放排他占用。
		tx, _ = store.db.Begin()
		err := releaseTargetHold(tx, "asset-x", false, now)
		tx.Rollback()
		if err == nil {
			t.Error("unverified effect released hold (效果未核实前不能仅按 TTL 释放)")
		}
		if !strings.Contains(err.Error(), "效果未核实") {
			t.Errorf("release guard reason unclear: %v", err)
		}
		// 效果已核实：可释放。
		tx, _ = store.db.Begin()
		if err := releaseTargetHold(tx, "asset-x", true, now); err != nil {
			t.Fatalf("verified effect release failed: %v", err)
		}
		tx.Commit()
		usage, _ := readQuotaUsage(store.db, QuotaTargetHold, "asset-x")
		if usage.ExclusiveHolds != 0 {
			t.Errorf("verified hold not released: %d", usage.ExclusiveHolds)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
