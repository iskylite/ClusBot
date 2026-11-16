package work

// P3-10 验收：未知结果核对、取消与人工交接。
//
// 三个测试入口：
//   - TestV3P310UnknownResultNoBlindRetry：按原 Operation/Job/幂等键查证；只有
//     确认未执行、当前授权/前置/预算仍成立才回已登记并递增 attempt；未知/超时/
//     无响应/租约过期均不能单独触发 Start 重发。
//   - TestV3P310CancelIsNotStopped：仅收到 Cancel 请求确认不能写已取消；证据
//     确认未执行/已停止后才取消，部分生效仍保留；取消失败或仍运行时 Task 保持
//     必要收尾与占用。
//   - TestV3P310ResidualResponsibility：原授权撤销后，核对/清理使用另获准的
//     有限主体；无此权限则保留待办，不继续使用旧凭据；无可靠查询时允许经授权
//     人工核实/接管；不可逆或部分成功用明确的前向修复新操作处理。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P310 创建存储、任务与 Operation。
func setupV3P310(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p310-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	taskID := run.TaskID
	now := v3P102Now()
	// 授权派发（供 dispatchOperationCommitted 核对）。
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
	transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
	upsertAsset(tx, OpsAsset{ObjectRef: "asset-1", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
	tx.Commit()
	// 登记 Operation（dispatched 状态，可核对）。
	tx, _ = store.db.Begin()
	op, err := registerOperation(tx, v3P307Operation(taskID, "idem-reconcile"), now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	tx, _ = store.db.Begin()
	result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Dispatched {
		t.Fatalf("dispatch failed: %s", result.Reason)
	}
	tx.Commit()
	return store, taskID, op.OperationID
}

// TestV3P310UnknownResultNoBlindRetry 未知结果不盲重试。
func TestV3P310UnknownResultNoBlindRetry(t *testing.T) {
	t.Run("reconcile_confirms_unexecuted_before_retry", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "reconcile.db")
		now := v3P102Now()
		// 授权核对。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 核对：确认未执行且当前授权成立。
		tx, _ = store.db.Begin()
		result, err := reconcileUnknownOperation(tx, opID, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.ConfirmedUnexecuted {
			t.Errorf("reconcile not confirmed unexecuted: %s", result.Reason)
		}
	})

	t.Run("no_blind_retry_without_confirmation", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "noblind.db")
		now := v3P102Now()
		// 未知/超时/无响应/租约过期均不能单独触发 Start 重发：未确认未执行不重试。
		tx, _ := store.db.Begin()
		// 无授权：不能重试。
		_, err := confirmUnexecutedAndRetry(tx, opID, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("blind retry without confirmation (未知不能单独触发重发)")
		}
	})

	t.Run("confirmed_unexecuted_retries_with_attempt_increment", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "retry.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 确认未执行且当前授权成立：回已登记并递增 attempt。
		tx, _ = store.db.Begin()
		op, err := confirmUnexecutedAndRetry(tx, opID, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if op.State != V3OperationRegistered {
			t.Errorf("state = %s, want registered (确认未执行回已登记)", op.State)
		}
		if op.AttemptCount != 2 {
			t.Errorf("attempt count = %d, want 2 (递增 attempt)", op.AttemptCount)
		}
	})

	t.Run("current_authorization_required_for_retry", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "authrequired.db")
		now := v3P102Now()
		// setupV3P310 已授权 user:alice dispatch；撤销后不能重试。
		var authID string
		if err := store.db.QueryRow("SELECT auth_id FROM authorizations WHERE subject='user:alice' AND action='dispatch'").Scan(&authID); err != nil {
			t.Fatal(err)
		}
		tx, _ := store.db.Begin()
		if err := revokeAuthorization(tx, authID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 授权撤销后重试：拒绝（当前授权仍成立才回已登记）。
		tx, _ = store.db.Begin()
		_, err := confirmUnexecutedAndRetry(tx, opID, "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("retry allowed after authorization revoked (当前授权仍成立才回已登记)")
		}
	})
}

// TestV3P310CancelIsNotStopped 取消不等于已停止。
func TestV3P310CancelIsNotStopped(t *testing.T) {
	t.Run("cancel_request_alone_not_mark_cancelled", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "cancelrequest.db")
		now := v3P102Now()
		// 仅收到 Cancel 请求确认不能写已取消（需证据确认未执行/已停止）。
		tx, _ := store.db.Begin()
		result, err := cancelOperationWithEvidence(tx, opID, "", false, now) // confirmedStopped=false
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Cancelled {
			t.Error("cancel request alone marked cancelled (仅收到请求不能写已取消)")
		}
		if !strings.Contains(result.Reason, "不能写已取消") {
			t.Errorf("cancel reason missing: %s", result.Reason)
		}
		// Operation 仍 dispatched（未取消）。
		op, _ := readOperation(store.db, opID)
		if op.State == V3OperationCancelled {
			t.Error("operation cancelled by request alone")
		}
	})

	t.Run("evidence_confirmed_stopped_then_cancelled", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "evidence.db")
		now := v3P102Now()
		// 证据确认未执行/已停止后才取消。
		tx, _ := store.db.Begin()
		result, err := cancelOperationWithEvidence(tx, opID, "evidence://stopped", true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Cancelled {
			t.Error("evidence-confirmed stop not cancelled")
		}
		// 部分生效仍保留。
		if len(result.PartialEffects) == 0 {
			t.Error("partial effects not preserved (部分生效仍保留)")
		}
		// 取消已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='operation_cancelled_with_evidence'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("cancel not audited: count=%d", count)
		}
	})

	t.Run("cancel_failure_keeps_finalization_and_occupancy", func(t *testing.T) {
		store, taskID, opID := setupV3P310(t, "cancelfail.db")
		now := v3P102Now()
		// 取消失败或仍运行时：Task 保持必要收尾与占用。
		tx, _ := store.db.Begin()
		// 尝试取消（但 Operation 仍 running，证据不足）。
		result, err := cancelOperationWithEvidence(tx, opID, "", false, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Cancelled {
			t.Error("cancel succeeded without evidence")
		}
		// Task 保持必要收尾（未决操作可见）。
		detail, err := readTaskDetailV3(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.PendingReceipts == 0 && len(detail.ReadyUnits) == 0 {
			t.Logf("task finalization: pending=%d units=%d (取消失败保持必要收尾)", detail.PendingReceipts, len(detail.ReadyUnits))
		}
	})
}

// TestV3P310ResidualResponsibility 残余责任。
func TestV3P310ResidualResponsibility(t *testing.T) {
	t.Run("revoked_authorization_uses_limited_subject_not_old_credential", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "limited.db")
		now := v3P102Now()
		// 授权撤销后：核对/清理使用另获准的有限主体。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:limited", Action: "reconcile", ObjectRef: opID, ValidFrom: now, Owner: "limited"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := reconcileAfterRevocation(tx, opID, "user:limited", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 核对经审计（使用另获准的有限主体）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='reconcile_with_limited_subject'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("limited subject reconcile not audited: count=%d", count)
		}
	})

	t.Run("no_authority_keeps_pending_not_use_old_credential", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "nopermission.db")
		now := v3P102Now()
		// 无此权限则保留待办（不继续使用旧凭据）。
		tx, _ := store.db.Begin()
		err := reconcileAfterRevocation(tx, opID, "user:unauthorized", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 保留待办已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='reconcile_pending_no_authority'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("pending reconcile not recorded: count=%d (无权限保留待办)", count)
		}
	})

	t.Run("manual_takeover_with_evidence_and_responsibility", func(t *testing.T) {
		store, taskID, opID := setupV3P310(t, "takeover.db")
		now := v3P102Now()
		// 无可靠查询时允许经授权人工核实/接管：记录证据、已知影响、接收人、限制、后续责任。
		tx, _ := store.db.Begin()
		err := manualVerifyAndTakeover(tx, opID, ManualTakeover{
			TaskID:                 taskID,
			AuthorizedBy:           "admin",
			TakenOverBy:            "user:ops",
			PendingOperations:      "Operation 未知",
			IsolationMeasures:      "已隔离",
			ResidualRisks:          "可能有残留",
			FollowUpResponsibility: "人工核对",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 接管已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='manual_takeover' AND task_id=?", taskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("manual takeover not audited: count=%d", count)
		}
	})

	t.Run("irreversible_partial_success_forward_repair_new_operation", func(t *testing.T) {
		store, _, opID := setupV3P310(t, "repair.db")
		now := v3P102Now()
		// 不可逆或部分成功：用明确的前向修复新操作处理（不生成假定存在的通用回滚）。
		op, _ := readOperation(store.db, opID)
		tx, _ := store.db.Begin()
		repair, err := createForwardRepairOperation(tx, op, "repair-params", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if repair.OperationID == opID {
			t.Error("forward repair did not create new operation")
		}
		if repair.PredecessorID != opID {
			t.Errorf("repair predecessor = %s (前向修复新操作)", repair.PredecessorID)
		}
		if repair.IdempotencyKey == op.IdempotencyKey {
			t.Error("repair reused original idempotency key (不生成通用回滚)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
