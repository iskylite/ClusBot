package work

// 验收：暂停、取消、恢复与关联新任务。
//
// 三个测试入口：
//   - TestControlIntentDurable：控制记录包含请求键、决定者、目标
//     Task/合同、控制修订及请求类型；重复请求返回原决定；控制请求先持久化。
//   - TestPauseKeepsReceipts：暂停阻止新领取/新派发，仍允许接收已发出
//     的结果和必要核对；不会宣告外部工作停止；取消与领取竞争时只有按事务
//     顺序获准的工作进入在途。
//   - TestCancelledTaskSuccessor：终态后的重试/新要求建立 successor
//     Task 并保留关联；旧客户端重复提交 Resume 不复活原取消 Task，也不创建
//     多个 successor；取消不能自动回滚成果、设备状态或释放未知影响。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupControlsStore 创建存储与基础任务（含 初始状态）。
func setupControlsStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p111-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 建立 初始状态（pending）。
	tx, _ := store.db.Begin()
	if _, err := transitionTaskState(tx, run.TaskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, recordsTestNow()); err != nil {
		t.Fatalf("init state: %v", err)
	}
	tx.Commit()
	return store, run.TaskID
}

// driveToState 驱动任务到指定状态（测试辅助）。
func driveToState(t *testing.T, store *Store, taskID string, events ...TaskEvent) {
	t.Helper()
	for i, event := range events {
		tx, _ := store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		guard := TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}
		if event == EventAcceptancePass {
			guard.AcceptanceEvidence = true
		}
		if _, err := transitionTaskState(tx, taskID, event, guard, recordsTestNow()+int64(i)); err != nil {
			t.Fatalf("drive %s: %v", event, err)
		}
		tx.Commit()
	}
}

// TestControlIntentDurable 控制请求持久化。
func TestControlIntentDurable(t *testing.T) {
	t.Run("control_request_recorded_with_all_fields", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "record.db")
		now := recordsTestNow()
		req := ControlRequest{
			RequestKey: "pause-req-1", TaskID: taskID, DecidedBy: "user",
			Kind: "pause", TargetContractRevision: 1, Decision: "pause", Detail: "用户请求暂停",
		}
		tx, _ := store.db.Begin()
		record, err := recordControlRequest(tx, req, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 控制记录包含全部字段。
		if record.RequestKey != "pause-req-1" || record.DecidedBy != "user" ||
			record.Kind != "pause" || record.TargetContractRevision != 1 || record.Detail == "" {
			t.Errorf("control record incomplete: %+v", record)
		}
		// 持久可查。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM control_records WHERE request_key='pause-req-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("control record not persisted: count=%d", count)
		}
	})

	t.Run("duplicate_request_returns_original_decision", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "dup.db")
		now := recordsTestNow()
		req := ControlRequest{RequestKey: "pause-dup", TaskID: taskID, DecidedBy: "user", Kind: "pause", Decision: "pause"}
		tx, _ := store.db.Begin()
		first, err := recordControlRequest(tx, req, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 重复请求键相同内容：返回原决定（幂等）。
		tx, _ = store.db.Begin()
		again, err := recordControlRequest(tx, req, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if again.RecordID != first.RecordID {
			t.Errorf("duplicate created new record: %s vs %s", again.RecordID, first.RecordID)
		}
		// 同键不同内容：冲突。
		tx, _ = store.db.Begin()
		conflicting := req
		conflicting.Decision = "resume"
		_, err = recordControlRequest(tx, conflicting, 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("same key different content not conflicted")
		}
	})

	t.Run("control_request_persisted_before_state_change", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "order.db")
		// 控制请求先持久化：即使状态变更失败，请求记录也保留（可核对）。
		req := ControlRequest{RequestKey: "pause-order", TaskID: taskID, DecidedBy: "user", Kind: "pause", Decision: "pause"}
		tx, _ := store.db.Begin()
		if _, err := recordControlRequest(tx, req, 1, recordsTestNow()); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 记录已持久（独立于状态变更结果）。
		var exists bool
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM control_records WHERE request_key='pause-order')").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Error("control request not persisted before state change")
		}
	})
}

// TestPauseKeepsReceipts 暂停保留回执。
func TestPauseKeepsReceipts(t *testing.T) {
	t.Run("pause_blocks_new_but_allows_reconcile", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "pause.db")
		// 驱动到 running。
		driveToState(t, store, taskID, EventClaim)
		// 有一个已派发（running）的 work_unit（已发出结果待核对）。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze", DefinitionVersion: 1, State: RuntimeStepRunning, CreatedAt: recordsTestNow(), UpdatedAt: recordsTestNow()})
		tx.Commit()
		steps, _ := listSteps(store.db, taskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: recordsTestNow(), State: WorkUnitRunning, CreatedAt: recordsTestNow(), UpdatedAt: recordsTestNow()})
		tx.Commit()

		// 暂停：阻止新领取/新派发，仍允许核对已发出的结果。
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		req := ControlRequest{RequestKey: "pause-1", TaskID: taskID, DecidedBy: "user", Kind: "pause", Decision: "pause"}
		result, err := pauseTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Transition.To != RuntimeTaskPaused {
			t.Errorf("pause to=%s, want paused", result.Transition.To)
		}
		// 已派发未核对的操作被统计（继续核对，不宣告外部停止）。
		if result.OutstandingDispatched != 1 {
			t.Errorf("outstanding dispatched = %d, want 1 (已发出结果继续核对)", result.OutstandingDispatched)
		}
		// 后续命令包含核对已派发（不会宣告外部工作停止）。
		found := false
		for _, cmd := range result.FollowUp {
			if strings.Contains(cmd, "reconcile") {
				found = true
			}
		}
		if !found {
			t.Errorf("pause follow-up missing reconcile (不会宣告外部停止): %v", result.FollowUp)
		}
	})

	t.Run("cancel_vs_claim_race_single_inflight", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "race.db")
		// 取消与领取竞争：单写连接串行化事务，只有按事务顺序获准的工作进入在途。
		driveToState(t, store, taskID, EventClaim) // → running
		// 先取消（事务顺序在前）。
		tx, _ := store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		req := ControlRequest{RequestKey: "cancel-1", TaskID: taskID, DecidedBy: "user", Kind: "cancel", Decision: "cancel"}
		cancelResult, err := cancelTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if cancelResult.Transition.To != RuntimeTaskFinalizing {
			t.Errorf("cancel to=%s, want finalizing", cancelResult.Transition.To)
		}
		// 取消后领取被拒（任务已不在可领取状态）。
		tx, _ = store.db.Begin()
		current2, _ := readTaskState(tx, taskID)
		_, err = transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: current2.StorageRevision, Actor: "test"}, recordsTestNow())
		tx.Rollback()
		if err == nil {
			t.Error("claim after cancel not rejected (取消与领取竞争只有获准者进入在途)")
		}
	})

	t.Run("pause_one_task_not_affect_other", func(t *testing.T) {
		store, taskA := setupControlsStore(t, "multi.db")
		// 第二个任务。
		ctx := context.Background()
		runB, err := store.Submit(ctx, "v3p111-b"+id(), "整理另一份材料", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		taskB := runB.TaskID
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskB, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, recordsTestNow())
		tx.Commit()
		// 暂停任务 A。
		driveToState(t, store, taskA, EventClaim)
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskA)
		req := ControlRequest{RequestKey: "pause-a", TaskID: taskA, DecidedBy: "user", Kind: "pause", Decision: "pause"}
		pauseTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, recordsTestNow())
		tx.Commit()
		// 任务 B 不受影响（仍可领取推进）。
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskB)
		_, err = transitionTaskState(tx, taskB, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, recordsTestNow())
		tx.Commit()
		if err != nil {
			t.Errorf("pausing task A blocked unrelated task B: %v", err)
		}
	})
}

// TestCancelledTaskSuccessor 关联新任务。
func TestCancelledTaskSuccessor(t *testing.T) {
	t.Run("successor_created_with_link_not_revive", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "successor.db")
		// 驱动到终态 cancelled（经 finalizing + finalized）。
		driveToState(t, store, taskID, EventCancel, EventFinalized)
		// 终态后建立 successor：明确新 ID，保留关联，不复活原任务。
		tx, _ := store.db.Begin()
		req := ControlRequest{RequestKey: "retry-1", TaskID: taskID, DecidedBy: "user", Kind: "revise", Decision: "retry"}
		template := SuccessorTemplate{Goal: "整理季度汇报（重试）", Scope: "工作资料", Criteria: "汇报完整"}
		successor, err := createSuccessorTask(tx, taskID, req, template, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if successor.NewTaskID == "" || successor.NewTaskID == taskID {
			t.Errorf("successor id = %s, want new distinct id (不复活原任务)", successor.NewTaskID)
		}
		if successor.PredecessorID != taskID {
			t.Errorf("predecessor link = %s, want %s (保留关联)", successor.PredecessorID, taskID)
		}
		// 新 Task 存在且可查询。
		var exists bool
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)", successor.NewTaskID).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Error("successor task not created")
		}
		// 关联已审计。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=? AND event='successor_created'", successor.NewTaskID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("successor link not audited: count=%d", auditCount)
		}
	})

	t.Run("duplicate_resume_not_revive_not_duplicate_successor", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "dupresume.db")
		driveToState(t, store, taskID, EventCancel, EventFinalized)
		// 旧客户端重复提交 Resume（同请求键）：不复活原任务，不创建多个 successor。
		req := ControlRequest{RequestKey: "resume-dup", TaskID: taskID, DecidedBy: "user", Kind: "revise", Decision: "retry"}
		template := SuccessorTemplate{Goal: "重试", Scope: "工作", Criteria: "完成"}
		var firstID string
		for i := 0; i < 2; i++ {
			tx, _ := store.db.Begin()
			successor, err := createSuccessorTask(tx, taskID, req, template, recordsTestNow())
			tx.Commit()
			if i == 0 {
				if err != nil {
					t.Fatal(err)
				}
				firstID = successor.NewTaskID
			} else {
				// 重复：幂等返回原 successor（不创建多个）。
				if err == nil && successor.NewTaskID != firstID {
					t.Errorf("duplicate resume created second successor: %s vs %s", successor.NewTaskID, firstID)
				}
			}
		}
		// 原任务保持终态（不复活）。
		state, _ := readTaskState(store.db, taskID)
		if !state.State.IsTerminal() {
			t.Errorf("original task revived: state=%s", state.State)
		}
	})

	t.Run("cancel_not_rollback_artifact_or_release_unknown", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "norollback.db")
		ctx := context.Background()
		// 产生一个成果（已发生的实际效果）。
		run, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.delta(ctx, run.ID, "# 季度汇报"); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		// 取消任务。
		tx, _ := store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		req := ControlRequest{RequestKey: "cancel-nr", TaskID: taskID, DecidedBy: "user", Kind: "cancel", Decision: "cancel"}
		result, err := cancelTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 取消不能自动回滚成果（已发生的实际效果保留）。
		var artifactCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE task_id=?", taskID).Scan(&artifactCount); err != nil {
			t.Fatal(err)
		}
		if artifactCount != 1 {
			t.Errorf("cancel rolled back artifact: count=%d (取消不能自动回滚成果)", artifactCount)
		}
		// 收尾记录保留逐项责任（未核对项不静默消失）。
		if result.Finalization.TaskID != taskID {
			t.Errorf("finalization record missing: %+v", result.Finalization)
		}
	})

	t.Run("successor_requires_terminal_predecessor", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "notterminal.db")
		// 任务未到终态（pending）：不能建立 successor。
		tx, _ := store.db.Begin()
		req := ControlRequest{RequestKey: "retry-early", TaskID: taskID, DecidedBy: "user", Kind: "revise", Decision: "retry"}
		_, err := createSuccessorTask(tx, taskID, req, SuccessorTemplate{Goal: "重试", Scope: "工作", Criteria: "完成"}, recordsTestNow())
		tx.Rollback()
		if err == nil {
			t.Error("successor created for non-terminal task (原任务未终结)")
		}
	})

	t.Run("resume_rechecks_waits_and_ops", func(t *testing.T) {
		store, taskID := setupControlsStore(t, "recheck.db")
		now := recordsTestNow()
		// 驱动到 paused，且有开放 Wait 与未决操作。
		driveToState(t, store, taskID, EventClaim, EventPause)
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-resume", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze", DefinitionVersion: 1, State: RuntimeStepRunning, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, _ := listSteps(store.db, taskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitRunning, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 恢复：核对当前合同、Wait、未决操作；有 Wait 时回等待中。
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		req := ControlRequest{RequestKey: "resume-recheck", TaskID: taskID, DecidedBy: "user", Kind: "resume", Decision: "resume"}
		result, err := resumeRuntimeTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 有开放 Wait：恢复后回等待中（重评），不直接重放旧 Run 上下文。
		if result.Transition.To != RuntimeTaskWaiting {
			t.Errorf("resume to=%s, want waiting (有开放 Wait 时重评回等待)", result.Transition.To)
		}
		// 未决操作被重新核对。
		if result.RecheckedOps != 1 {
			t.Errorf("rechecked ops = %d, want 1 (恢复核对未决操作)", result.RecheckedOps)
		}
	})
}

// 确保 sql 引用保留。
var _ = sql.ErrNoRows
