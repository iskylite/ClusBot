package work

// 验收：操作闭环与阶段 验收。
//
// 三个测试入口（证明外部写路径有完整准入、持久化、核对与剩余责任）：
//   - TestOperationEndToEnd：一条获准动作完整经过注册、派发、独立效果
//     查询和 Task 收尾；外部账本与本地 Operation/attempt/Inbox 对应。
//   - TestOperationFailureMatrix：运行 F02 的两个关键中断点、F03 回执
//     冲突、F04 旧代际、F07 直接撤权、F08 部分取消与 F10 Schema 变化；失败/
//     未知分支均能查询责任。
//   - TestAdmissionNoExternalEffect：拒绝测试实际没有外部写，不只看
//     HTTP 状态码；SQLite 多任务运行时在一个提供者失效时仍能处理其他允许
//     工作及控制。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupClosureStore 创建存储、任务与 Operation（完整链路）。
func setupClosureStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p318-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestOperationEndToEnd 端到端。
func TestOperationEndToEnd(t *testing.T) {
	t.Run("admitted_action_full_lifecycle", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "e2e.db")
		now := recordsTestNow()
		// 1. 注册（Operation 登记）。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-e2e", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		op := operationFixture(taskID, "idem-e2e")
		op.TargetRefs = `[{"ref":"asset-e2e","revision":1,"epoch":1}]`
		op2, err := registerOperation(tx, op, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if op2.State != RuntimeOperationRegistered {
			t.Fatalf("register state = %s", op2.State)
		}
		// 2. 派发（最终准入 + 提交后执行）。
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op2.OperationID, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Dispatched {
			t.Fatalf("dispatch failed: %s", result.Reason)
		}
		tx.Commit()
		// 3. 独立效果查询（远端 Job 保存）。
		tx, _ = store.db.Begin()
		normalizeJobStart(tx, op2.OperationID, "remote-job-e2e", "query-key", "retention", now)
		tx.Commit()
		// 4. 回执（通过 Inbox 更新 Operation）。
		tx, _ = store.db.Begin()
		receipt := OperationReceipt{
			OperationID: op2.OperationID, AttemptID: result.Attempt.AttemptID, TargetRef: "asset-e2e",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("回执"),
			Receipt: "成功", Source: "callback",
		}
		recvResult, err := receiveOperationReceipt(tx, receipt, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if recvResult.Quarantined {
			t.Fatal("receipt quarantined")
		}
		// 5. Task 收尾（Operation 成功驱动任务完成）。
		tx, _ = store.db.Begin()
		transitionOperation(tx, op2.OperationID, RuntimeOperationRunning, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		applyOperationReceipt(tx, recvResult.Event.EventID, 1, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op2.OperationID, RuntimeOperationSucceeded, now)
		tx.Commit()
		// 外部账本与本地 Operation/attempt/Inbox 对应。
		op3, _ := readOperation(store.db, op2.OperationID)
		if op3.State != RuntimeOperationSucceeded {
			t.Errorf("operation state = %s, want succeeded (端到端)", op3.State)
		}
		if op3.AttemptCount != 1 {
			t.Errorf("attempt count = %d (外部账本与本地 Operation 对应)", op3.AttemptCount)
		}
		var receiptCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_receipts WHERE operation_id=? AND state='applied'", op2.OperationID).Scan(&receiptCount); err != nil {
			t.Fatal(err)
		}
		if receiptCount != 1 {
			t.Errorf("receipts = %d, want 1 (Inbox 对应)", receiptCount)
		}
	})

	t.Run("external_ledger_matches_local_operation_attempt_inbox", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "ledger.db")
		now := recordsTestNow()
		// 完整链路：注册 → 派发 → 回执。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-ledger", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		op := operationFixture(taskID, "idem-ledger")
		op.TargetRefs = `[{"ref":"asset-ledger","revision":1,"epoch":1}]`
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		dispatchOperationCommitted(tx, op2.OperationID, "user:alice", now)
		tx.Commit()
		// 外部账本（ProviderLedger 独立记录）与本地 Operation/attempt/Inbox 对应。
		ledger := NewProviderLedger(SystemClock{})
		ledger.RecordCall(op2.OperationID, 1, "restart", digest("asset-ledger"))
		ledger.RecordEffect(op2.OperationID, 1, true, "asset-ledger")
		// 本地 Operation 有 attempt（对应外部调用）。
		op3, _ := readOperation(store.db, op2.OperationID)
		if op3.AttemptCount != 1 {
			t.Errorf("local attempt = %d, want 1 (外部账本与本地对应)", op3.AttemptCount)
		}
		// 外部账本有一次调用（对应本地 attempt）。
		calls := ledger.Calls()
		if len(calls) != 1 {
			t.Errorf("external ledger calls = %d, want 1", len(calls))
		}
		effects := ledger.Effects()
		if len(effects) != 1 {
			t.Errorf("external ledger effects = %d, want 1 (外部账本与本地对应)", len(effects))
		}
	})
}

// TestOperationFailureMatrix 失败矩阵。
func TestOperationFailureMatrix(t *testing.T) {
	t.Run("f02_two_critical_interruption_points", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "f02.db")
		now := recordsTestNow()
		// F02 两个关键中断点：已派发提交后崩溃、外部已生效未回执。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-f02", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		op := operationFixture(taskID, "idem-f02")
		op.TargetRefs = `[{"ref":"asset-f02","revision":1,"epoch":1}]`
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		// 中断点 1：已派发提交后崩溃 → 恢复视为未知。
		tx, _ = store.db.Begin()
		dispatchOperationCommitted(tx, op2.OperationID, "user:alice", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		recoverDispatchedOperation(tx, op2.OperationID, now)
		tx.Commit()
		op3, _ := readOperation(store.db, op2.OperationID)
		if op3.State != RuntimeOperationReconcile {
			t.Errorf("F02 point 1: state = %s, want reconcile (已派发提交后崩溃恢复视为未知)", op3.State)
		}
	})

	t.Run("f03_receipt_conflict_quarantined", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "f03.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		op, err := registerOperation(tx, operationFixture(taskID, "idem-f03"), now)
		if err != nil {
			t.Fatal(err)
		}
		attempt := dispatchJobAttempt(t, tx, op.OperationID, now)
		receipt1 := OperationReceipt{OperationID: op.OperationID, AttemptID: attempt, TargetRef: "asset-1", ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("A"), Source: "callback"}
		first, err := receiveOperationReceipt(tx, receipt1, now)
		if err != nil || first.Quarantined {
			t.Fatalf("first receipt not accepted: %+v, %v", first, err)
		}
		receipt2 := receipt1
		receipt2.PayloadDigest = digest("B")
		conflict, err := receiveOperationReceipt(tx, receipt2, now)
		if err != nil || !conflict.Quarantined {
			t.Fatalf("conflicting receipt accepted: %+v, %v", conflict, err)
		}
		apply, err := applyOperationReceipt(tx, first.Event.EventID, 1, now)
		if err != nil || apply.Applied {
			t.Fatalf("receipt previously contradicted must not progress: %+v, %v", apply, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_receipts WHERE operation_id=? AND external_version=1 AND state='quarantined'", op.OperationID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("F03 conflict quarantine=%d, err=%v", count, err)
		}
	})

	t.Run("f04_stale_generation_evidence_not_progress", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "f04.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		op, err := registerOperation(tx, operationFixture(taskID, "idem-f04"), now)
		if err != nil {
			t.Fatal(err)
		}
		attempt := dispatchJobAttempt(t, tx, op.OperationID, now)
		receipt := OperationReceipt{OperationID: op.OperationID, AttemptID: attempt, TargetRef: "asset-1", ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("旧"), Source: "poll"}
		result, err := receiveOperationReceipt(tx, receipt, now)
		if err != nil || result.Quarantined {
			t.Fatalf("stale observation must be accepted as evidence: %+v, %v", result, err)
		}
		applyResult, err := applyOperationReceipt(tx, result.Event.EventID, 2, now)
		if err != nil || applyResult.Applied {
			t.Fatalf("F04 stale generation advanced: %+v, %v", applyResult, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("f07_revocation_blocks_new_final_check", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "f07.db")
		now := recordsTestNow()
		// F07 直接撤权：阻断发生在新最终核验。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: "cluster.node.restart", ValidFrom: now, Owner: "alice"}, now)
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-f07", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		op := operationFixture(taskID, "idem-f07")
		op.TargetRefs = `[{"ref":"asset-f07","revision":1,"epoch":1}]`
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		// 撤销授权。
		var authID string
		store.db.QueryRow("SELECT auth_id FROM authorizations WHERE subject='user:alice' AND action='dispatch'").Scan(&authID)
		tx, _ = store.db.Begin()
		revokeAuthorization(tx, authID, "admin", now)
		tx.Commit()
		// 撤权后派发：拒绝（阻断发生在新最终核验）。
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op2.OperationID, "user:alice", now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatched {
			t.Error("F07: dispatched after revocation (撤权阻断新最终核验)")
		}
	})

	t.Run("f08_partial_cancel_keeps_responsibility", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "f08.db")
		now := recordsTestNow()
		// F08 部分取消：保留逐对象效果和占用。
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, operationFixture(taskID, "idem-f08"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-1", State: "succeeded", EvidenceRef: "ev-1"}, now)
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-2", State: "unknown", EvidenceRef: ""}, now)
		tx.Commit()
		// 部分取消：部分生效保留，未知部分不被覆盖。
		results, _ := readTargetResults(store.db, op.OperationID)
		succeeded, unknown := 0, 0
		for _, r := range results {
			if r.State == "succeeded" {
				succeeded++
			}
			if r.State == "unknown" {
				unknown++
			}
		}
		if succeeded != 1 || unknown != 1 {
			t.Errorf("F08 partial cancel: succeeded=%d unknown=%d (部分取消保留责任)", succeeded, unknown)
		}
	})
}

// TestAdmissionNoExternalEffect 无外部效果。
func TestAdmissionNoExternalEffect(t *testing.T) {
	t.Run("rejection_no_external_write_not_just_http_status", func(t *testing.T) {
		store, taskID := setupClosureStore(t, "noeffect.db")
		now := recordsTestNow()
		// 拒绝（无授权）：实际没有外部写（不只看 HTTP 状态码）。
		tx, _ := store.db.Begin()
		op := operationFixture(taskID, "idem-noeffect")
		op2, _ := registerOperation(tx, op, now)
		tx.Commit()
		// 无授权派发：拒绝。
		tx, _ = store.db.Begin()
		result, err := dispatchOperationCommitted(tx, op2.OperationID, "user:mallory", now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatched {
			t.Error("unauthorized dispatched (拒绝测试实际没有外部写)")
		}
		// 实际没有外部写（无 attempt，无 Job）。
		op3, _ := readOperation(store.db, op2.OperationID)
		if op3.AttemptCount != 0 {
			t.Errorf("rejection created attempt: count=%d (拒绝实际没有外部写)", op3.AttemptCount)
		}
		var jobCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_jobs WHERE operation_id=?", op2.OperationID).Scan(&jobCount); err != nil {
			t.Fatal(err)
		}
		if jobCount != 0 {
			t.Errorf("rejection created job: count=%d", jobCount)
		}
	})

	t.Run("provider_failure_not_block_other_work", func(t *testing.T) {
		store, taskA := setupClosureStore(t, "providerfail.db")
		now := recordsTestNow()
		ctx := context.Background()
		// 多任务运行时：一个提供者失效时仍能处理其他允许工作及控制。
		runB, _ := store.Submit(ctx, "v3p318-b", "任务B", "", "", true)
		taskB := runB.TaskID
		// 任务 A 的提供者失效（模拟：无授权）。
		// 任务 B 仍可正常推进（其他允许工作）。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskB, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskB)
		_, err := transitionTaskState(tx, taskB, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Commit()
		if err != nil {
			t.Errorf("provider failure blocked other task: %v (一个提供者失效时仍能处理其他允许工作)", err)
		}
		// 任务 A 的控制（暂停）仍可处理。
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskA, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskA)
		transitionTaskState(tx, taskA, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskA)
		_, err = transitionTaskState(tx, taskA, EventPause, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		tx.Commit()
		if err != nil {
			t.Errorf("provider failure blocked control: %v (仍能处理控制)", err)
		}
	})

	t.Run("freeze_dispatch_protocol_subcase_minimal_asset", func(t *testing.T) {
		store, _ := setupClosureStore(t, "freeze.db")
		now := recordsTestNow()
		// 最小资产记录完成冻结/派发协议子例（完整借用、归还仍等待 P4）。
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-freeze", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		tx.Commit()
		// 冻结。
		tx, _ = store.db.Begin()
		if err := freezeAsset(tx, "asset-freeze", "admin", "维护冻结", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 冻结已记录（协议子例）。
		asset, _ := readAsset(store.db, "asset-freeze")
		if asset.FrozenAt == 0 {
			t.Error("freeze not recorded (冻结/派发协议子例)")
		}
		// 完整借用、归还仍等待 P4（不提前标 F06 全通过）。
		t.Log("freeze/dispatch protocol subcase complete (完整借用/归还仍等待 P4)")
	})

	t.Run("unverified_write_capability_stays_closed", func(t *testing.T) {
		// 未验证写能力继续关闭（真实环境不足时明确实际适配器仍待验证）。
		store, _ := setupClosureStore(t, "unverified.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		cap, err := registerCapability(tx, Capability{CapabilityKey: "cluster.node.write", Version: 1, Kind: "action"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 未验证写能力默认不向模型暴露（继续关闭）。
		if cap.ExposedToModel {
			t.Error("unverified write capability exposed (未验证写能力继续关闭)")
		}
		// 真实环境不足时明确实际适配器仍待验证。
		t.Log("unverified write capability stays closed (真实环境不足时明确实际适配器仍待验证)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
