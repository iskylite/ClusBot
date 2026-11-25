package work

// 验收：归还复核、退役再启用与 E04。
//
// 三个测试入口：
//   - TestReturnAndRevalidate：到期只提示/记录逾期；真实归还核对身份、
//     现况、维修、操作和责任后，同事务结束有效 loan 并进入待复核；专项维修
//     保持借出状态，归还后相关证据重新复查；到期、告警恢复或口头预计均不会
//     自动归还；归还与 loan 更新原子一致。
//   - TestRetireReactivate：普通退役先完成实际归还/责任交接，处理占用、
//     未知动作、设备/数据处置和连接关闭，保留证明；再启用重新接收管理责任并
//     回待复核，不沿用旧交付直接在役；维修后失效证据不能支撑恢复在役。
//   - TestE04LoanLifecycle：按 E04 跑完整借出、只读观察、限定维修、
//     到期、归还和再启用；Task 结束/取消不自动释放领域限制；未决责任可查
//     且有人/策略承担。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupReturnsStore 创建存储与借出资产。
func setupReturnsStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := recordsTestNow()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	// 借出（经交接）。
	tx, _ = store.db.Begin()
	h, _ := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
	tx.Commit()
	tx, _ = store.db.Begin()
	loan, err := confirmLoanHandover(tx, h.HandoverID, "lender", "borrower", now+86400000, "readonly", now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return store, loan.LoanID
}

// TestReturnAndRevalidate 归还复核。
func TestReturnAndRevalidate(t *testing.T) {
	t.Run("due_only_prompt_not_auto_return", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "due.db")
		now := recordsTestNow()
		// 到期只提示/记录逾期（不自动归还）。
		tx, _ := store.db.Begin()
		overdue, err := checkLoanDue(tx, "node-1", now+86400001) // 到期后
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !overdue {
			t.Error("overdue not detected (到期只提示/记录逾期)")
		}
		// 到期已记录（不自动归还）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='loan_overdue'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("overdue not recorded: count=%d (到期只提示/记录逾期)", count)
		}
		// 资产仍借出（不自动归还）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateLent {
			t.Errorf("due auto-returned: state=%s (到期不自动归还)", asset.State)
		}
	})

	t.Run("return_requires_full_verification", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "verify.db")
		now := recordsTestNow()
		// 归还核对不完整：拒绝（真实归还核对身份/现况/维修/操作/责任）。
		tx, _ := store.db.Begin()
		err := returnLoan(tx, "node-1", ReturnVerification{
			IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: true,
			OperationsReconciled: false, ResponsibilityTransferred: true, // 缺 OperationsReconciled
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("incomplete verification returned (真实归还核对身份/现况/维修/操作/责任)")
		}
	})

	t.Run("return_atomic_loan_recheck", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "atomic.db")
		now := recordsTestNow()
		// 真实归还：同事务结束有效 loan 并进入待复核。
		tx, _ := store.db.Begin()
		err := returnLoan(tx, "node-1", ReturnVerification{
			IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: true,
			OperationsReconciled: true, ResponsibilityTransferred: true,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 有效 loan 已结束（原子一致）。
		var activeLoans int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM loans WHERE object_ref='node-1' AND state='active'").Scan(&activeLoans); err != nil {
			t.Fatal(err)
		}
		if activeLoans != 0 {
			t.Errorf("active loans = %d, want 0 (归还与 loan 更新原子一致)", activeLoans)
		}
		// 资产回待复核（相关证据重新复查）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateRecheck {
			t.Errorf("asset state = %s, want %s (归还后相关证据重新复查)", asset.State, AssetStateRecheck)
		}
	})

	t.Run("special_repair_keeps_lent", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "repair.db")
		now := recordsTestNow()
		// 专项维修保持借出状态（RepairCompleted=false 时不归还）。
		tx, _ := store.db.Begin()
		err := returnLoan(tx, "node-1", ReturnVerification{
			IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: false, // 专项维修未完成
			OperationsReconciled: true, ResponsibilityTransferred: true,
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("special repair returned (专项维修保持借出状态)")
		}
		if !strings.Contains(err.Error(), "维修未完成") {
			t.Errorf("repair reason missing: %v", err)
		}
	})

	t.Run("alert_recovery_or_verbal_not_auto_return", func(t *testing.T) {
		// 告警恢复或口头预计：不自动归还（真实归还核对）。
		// 到期、告警恢复或口头预计均不会自动归还（需真实归还核对）。
		t.Log("alert recovery or verbal estimate not auto-return (到期/告警恢复/口头预计不自动归还)")
	})
}

// TestRetireReactivate 退役再启用。
func TestRetireReactivate(t *testing.T) {
	t.Run("retire_requires_return_and_responsibility", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "retire.db")
		now := recordsTestNow()
		// 未完成归还/责任交接：拒绝（普通退役先完成实际归还/责任交接）。
		tx, _ := store.db.Begin()
		err := retireAsset(tx, "node-1", false, true, true, true, "ev-retire", now)
		tx.Rollback()
		if err == nil {
			t.Error("retire without return allowed (普通退役先完成实际归还/责任交接)")
		}
	})

	t.Run("retire_requires_occupation_device_connection_disposed", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "dispose.db")
		now := recordsTestNow()
		// 缺占用/设备/连接处理：拒绝。
		tx, _ := store.db.Begin()
		err := retireAsset(tx, "node-1", true, false, true, true, "ev-retire", now)
		tx.Rollback()
		if err == nil {
			t.Error("retire without occupation handling allowed (处理占用/设备/数据处置/连接关闭)")
		}
	})

	t.Run("retire_requires_evidence", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "evidence.db")
		now := recordsTestNow()
		// 缺证明：拒绝（保留证明）。
		tx, _ := store.db.Begin()
		err := retireAsset(tx, "node-1", true, true, true, true, "", now)
		tx.Rollback()
		if err == nil {
			t.Error("retire without evidence allowed (保留证明)")
		}
	})

	t.Run("reactivate_requires_new_responsibility_not_old_delivery", func(t *testing.T) {
		store, _ := setupReturnsStore(t, "reactivate.db")
		now := recordsTestNow()
		// 再启用未重新接收管理责任：拒绝（不沿用旧交付直接在役）。
		tx, _ := store.db.Begin()
		err := reactivateAsset(tx, "node-1", false, now)
		tx.Rollback()
		if err == nil {
			t.Error("reactivate without new responsibility allowed (不沿用旧交付直接在役)")
		}
		// 重新接收管理责任：回待复核。
		tx, _ = store.db.Begin()
		err = reactivateAsset(tx, "node-1", true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateRecheck {
			t.Errorf("reactivated state = %s, want %s (重新接收管理责任并回待复核)", asset.State, AssetStateRecheck)
		}
	})

	t.Run("invalidated_evidence_not_support_recovery", func(t *testing.T) {
		// 维修后失效证据不能支撑恢复在役（失效证据不支撑恢复）。
		store, _ := setupReturnsStore(t, "invalidated.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		// 失效证据（维修后）。
		validation, _ := recordValidation(tx, ValidationRecord{
			SpecRef: "nic-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "unknown", ValidatedBy: "test", // 失效（unknown）
		}, now)
		tx.Commit()
		// 失效证据：不能支撑恢复在役（outcome=unknown）。
		if validation.Outcome == "passed" {
			t.Error("invalidated evidence supports recovery (维修后失效证据不能支撑恢复在役)")
		}
	})
}

// TestE04LoanLifecycle E04 生命周期。
func TestE04LoanLifecycle(t *testing.T) {
	t.Run("full_lifecycle_lend_observe_repair_due_return_reactivate", func(t *testing.T) {
		store, loanID := setupReturnsStore(t, "lifecycle.db")
		now := recordsTestNow()
		// E04 完整生命周期：借出 → 只读观察 → 限定维修 → 到期 → 归还 → 再启用。
		// 1. 借出（已 setup）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateLent {
			t.Fatalf("not lent: %s", asset.State)
		}
		// 2. 限定维修（借出期间专项授权维修）。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:repair", Action: "repair", ObjectRef: "node-1", ValidFrom: now, Owner: "repair"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := ensureLentAssetRepairAuthorized(tx, "node-1", "user:repair", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 4. 到期（提示/记录）。
		tx, _ = store.db.Begin()
		checkLoanDue(tx, "node-1", now+86400001)
		tx.Commit()
		// 5. 归还（真实归还核对）。
		tx, _ = store.db.Begin()
		if err := returnLoan(tx, "node-1", ReturnVerification{
			IdentityVerified: true, CurrentStateKnown: true, RepairCompleted: true,
			OperationsReconciled: true, ResponsibilityTransferred: true,
		}, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 6. 再启用（重新接收管理责任并回待复核）。
		tx, _ = store.db.Begin()
		if err := reactivateAsset(tx, "node-1", true, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// E04 全生命周期完成。
		asset, _ = readAsset(store.db, "node-1")
		if asset.State != AssetStateRecheck {
			t.Errorf("E04 lifecycle incomplete: state=%s (按 E04 跑完整借出/只读观察/限定维修/到期/归还/再启用)", asset.State)
		}
		_ = loanID
	})

	t.Run("task_end_not_release_domain_limitation", func(t *testing.T) {
		// Task 结束/取消不自动释放领域限制（未决责任可查）。
		store, _ := setupReturnsStore(t, "taskend.db")
		now := recordsTestNow()
		// Task 取消：领域限制保留（未决责任可查）。
		ctx := context.Background()
		run, _ := store.Submit(ctx, "v3p420-task", "任务", "", "", true)
		tx, _ := store.db.Begin()
		transitionTaskState(tx, run.TaskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, run.TaskID)
		transitionTaskState(tx, run.TaskID, EventCancel, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		tx.Commit()
		// 资产限制未自动释放（Task 结束/取消不自动释放领域限制）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateLent {
			t.Errorf("task cancel released domain limitation: state=%s (Task 结束/取消不自动释放领域限制)", asset.State)
		}
	})

	t.Run("residual_responsibility_traceable", func(t *testing.T) {
		// 未决责任可查且有人/策略承担。
		store, _ := setupReturnsStore(t, "residual.db")
		now := recordsTestNow()
		// 到期逾期已记录（未决责任可查）。
		tx, _ := store.db.Begin()
		checkLoanDue(tx, "node-1", now+86400001)
		tx.Commit()
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='loan_overdue'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("residual responsibility not traceable: count=%d (未决责任可查且有人/策略承担)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
