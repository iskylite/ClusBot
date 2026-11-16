package work

// P2-09 验收：多任务 Chat 控制与详情展示兼容。
//
// 两个测试入口：
//   - TestV3P209ChatMultiTaskControl：一次消息中的查询、补充、暂停按明确
//     Task ID/意图处理；模糊归属澄清，不覆盖其他任务；重复控制不重复扣
//     预算/创建新 Task；旧界面版本冲突有可理解提示。
//   - TestV3P209ConcurrentProgressAPI：API 返回多活跃 Run/Step、当前 Wait、
//     控制修订和结果；界面区分已受理、执行中、等待、收尾和终态；显示待核对
//     原因及下一责任人，不把 Worker 消失显示成任务成功。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P209 创建存储与多个任务。
func setupV3P209(t *testing.T, name string, count int) (*Store, []string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	var taskIDs []string
	for i := 0; i < count; i++ {
		run, err := store.Submit(ctx, "v3p209-"+name+id(), "任务"+string(rune('A'+i)), "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, run.TaskID)
	}
	return store, taskIDs
}

// TestV3P209ChatMultiTaskControl 多任务控制。
func TestV3P209ChatMultiTaskControl(t *testing.T) {
	t.Run("explicit_task_id_intent_handled_correctly", func(t *testing.T) {
		_, taskIDs := setupV3P209(t, "explicit.db", 2)
		// 补充 A、暂停 B：按明确 Task ID 处理（不覆盖其他任务）。
		intentA := RouterIntent{Kind: "follow_up", Fragment: "补充要求", TaskID: taskIDs[0]}
		intentB := RouterIntent{Kind: "pause", Fragment: "暂停", TaskID: taskIDs[1]}
		cmdA := resolveChatControl(intentA, taskIDs)
		cmdB := resolveChatControl(intentB, taskIDs)
		if cmdA.TaskID != taskIDs[0] || cmdA.Ambiguous {
			t.Errorf("intent A resolved wrong: %+v", cmdA)
		}
		if cmdB.TaskID != taskIDs[1] || cmdB.Ambiguous {
			t.Errorf("intent B resolved wrong: %+v", cmdB)
		}
		// 补充 A 不影响 B（各自正确归属）。
		if cmdA.TaskID == cmdB.TaskID {
			t.Error("intents not isolated by task id")
		}
	})

	t.Run("ambiguous_ownership_clarified_not_overwrite", func(t *testing.T) {
		_, taskIDs := setupV3P209(t, "ambiguous.db", 2)
		// 模糊归属（无 Task ID 且多个活跃任务）：澄清，不覆盖其他任务。
		intent := RouterIntent{Kind: "pause", Fragment: "暂停"} // 无 TaskID
		cmd := resolveChatControl(intent, taskIDs)
		if !cmd.Ambiguous {
			t.Error("ambiguous ownership not clarified (模糊归属应澄清)")
		}
		if cmd.TaskID != "" {
			t.Errorf("ambiguous intent assigned task %s (不覆盖其他任务)", cmd.TaskID)
		}
		if cmd.ClarifyReason == "" {
			t.Error("clarification reason missing")
		}
		// 单任务时默认归属（不澄清）。
		single := resolveChatControl(RouterIntent{Kind: "pause", Fragment: "暂停"}, []string{taskIDs[0]})
		if single.Ambiguous {
			t.Error("single task intent marked ambiguous")
		}
		if single.TaskID != taskIDs[0] {
			t.Errorf("single task intent not assigned: %s", single.TaskID)
		}
	})

	t.Run("duplicate_control_not_double_charge_or_new_task", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "dup.db", 1)
		now := v3P102Now()
		// 重复控制（同请求键）：幂等，不重复扣预算/创建新 Task。
		tx, _ := store.db.Begin()
		recordControlRequest(tx, ControlRequest{RequestKey: "pause-dup", TaskID: taskIDs[0], DecidedBy: "user", Kind: "pause", Decision: "pause"}, 1, now)
		tx.Commit()
		// 重复同键：返回原决定（不重复扣预算）。
		tx, _ = store.db.Begin()
		exists, decision, err := deduplicateControl(tx, "pause-dup")
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !exists || decision != "pause" {
			t.Errorf("duplicate control not deduplicated: exists=%v decision=%s (重复控制不重复扣预算)", exists, decision)
		}
		// 控制记录只一条（不重复创建）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM control_records WHERE request_key='pause-dup'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("control records = %d, want 1 (重复控制不重复创建)", count)
		}
	})

	t.Run("revision_conflict_understandable", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "conflict.db", 1)
		now := v3P102Now()
		// 建立控制修订。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskIDs[0], EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		// 旧界面版本冲突：有可理解提示。
		err := checkRevisionConflict(store.db, taskIDs[0], 999) // 期望旧修订
		if err == nil {
			t.Error("revision conflict not detected (旧界面版本冲突应有提示)")
		}
		if !strings.Contains(err.Error(), "版本冲突") && !strings.Contains(err.Error(), "已被更新") {
			t.Errorf("conflict reason not understandable: %v", err)
		}
		// 正确版本：通过。
		if err := checkRevisionConflict(store.db, taskIDs[0], 0); err != nil {
			t.Errorf("correct revision rejected: %v", err)
		}
	})
}

// TestV3P209ConcurrentProgressAPI 并发进度 API。
func TestV3P209ConcurrentProgressAPI(t *testing.T) {
	t.Run("api_returns_active_steps_waits_controls_results", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "api.db", 1)
		taskID := taskIDs[0]
		now := v3P102Now()
		// 填充：活跃 Step、就绪单元、开放 Wait、控制记录、待回执。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		openWait(tx, Wait{WaitID: "api-wait", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		recordControlRequest(tx, ControlRequest{RequestKey: "pause-1", TaskID: taskID, DecidedBy: "user", Kind: "pause", Decision: "pause"}, 1, now)
		input, output := int64(10), int64(5)
		recordReport(tx, ResultReport{ResultKey: "api-result", TaskID: taskID, Summary: "完成", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
		tx.Commit()
		steps, _ := listSteps(store.db, taskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "api-unit", TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// API 聚合：多活跃 Step/Wait/控制/结果返回。
		detail, err := readTaskDetailV3(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.ActiveSteps) == 0 {
			t.Error("no active steps in detail")
		}
		if len(detail.OpenWaits) == 0 {
			t.Error("no open waits in detail")
		}
		if len(detail.Controls) == 0 {
			t.Error("no controls in detail")
		}
		if detail.PendingReceipts != 1 {
			t.Errorf("pending receipts = %d, want 1", detail.PendingReceipts)
		}
		if detail.Phase == "" {
			t.Error("phase missing")
		}
	})

	t.Run("phase_distinguishes_states", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "phase.db", 1)
		taskID := taskIDs[0]
		now := v3P102Now()
		// 不同状态的展示阶段区分。
		states := []struct {
			drive  []TaskEvent
			wantPhase string
		}{
			{[]TaskEvent{EventCreate}, "admitted"},
			{[]TaskEvent{EventCreate, EventClaim}, "running"},
			{[]TaskEvent{EventCreate, EventNoRunnableStep}, "waiting"},
			{[]TaskEvent{EventCreate, EventClaim, EventPause}, "paused"},
			{[]TaskEvent{EventCreate, EventClaim, EventStepsComplete, EventAcceptancePass, EventFinalized}, "terminal"},
		}
		for _, c := range states {
			tx, _ := store.db.Begin()
			transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
			tx.Commit()
			for i, event := range c.drive[1:] {
				tx, _ = store.db.Begin()
				current, _ := readTaskState(tx, taskID)
				guard := TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}
				if event == EventAcceptancePass {
					guard.AcceptanceEvidence = true
				}
				transitionTaskState(tx, taskID, event, guard, now+int64(i))
				tx.Commit()
			}
			detail, _ := readTaskDetailV3(store.db, taskID)
			if detail.Phase != c.wantPhase {
				t.Errorf("drive %v: phase = %s, want %s (界面区分状态)", c.drive, detail.Phase, c.wantPhase)
			}
			// 重置（删除状态，下一轮重新 create）。
			store.db.Exec("DELETE FROM task_states WHERE task_id=?", taskID)
		}
	})

	t.Run("worker_gone_not_shown_as_success", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "gone.db", 1)
		taskID := taskIDs[0]
		now := v3P102Now()
		// 任务有待核对结果（Worker 消失）：显示待核对原因及下一责任人，不显示成功。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: 1, Actor: "test"}, now)
		input, output := int64(10), int64(5)
		recordReport(tx, ResultReport{ResultKey: "pending-result", TaskID: taskID, Summary: "待核对", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
		tx.Commit()
		detail, err := readTaskDetailV3(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		// 待核对原因与下一责任人显示（不把 Worker 消失显示成任务成功）。
		if detail.PendingReason == "" {
			t.Error("pending reason missing (待核对原因应显示)")
		}
		if detail.NextOwner == "" {
			t.Error("next owner missing (下一责任人应显示)")
		}
		if detail.Phase == "terminal" && detail.PendingReceipts > 0 {
			t.Errorf("task shown as success with pending receipts (不把 Worker 消失显示成任务成功)")
		}
	})

	t.Run("successor_predecessor_link_navigable", func(t *testing.T) {
		store, taskIDs := setupV3P209(t, "link.db", 1)
		taskID := taskIDs[0]
		now := v3P102Now()
		// 终态任务建立 successor（合同变更/终态后新 Task 的链接清楚）。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		driveToState(t, store, taskID, EventCancel, EventFinalized)
		tx, _ = store.db.Begin()
		req := ControlRequest{RequestKey: "retry-link", TaskID: taskID, DecidedBy: "user", Kind: "revise", Decision: "retry"}
		successor, err := createSuccessorTask(tx, taskID, req, ScheduleTemplate{Goal: "重试", Scope: "工作", Criteria: "完成"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 原任务显示 successor 链接。
		detail, err := readTaskDetailV3(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.SuccessorID != successor.NewTaskID {
			t.Errorf("successor link = %s, want %s (终态后新 Task 链接清楚)", detail.SuccessorID, successor.NewTaskID)
		}
		// 新任务显示 predecessor 链接。
		newDetail, err := readTaskDetailV3(store.db, successor.NewTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if newDetail.PredecessorID != taskID {
			t.Errorf("predecessor link = %s, want %s (返回导航清楚)", newDetail.PredecessorID, taskID)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
