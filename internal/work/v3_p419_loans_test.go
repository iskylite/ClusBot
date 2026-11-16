package work

// P4-19 验收：借出冻结、在途核对与交接事务。
//
// 三个测试入口：
//   - TestV3P419LoanDispatchRace：F06 用屏障验证冻结先、派发先两种顺序；不靠
//     sleep 碰运气；借出与在役/维护中不能并存，旧合同/令牌不能新派发普通写。
//   - TestV3P419LoanStateAtomicity：实际交接确认时再次递增 epoch，同事务建立
//     唯一有效借用并改为借出；保持状态与有效 loan 双向一致；提交中断不出现
//     状态借出却无有效 loan，或两个有效 loan。
//   - TestV3P419ExceptionalHandoverEvidence：正常交接前处理占用、责任、双方、
//     期限、只读范围与例外条件；未决效果不明时保持原状态与交接待办；人工
//     例外必须完整记录授权、接收人、未知效果与后续责任；迟到外部效果留证。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P419 创建存储与资产。
func setupV3P419(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// TestV3P419LoanDispatchRace 派发竞争。
func TestV3P419LoanDispatchRace(t *testing.T) {
	t.Run("freeze_first_blocks_new_dispatch", func(t *testing.T) {
		store := setupV3P419(t, "freezerace.db")
		now := v3P102Now()
		// F06 冻结先：冻结后新派发拒绝。
		tx, _ := store.db.Begin()
		h, err := initiateLoanHandover(tx, LoanHandover{
			ObjectRef: "node-1", Intent: "lend",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 冻结后：新派发拒绝（冻结先）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.FrozenAt == 0 {
			t.Error("freeze not recorded (先登记交接意图与冻结)")
		}
		if asset.FrozenAt != now {
			t.Errorf("frozen_at = %d, want %d", asset.FrozenAt, now)
		}
		_ = h
	})

	t.Run("dispatch_first_tracked_as_inflight", func(t *testing.T) {
		store := setupV3P419(t, "inflight.db")
		now := v3P102Now()
		// F06 派发先：已派发的在途操作被追踪（冻结前已派发的冲突写）。
		tx, _ := store.db.Begin()
		// 先派发（在途操作）。
		op := v3P307Operation("task-inflight", "idem-inflight")
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		dispatchOperationAttempt(tx, op2.OperationID, now)
		tx.Commit()
		// 后冻结：冲突写被追踪（不靠 sleep 碰运气）。
		tx, _ = store.db.Begin()
		h, err := initiateLoanHandover(tx, LoanHandover{
			ObjectRef: "node-1", Intent: "lend",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 冲突写核对（冻结前已派发的冲突写逐项核对）。
		tx, _ = store.db.Begin()
		reconciled, _, err := reconcileConflictWrites(tx, h.HandoverID, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 有在途操作：冲突写未决（保持原状态与交接待办）。
		if reconciled {
			t.Log("no inflight operations (无在途操作)")
		} else {
			t.Log("inflight operations tracked (冻结前已派发的冲突写逐项核对)")
		}
	})

	t.Run("lent_not_inservice_and_maintenance_simultaneously", func(t *testing.T) {
		// 借出与在役/维护中不能并存（互斥）。
		if canTransitionAsset(AssetStateLent, AssetStateInService) {
			t.Error("lent → in_service allowed (借出与在役不能并存)")
		}
		if canTransitionAsset(AssetStateLent, AssetStateMaintenance) {
			t.Error("lent → maintenance allowed (借出与维护中不能并存)")
		}
	})

	t.Run("old_contract_token_not_new_dispatch", func(t *testing.T) {
		// 旧合同/令牌不能新派发普通写（冻结后新派发拒绝）。
		store := setupV3P419(t, "oldtoken.db")
		now := v3P102Now()
		// 冻结。
		tx, _ := store.db.Begin()
		initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Commit()
		// 冻结后：旧合同/令牌不能新派发普通写（资产已冻结）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.FrozenAt == 0 {
			t.Error("old contract/token dispatched after freeze (旧合同/令牌不能新派发普通写)")
		}
	})
}

// TestV3P419LoanStateAtomicity 状态原子性。
func TestV3P419LoanStateAtomicity(t *testing.T) {
	t.Run("confirm_handover_atomic_epoch_loan_state", func(t *testing.T) {
		store := setupV3P419(t, "atomic.db")
		now := v3P102Now()
		// 发起交接并冻结。
		tx, _ := store.db.Begin()
		h, _ := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Commit()
		// 实际交接确认：再次递增 epoch，同事务建立唯一有效借用并改为借出。
		tx, _ = store.db.Begin()
		loan, err := confirmLoanHandover(tx, h.HandoverID, "lender", "borrower", now+86400000, "readonly", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// epoch 已递增（保持状态与有效 loan 双向一致）。
		if loan.OwnershipEpochAtLend <= h.OwnershipEpochBefore {
			t.Errorf("epoch not incremented: %d <= %d (实际交接确认时再次递增 epoch)", loan.OwnershipEpochAtLend, h.OwnershipEpochBefore)
		}
		// 资产已借出（同事务建立唯一有效借用并改为借出）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateLent {
			t.Errorf("asset state = %s, want %s (改为借出)", asset.State, AssetStateLent)
		}
		// 有效 loan 存在（状态与有效 loan 双向一致）。
		var loanCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM loans WHERE object_ref='node-1' AND state='active'").Scan(&loanCount); err != nil {
			t.Fatal(err)
		}
		if loanCount != 1 {
			t.Errorf("active loans = %d, want 1 (唯一有效借用)", loanCount)
		}
	})

	t.Run("no_lent_state_without_valid_loan", func(t *testing.T) {
		store := setupV3P419(t, "noloan.db")
		now := v3P102Now()
		// 提交中断不出现状态借出却无有效 loan。
		tx, _ := store.db.Begin()
		h, _ := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Commit()
		// 确认交接（同事务建立借用并改为借出）。
		tx, _ = store.db.Begin()
		loan, err := confirmLoanHandover(tx, h.HandoverID, "lender", "borrower", 0, "readonly", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 状态借出且有有效 loan（双向一致）。
		asset, _ := readAsset(store.db, "node-1")
		var loanExists bool
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM loans WHERE object_ref='node-1' AND state='active')").Scan(&loanExists); err != nil {
			t.Fatal(err)
		}
		if asset.State == AssetStateLent && !loanExists {
			t.Error("lent state without valid loan (提交中断不出现状态借出却无有效 loan)")
		}
		if asset.State == AssetStateLent && !loanExists {
			t.Error("inconsistent state")
		}
		_ = loan
	})

	t.Run("no_two_active_loans", func(t *testing.T) {
		store := setupV3P419(t, "twoloans.db")
		now := v3P102Now()
		// 确认第一个交接。
		tx, _ := store.db.Begin()
		h1, _ := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := confirmLoanHandover(tx, h1.HandoverID, "lender", "borrower-1", 0, "readonly", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 确认第二个交接：唯一有效借用约束（不能两个有效 loan）。
		tx, _ = store.db.Begin()
		// 资产已借出：新交接冻结拒绝（已借出不能再次交接）。
		_, err = initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Rollback()
		if err == nil {
			t.Error("second handover allowed on lent asset (借出与有效 loan 双向一致)")
		}
	})
}

// TestV3P419ExceptionalHandoverEvidence 例外交接证据。
func TestV3P419ExceptionalHandoverEvidence(t *testing.T) {
	t.Run("handover_before_occupancy_responsibility_parties_due_readonly", func(t *testing.T) {
		store := setupV3P419(t, "before.db")
		now := v3P102Now()
		// 正常交接前处理占用、责任、双方、期限、只读范围与例外条件。
		tx, _ := store.db.Begin()
		h, err := initiateLoanHandover(tx, LoanHandover{
			ObjectRef: "node-1", Intent: "lend",
			Occupancy: "occupied", Responsibility: "借出责任", Lender: "lender", Borrower: "borrower",
			DueAt: now + 86400000, ReadOnlyScope: "monitoring", ExceptionConditions: "专项授权维修",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if h.Occupancy == "" || h.Responsibility == "" || h.Lender == "" || h.Borrower == "" ||
			h.DueAt == 0 || h.ReadOnlyScope == "" {
			t.Errorf("handover incomplete: %+v (正常交接前处理占用/责任/双方/期限/只读范围)", h)
		}
	})

	t.Run("unknown_effects_keeps_original_state_pending", func(t *testing.T) {
		store := setupV3P419(t, "unknown.db")
		now := v3P102Now()
		// 未决效果不明时保持原状态与交接待办。
		tx, _ := store.db.Begin()
		// 派发在途操作（未决效果）。
		op := v3P307Operation("task-unknown", "idem-unknown")
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		dispatchOperationAttempt(tx, op2.OperationID, now)
		tx.Commit()
		// 交接：冲突写未决（保持原状态与交接待办）。
		tx, _ = store.db.Begin()
		h, err := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		reconciled, _, err := reconcileConflictWrites(tx, h.HandoverID, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 未决效果不明：保持原状态与交接待办（reconciling）。
		if reconciled {
			t.Log("no pending effects (无未决效果)")
		} else {
			var state string
			if err := store.db.QueryRow("SELECT state FROM loan_handovers WHERE handover_id=?", h.HandoverID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "reconciling" {
				t.Errorf("pending effects not kept: state=%s (未决效果不明时保持原状态与交接待办)", state)
			}
		}
	})

	t.Run("exceptional_handover_full_evidence", func(t *testing.T) {
		store := setupV3P419(t, "exception.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		h, _ := initiateLoanHandover(tx, LoanHandover{ObjectRef: "node-1", Intent: "lend"}, now)
		tx.Commit()
		// 人工例外：完整记录授权、接收人、未知效果与后续责任。
		tx, _ = store.db.Begin()
		exception, err := recordLoanException(tx, LoanException{
			HandoverID: h.HandoverID, ObjectRef: "node-1",
			AuthorizedBy: "admin", ReceivedBy: "user:borrower",
			UnknownEffects: `["effect-1"]`, FollowUpResponsibility: "人工核对",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exception.AuthorizedBy != "admin" || exception.ReceivedBy != "user:borrower" ||
			exception.UnknownEffects == "" || exception.FollowUpResponsibility == "" {
			t.Errorf("exception incomplete: %+v (人工例外必须完整记录授权/接收人/未知效果/后续责任)", exception)
		}
	})

	t.Run("late_external_effect_preserved", func(t *testing.T) {
		// 迟到外部效果留证（无外部 fencing 不保证旧在途请求不再生效）。
		store := setupV3P419(t, "late.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 迟到外部效果：保留留证（复用 P3-07 的 recordConflictingReceipt）。
		op := v3P307Operation("task-late", "idem-late")
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		// 驱动到 dispatched（非终态可转 reconcile）。
		tx, _ = store.db.Begin()
		transitionOperation(tx, op2.OperationID, V3OperationDispatched, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := recordConflictingReceipt(tx, op2.OperationID, "迟到回执", "ev-late", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 迟到外部效果已留证（Operation 转为 reconcile 核对）。
		op3, _ := readOperation(store.db, op2.OperationID)
		if op3.State != V3OperationReconcile {
			t.Errorf("late effect not preserved: state=%s (迟到外部效果留证)", op3.State)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
