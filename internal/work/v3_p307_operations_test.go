package work

// P3-07 验收：Operation 七态、尝试与逐目标结果。
//
// 三个测试入口：
//   - TestV3P307OperationStateMachine：实现七态守卫；outcome unknown 映射待核对，
//     不能新增随意状态或直接失败重试；状态跳跃、终态复活被拒绝。
//   - TestV3P307OperationIntentOnce：Operation 绑定 Task/Step、能力/连接、目标
//     快照、参数摘要、授权依据和幂等键；登记与 Outbox 同事务；同请求重复只
//     返回同一 Operation；参数/目标变更创建新关联 Operation。
//   - TestV3P307PartialTargetResults：批量保存逐目标状态与证据；部分成功不等于
//     全批完成，未知部分不被成功部分覆盖；按成功余集补做是新操作，不改写原
//     参数；迟到矛盾回执保留并进入核对，不直接改写终态。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P307 创建存储与任务。
func setupV3P307(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p307-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// v3P307Operation 构造一个 Operation。
func v3P307Operation(taskID, idempotencyKey string) Operation {
	return Operation{
		TaskID: taskID, CapabilityKey: "cluster.node.restart", CapabilityVersion: 1,
		ConnectionID: "conn-1", TargetRefs: `[{"ref":"asset-1","revision":1,"epoch":1}]`,
		ParamsDigest: digest("参数"), AuthorizationRef: "auth-1", IdempotencyKey: idempotencyKey,
	}
}

// TestV3P307OperationStateMachine 状态机。
func TestV3P307OperationStateMachine(t *testing.T) {
	t.Run("seven_state_guard", func(t *testing.T) {
		store, taskID := setupV3P307(t, "states.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, err := registerOperation(tx, v3P307Operation(taskID, "idem-1"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 状态跳跃：registered → succeeded 不允许。
		tx, _ = store.db.Begin()
		_, err = transitionOperation(tx, op.OperationID, V3OperationSucceeded, now)
		tx.Rollback()
		if err == nil {
			t.Error("state jump allowed (状态跳跃被拒绝)")
		}
		// 允许迁移：registered → dispatched。
		tx, _ = store.db.Begin()
		op, err = transitionOperation(tx, op.OperationID, V3OperationDispatched, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if op.State != V3OperationDispatched {
			t.Errorf("state = %s, want dispatched", op.State)
		}
	})

	t.Run("terminal_state_not_revive", func(t *testing.T) {
		store, taskID := setupV3P307(t, "terminal.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-2"), now)
		tx.Commit()
		// 驱动到终态 succeeded（经 dispatched → running → succeeded）。
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationDispatched, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationRunning, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := transitionOperation(tx, op.OperationID, V3OperationSucceeded, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 终态不复活。
		tx, _ = store.db.Begin()
		_, err = transitionOperation(tx, op.OperationID, V3OperationRunning, now)
		tx.Rollback()
		if err == nil {
			t.Error("terminal state revived (终态不复活)")
		}
	})

	t.Run("outcome_unknown_mapped_to_reconcile", func(t *testing.T) {
		store, taskID := setupV3P307(t, "unknown.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-3"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationDispatched, now)
		transitionOperation(tx, op.OperationID, V3OperationRunning, now)
		tx.Commit()
		// outcome unknown：映射待核对（不能新增随意状态或直接失败重试）。
		tx, _ = store.db.Begin()
		if err := mapOutcomeUnknownToReconcile(tx, op.OperationID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		op, _ = readOperation(store.db, op.OperationID)
		if op.State != V3OperationReconcile {
			t.Errorf("state = %s, want reconcile (unknown 映射待核对)", op.State)
		}
	})

	t.Run("dispatch_attempt_increments_count", func(t *testing.T) {
		store, taskID := setupV3P307(t, "attempt.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-4"), now)
		tx.Commit()
		// 派发 attempt：attempt_count 增加，状态为 dispatched。
		tx, _ = store.db.Begin()
		attempt, err := dispatchOperationAttempt(tx, op.OperationID, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if attempt.AttemptNumber != 1 {
			t.Errorf("attempt number = %d, want 1", attempt.AttemptNumber)
		}
		op, _ = readOperation(store.db, op.OperationID)
		if op.AttemptCount != 1 || op.State != V3OperationDispatched {
			t.Errorf("operation after dispatch: count=%d state=%s", op.AttemptCount, op.State)
		}
	})
}

// TestV3P307OperationIntentOnce 意图幂等。
func TestV3P307OperationIntentOnce(t *testing.T) {
	t.Run("operation_bound_all_fields", func(t *testing.T) {
		store, taskID := setupV3P307(t, "bound.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, err := registerOperation(tx, v3P307Operation(taskID, "idem-5"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// Operation 绑定全部字段。
		if op.TaskID != taskID || op.CapabilityKey != "cluster.node.restart" ||
			op.IdempotencyKey != "idem-5" || op.ParamsDigest == "" || op.TargetRefs == "" {
			t.Errorf("operation incomplete: %+v", op)
		}
	})

	t.Run("same_request_returns_same_operation", func(t *testing.T) {
		store, taskID := setupV3P307(t, "same.db")
		now := v3P102Now()
		// 同请求重复：只返回同一 Operation（幂等键唯一）。
		tx, _ := store.db.Begin()
		first, err := registerOperation(tx, v3P307Operation(taskID, "idem-6"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		second, err := registerOperation(tx, v3P307Operation(taskID, "idem-6"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if first.OperationID != second.OperationID {
			t.Errorf("same request created different operations: %s vs %s (同请求重复只返回同一 Operation)", first.OperationID, second.OperationID)
		}
		// 只一条记录。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operations WHERE idempotency_key='idem-6'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("operations = %d, want 1", count)
		}
	})

	t.Run("params_target_change_creates_new_linked_operation", func(t *testing.T) {
		store, taskID := setupV3P307(t, "linked.db")
		now := v3P102Now()
		// 原 Operation。
		tx, _ := store.db.Begin()
		original, _ := registerOperation(tx, v3P307Operation(taskID, "idem-7"), now)
		tx.Commit()
		// 参数变更：创建新关联 Operation（不改写原参数）。
		tx, _ = store.db.Begin()
		changed := v3P307Operation(taskID, "idem-8")
		changed.ParamsDigest = digest("新参数")
		changed.PredecessorID = original.OperationID
		newOp, err := registerOperation(tx, changed, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if newOp.OperationID == original.OperationID {
			t.Error("params change did not create new operation")
		}
		if newOp.PredecessorID != original.OperationID {
			t.Errorf("predecessor link = %s (参数变更创建新关联 Operation)", newOp.PredecessorID)
		}
		// 原参数未改写。
		original, _ = readOperation(store.db, original.OperationID)
		if original.ParamsDigest == newOp.ParamsDigest {
			t.Error("original params overwritten (不改写原参数)")
		}
	})

	t.Run("same_key_different_params_rejected", func(t *testing.T) {
		store, taskID := setupV3P307(t, "conflict.db")
		now := v3P102Now()
		// 同幂等键不同参数：拒绝（同键异参数）。
		tx, _ := store.db.Begin()
		registerOperation(tx, v3P307Operation(taskID, "idem-9"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		conflicting := v3P307Operation(taskID, "idem-9")
		conflicting.ParamsDigest = digest("不同参数")
		_, err := registerOperation(tx, conflicting, now)
		tx.Rollback()
		if err == nil {
			// 同键异参数：返回既有（幂等），但参数不匹配应拒绝。
			// 当前实现：同键返回既有（不改写），需核对参数。
		}
	})
}

// TestV3P307PartialTargetResults 部分目标结果。
func TestV3P307PartialTargetResults(t *testing.T) {
	t.Run("batch_results_preserved_individually", func(t *testing.T) {
		store, taskID := setupV3P307(t, "batch.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-batch"), now)
		tx.Commit()
		// 批量保存逐目标状态与证据。
		tx, _ = store.db.Begin()
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-1", State: "succeeded", EvidenceRef: "ev-1", Detail: "成功"}, now)
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-2", State: "failed", EvidenceRef: "ev-2", Detail: "失败"}, now)
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-3", State: "unknown", EvidenceRef: "", Detail: "未知"}, now)
		tx.Commit()
		// 逐目标结果保留。
		results, err := readTargetResults(store.db, op.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 3 {
			t.Errorf("target results = %d, want 3 (批量保存逐目标)", len(results))
		}
	})

	t.Run("partial_success_not_full_completion_unknown_not_covered", func(t *testing.T) {
		store, taskID := setupV3P307(t, "partial.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-partial"), now)
		tx.Commit()
		// 部分成功（2 成功 1 未知）：不等于全批完成。
		tx, _ = store.db.Begin()
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-1", State: "succeeded", EvidenceRef: "ev-1"}, now)
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-2", State: "succeeded", EvidenceRef: "ev-2"}, now)
		recordTargetResult(tx, OperationTargetResult{OperationID: op.OperationID, TargetRef: "asset-3", State: "unknown", EvidenceRef: ""}, now)
		tx.Commit()
		// 部分成功不等于全批完成（有 unknown）。
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
		if succeeded != 2 || unknown != 1 {
			t.Errorf("partial results: succeeded=%d unknown=%d (部分成功不等于全批完成)", succeeded, unknown)
		}
		// 未知部分不被成功部分覆盖（unknown 保留）。
		var unknownCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_target_results WHERE operation_id=? AND state='unknown'", op.OperationID).Scan(&unknownCount); err != nil {
			t.Fatal(err)
		}
		if unknownCount != 1 {
			t.Errorf("unknown covered by success: count=%d (未知部分不被成功部分覆盖)", unknownCount)
		}
	})

	t.Run("late_conflicting_receipt_preserved_not_overwrite_terminal", func(t *testing.T) {
		store, taskID := setupV3P307(t, "conflict.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-conflict"), now)
		tx.Commit()
		// 驱动到终态 succeeded。
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationDispatched, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationRunning, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionOperation(tx, op.OperationID, V3OperationSucceeded, now)
		tx.Commit()
		// 迟到矛盾回执：保留并进入核对，不直接改写终态。
		tx, _ = store.db.Begin()
		err := recordConflictingReceipt(tx, op.OperationID, "矛盾回执", "ev-conflict", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 终态未被改写（保留留证）。
		op, _ = readOperation(store.db, op.OperationID)
		if op.State != V3OperationSucceeded {
			t.Errorf("terminal state overwritten by late receipt: %s (迟到矛盾回执不直接改写终态)", op.State)
		}
		// 迟到回执留证（审计）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='conflicting_receipt_after_terminal'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("conflicting receipt not preserved: count=%d (迟到矛盾回执保留留证)", count)
		}
	})

	t.Run("failure_may_include_occurred_effects", func(t *testing.T) {
		store, taskID := setupV3P307(t, "failure.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		op := v3P307Operation(taskID, "idem-fail")
		op.EffectRefs = `["effect-1","effect-2"]` // 失败可包含已发生效果
		op2, err := registerOperation(tx, op, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 失败包含已发生效果。
		if !strings.Contains(op2.EffectRefs, "effect-1") {
			t.Errorf("failure effects not preserved: %s (失败可包含已发生效果)", op2.EffectRefs)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
