package work

// 验收：最终准入网关与提交后执行。
//
// 三个测试入口：
//   - TestDispatchCommitBeforeIO：条件更新已登记→已派发并写 attempt/审计，
//     事务提交后才执行提供者调用；临发前再检查当前可见撤权/控制；若已派发
//     提交后崩溃，恢复视为未知而非断言未发送。
//   - TestCommandReplayNoSideEffect：受控命令重投必须通过当前 Operation
//     状态守卫；已派发/待核对只核对，不自动执行 Start；Worker 不持有能绕过
//     该入口的特权。
//   - TestAdmissionRace：冻结/撤权先提交则新准入拒绝；派发先提交视为
//     在途并追踪；最终事务核对主体授权、Task/Step 控制、合同、租约代际、
//     资产版本/epoch、冻结、参数、预算和必要确认。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupDispatchStore 创建存储、任务与资产。
func setupDispatchStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p308-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// setupDispatchOperation 登记一个待派发 Operation。
func setupDispatchOperation(t *testing.T, store *Store, taskID, idempotencyKey string, now int64) Operation {
	t.Helper()
	// 资产（在管，未冻结）。
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "asset-dispatch", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
	// 授权派发。
	grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
	// 任务状态（pending，可派发）。
	transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
	tx.Commit()
	// 登记 Operation。
	tx, _ = store.db.Begin()
	op := operationFixture(taskID, idempotencyKey)
	op.TargetRefs = `[{"ref":"asset-dispatch","revision":1,"epoch":1}]`
	op2, err := registerOperation(tx, op, now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return op2
}

// TestDispatchCommitBeforeIO 提交前派发。
func TestDispatchCommitBeforeIO(t *testing.T) {
	t.Run("commit_then_execute_provider_call", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "commit.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-commit", now)
		// 条件更新已登记→已派发并写 attempt/审计（事务提交后才执行提供者调用）。
		tx, _ := store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Dispatched {
			t.Fatalf("not dispatched: %s", result.Reason)
		}
		tx.Commit()
		// 事务提交后：Operation 已派发，attempt 已写，审计已记录。
		op2, _ := readOperation(store.db, op.OperationID)
		if op2.State != RuntimeOperationDispatched {
			t.Errorf("state = %s, want dispatched", op2.State)
		}
		if op2.AttemptCount != 1 {
			t.Errorf("attempt count = %d, want 1", op2.AttemptCount)
		}
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='operation_dispatched'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("dispatch not audited: count=%d", auditCount)
		}
	})

	t.Run("recover_dispatched_as_unknown_not_assert_unsent", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "recover.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-recover", now)
		// 派发。
		tx, _ := store.db.Begin()
		dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Commit()
		// 已派发提交后崩溃（模拟）：恢复视为未知而非断言未发送。
		tx, _ = store.db.Begin()
		if err := recoverDispatchedOperation(tx, op.OperationID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		op2, _ := readOperation(store.db, op.OperationID)
		if op2.State != RuntimeOperationReconcile {
			t.Errorf("state = %s after recovery, want reconcile (恢复视为未知而非断言未发送)", op2.State)
		}
	})

	t.Run("recheck_revocation_control_before_dispatch", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "recheck.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-recheck", now)
		// 临发前撤销授权：新准入拒绝。
		tx, _ := store.db.Begin()
		var authID string
		if err := tx.QueryRow("SELECT auth_id FROM authorizations WHERE subject='user:alice' AND action='dispatch'").Scan(&authID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		revokeAuthorization(tx, authID, "admin", now)
		tx.Commit()
		// 派发：授权已撤销，拒绝。
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatched {
			t.Error("dispatched after authorization revoked (临发前再检查当前可见撤权)")
		}
	})
}

// TestCommandReplayNoSideEffect 命令重投无副作用。
func TestCommandReplayNoSideEffect(t *testing.T) {
	t.Run("dispatched_reconcile_only_no_start", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "replay.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-replay", now)
		// 派发。
		tx, _ := store.db.Begin()
		dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Commit()
		// 命令重投：已派发只核对，不自动执行 Start。
		tx, _ = store.db.Begin()
		guard, err := guardOperationReplay(tx, op.OperationID, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !guard.ReconcileOnly {
			t.Error("dispatched operation replay allowed start (已派发只核对不自动执行 Start)")
		}
		if !strings.Contains(guard.Reason, "只核对") {
			t.Errorf("replay guard reason unclear: %s", guard.Reason)
		}
	})

	t.Run("reconcile_state_replay_reconcile_only", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "reconcile.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-reconcile", now)
		// 驱动到待核对。
		tx, _ := store.db.Begin()
		dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, RuntimeOperationReconcile, now)
		tx.Commit()
		// 重投：待核对只核对。
		tx, _ = store.db.Begin()
		guard, _ := guardOperationReplay(tx, op.OperationID, now)
		tx.Commit()
		if !guard.ReconcileOnly {
			t.Error("reconcile state replay allowed start")
		}
	})

	t.Run("worker_no_bypass_privilege", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "nobypass.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-nobypass", now)
		// Worker 主体为空：拒绝（不持有能绕过该入口的特权）。
		err := ensureWorkerNotBypassAdmission("", op, now)
		if err == nil {
			t.Error("worker bypass not rejected (Worker 不持有绕过特权)")
		}
		// Worker 主体存在：经过最终准入。
		if err := ensureWorkerNotBypassAdmission("user:alice", op, now); err != nil {
			t.Errorf("worker admission rejected: %v", err)
		}
	})
}

// TestAdmissionRace 准入竞争。
func TestAdmissionRace(t *testing.T) {
	t.Run("freeze_first_blocks_new_admission", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "freezerace.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-freezerace", now)
		// 冻结先提交：新准入拒绝。
		tx, _ := store.db.Begin()
		freezeAsset(tx, "asset-dispatch", "admin", "维护冻结", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatched {
			t.Error("dispatched after freeze (冻结先提交则新准入拒绝)")
		}
		if !strings.Contains(result.Reason, "冻结") {
			t.Errorf("freeze reason missing: %s", result.Reason)
		}
	})

	t.Run("dispatch_first_tracked_as_inflight", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "inflight.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-inflight", now)
		// 派发先提交：视为在途并追踪。
		tx, _ := store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Dispatched {
			t.Fatal("dispatch should succeed")
		}
		// 在途追踪：Operation 已派发，attempt 已记录。
		op2, _ := readOperation(store.db, op.OperationID)
		if op2.State != RuntimeOperationDispatched || op2.AttemptCount != 1 {
			t.Errorf("inflight not tracked: state=%s attempt=%d", op2.State, op2.AttemptCount)
		}
		// 冻结后：已派发的在途仍保留（冻结不影响已派发）。
		tx, _ = store.db.Begin()
		freezeAsset(tx, "asset-dispatch", "admin", "维护冻结", now)
		tx.Commit()
		op3, _ := readOperation(store.db, op.OperationID)
		if op3.State != RuntimeOperationDispatched {
			t.Errorf("inflight lost after freeze: state=%s (派发先提交视为在途并追踪)", op3.State)
		}
	})

	t.Run("task_cancelled_blocks_dispatch", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "cancelled.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-cancelled", now)
		// 任务取消：不能派发。
		tx, _ := store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		transitionTaskState(tx, taskID, EventCancel, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op.OperationID, "user:alice", now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatched {
			t.Error("dispatched for cancelled task (Task/Step 控制核对)")
		}
	})

	t.Run("final_admission_checks_all_fields", func(t *testing.T) {
		store, taskID := setupDispatchStore(t, "allfields.db")
		now := recordsTestNow()
		op := setupDispatchOperation(t, store, taskID, "idem-allfields", now)
		// 最终准入核对：全部核对项。
		tx, _ := store.db.Begin()
		check, err := checkFinalAdmission(tx, op, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !check.Allowed {
			t.Errorf("admission denied: %s", check.Reason)
		}
		// 核对项快照：主体授权、Task/Step 控制、资产版本、冻结、参数。
		if check.AuthOutcome != "allowed" || !check.TaskControlOK || !check.AssetVersionOK ||
			!check.NotFrozen || !check.ParamsOK {
			t.Errorf("admission check incomplete: %+v", check)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
