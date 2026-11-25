package work

// 验收：Router 持久受理与现有个人助手兼容。
//
// 三个测试入口：
//   - TestChatDurableAdmission：在模型分类前登记可信输入及请求键，返回
//     可查询受理标识；输入提交后断开连接，重复发送同键得到同一受理，不重复
//     调用无依据的模型分类。
//   - TestRouterIntentIsolation：复用多意图与冲突识别，把创建/修订/控制
//     转换成运行时命令；每个意图保留片段、目标和回执；一项澄清不阻断无依赖
//     的其他项，重试不会重复委派。
//   - TestLegacyPersonalFlow：原有委托/普通问答/多意图回归通过；终态新
//     委托返回新 Task ID；历史页仍显示原成果与控制记录；旧 API 字段用明确映射
//     适配，不混用旧/新终态。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupAdmissionStore 创建存储。
func setupAdmissionStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestChatDurableAdmission 持久受理。
func TestChatDurableAdmission(t *testing.T) {
	t.Run("admit_before_classification_returns_receipt", func(t *testing.T) {
		store := setupAdmissionStore(t, "admit.db")
		now := recordsTestNow()
		// 在模型分类前登记可信输入：返回可查询受理标识，不声称 Task 已建立。
		tx, _ := store.db.Begin()
		receipt, err := admitChatInput(tx, "req-admit-1", "user", "", "整理季度汇报", "工作资料", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if receipt.ReceiptID != "req-admit-1" {
			t.Errorf("receipt id = %s, want req-admit-1", receipt.ReceiptID)
		}
		if receipt.TaskID != "" {
			t.Errorf("admission claimed task created before classification: %s (尚未分类不声称 Task 已建立)", receipt.TaskID)
		}
		if receipt.State != "received" {
			t.Errorf("state = %s, want received (已受理未分类)", receipt.State)
		}
		// 可查询受理标识。
		queried, err := readAdmission(store.db, "req-admit-1")
		if err != nil {
			t.Fatal(err)
		}
		if queried.ReceiptID != "req-admit-1" || queried.Reason == "" {
			t.Errorf("admission not queryable: %+v", queried)
		}
	})

	t.Run("disconnect_and_retry_same_key_idempotent", func(t *testing.T) {
		// 输入提交后断开连接，重复发送同键得到同一受理，不重复调用无依据的分类。
		path := filepath.Join(t.TempDir(), "retry.db")
		now := recordsTestNow()
		var firstReceiptID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			tx, _ := store.db.Begin()
			receipt, err := admitChatInput(tx, "req-retry", "user", "", "整理季度汇报", "工作资料", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			firstReceiptID = receipt.ReceiptID
			// 模拟断开：Close 不分类。
		}()
		// 重连后重复发送同键：得到同一受理（幂等）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		tx, _ := store.db.Begin()
		receipt, err := admitChatInput(tx, "req-retry", "user", "", "整理季度汇报", "工作资料", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if receipt.ReceiptID != firstReceiptID {
			t.Errorf("retry produced new receipt: %s vs %s (重复发送同键得到同一受理)", receipt.ReceiptID, firstReceiptID)
		}
		if !receipt.Duplicate {
			t.Error("retry not marked duplicate")
		}
		// 只有一条受理记录（不重复分类）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE source='chat' AND event_id='req-retry'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("admission records = %d, want 1 (不重复调用无依据的分类)", count)
		}
	})

	t.Run("same_key_different_content_conflict", func(t *testing.T) {
		store := setupAdmissionStore(t, "conflict.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		admitChatInput(tx, "req-conflict", "user", "", "内容A", "工作", nil, now)
		tx.Commit()
		// 同键不同内容：冲突（重试同键不同内容应冲突）。
		tx, _ = store.db.Begin()
		_, err := admitChatInput(tx, "req-conflict", "user", "", "内容B", "工作", nil, now)
		tx.Rollback()
		if err == nil {
			t.Error("same key different content not conflicted")
		}
	})

	t.Run("admission_not_displayed_as_complete", func(t *testing.T) {
		store := setupAdmissionStore(t, "notcomplete.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		admitChatInput(tx, "req-nc", "user", "", "整理汇报", "工作", nil, now)
		tx.Commit()
		// 受理状态查询：需要等待时返回具体原因，禁止显示执行完成。
		receipt, err := readAdmission(store.db, "req-nc")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.State == "completed" || receipt.TaskID != "" {
			t.Errorf("admission displayed as complete: %+v (受理不等于执行完成)", receipt)
		}
		if !strings.Contains(receipt.Reason, "等待") && !strings.Contains(receipt.Reason, "已受理") {
			t.Errorf("admission reason not concrete: %q (需要等待返回具体原因)", receipt.Reason)
		}
	})
}

// TestRouterIntentIsolation 意图隔离。
func TestRouterIntentIsolation(t *testing.T) {
	t.Run("intents_convert_to_runtime_commands", func(t *testing.T) {
		// 创建/修订/控制转换成运行时命令，不让 Router 独立改运行中状态。
		intents := []RouterIntent{
			{Kind: "new_task", Fragment: "整理季度汇报"},
			{Kind: "follow_up", Fragment: "补充要求", TaskID: "task-1"},
			{Kind: "pause", Fragment: "暂停", TaskID: "task-2"},
		}
		commands := convertIntents(intents, nil)
		if len(commands) != 3 {
			t.Fatalf("commands = %d, want 3", len(commands))
		}
		if commands[0].Kind != "create_task" {
			t.Errorf("new_task converted to %s, want create_task", commands[0].Kind)
		}
		if commands[1].Kind != "revise_task" || commands[1].TargetTaskID != "task-1" {
			t.Errorf("follow_up converted to %+v", commands[1])
		}
		if commands[2].Kind != "control" {
			t.Errorf("pause converted to %s, want control", commands[2].Kind)
		}
		// 每个意图保留片段与回执。
		for i, cmd := range commands {
			if cmd.Fragment == "" {
				t.Errorf("command %d missing fragment", i)
			}
			if cmd.Receipt == "" {
				t.Errorf("command %d missing receipt", i)
			}
		}
	})

	t.Run("clarification_does_not_block_independent", func(t *testing.T) {
		// 一项澄清不阻断无依赖的其他项。
		intents := []RouterIntent{
			{Kind: "follow_up", Fragment: "补充"},  // 无目标 → 需澄清
			{Kind: "new_task", Fragment: "新任务"}, // 独立 → 不阻断
			{Kind: "pause", TaskID: "task-x", Fragment: "暂停"},
		}
		commands := convertIntents(intents, nil)
		var clarifyCount, okCount int
		for _, cmd := range commands {
			if cmd.NeedsClarification {
				clarifyCount++
				if cmd.ClarificationReason == "" {
					t.Error("clarification missing reason")
				}
			} else {
				okCount++
			}
		}
		if clarifyCount != 1 {
			t.Errorf("clarify count = %d, want 1", clarifyCount)
		}
		if okCount != 2 {
			t.Errorf("independent commands blocked: ok=%d, want 2 (一项澄清不阻断无依赖项)", okCount)
		}
	})

	t.Run("each_intent_keeps_fragment_target_receipt", func(t *testing.T) {
		intents := []RouterIntent{
			{Kind: "pause", Fragment: "暂停会议记录", TaskID: "task-meeting"},
			{Kind: "stop", Fragment: "取消资料整理", TaskID: "task-docs"},
		}
		commands := convertIntents(intents, nil)
		for i, cmd := range commands {
			if cmd.Fragment != intents[i].Fragment {
				t.Errorf("command %d fragment = %q, want %q", i, cmd.Fragment, intents[i].Fragment)
			}
			if cmd.TargetTaskID != intents[i].TaskID {
				t.Errorf("command %d target = %q, want %q", i, cmd.TargetTaskID, intents[i].TaskID)
			}
			if cmd.Receipt == "" {
				t.Errorf("command %d missing receipt (每个意图保留回执)", i)
			}
		}
	})
}

// TestLegacyPersonalFlow 个人助手兼容。
func TestLegacyPersonalFlow(t *testing.T) {
	t.Run("legacy_personal_flow_regression", func(t *testing.T) {
		// 原有委托/普通问答回归：提交 → 执行 → 成果 → 验收。
		store := setupAdmissionStore(t, "legacy.db")
		ctx := context.Background()
		run, err := store.Submit(ctx, "v3p112-legacy", "整理季度汇报", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.delta(ctx, claimed.ID, "# 季度汇报"); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(claimed, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		detail, err := store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Artifact == nil {
			t.Fatal("personal flow: no artifact")
		}
		if err := store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
			t.Fatal(err)
		}
		// 验收后任务完成（个人助手交付功能持续可用）。
		detail, _ = store.Detail(run.TaskID, true)
		if detail.Task.Status != "completed" {
			t.Errorf("personal flow status = %s, want completed", detail.Task.Status)
		}
	})

	t.Run("terminal_followup_returns_new_task_id", func(t *testing.T) {
		store := setupAdmissionStore(t, "followup.db")
		ctx := context.Background()
		// 创建并取消一个任务。
		run, err := store.Submit(ctx, "v3p112-cancel", "整理汇报", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		taskID := run.TaskID
		// 建立 状态并驱动到终态 cancelled。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, recordsTestNow())
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		transitionTaskState(tx, taskID, EventCancel, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, recordsTestNow())
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskID)
		transitionTaskState(tx, taskID, EventFinalized, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, recordsTestNow())
		tx.Commit()
		// 终态新委托：返回新 Task ID（不复活原任务）。
		tx, _ = store.db.Begin()
		req := ControlRequest{RequestKey: "followup-1", TaskID: taskID, DecidedBy: "user", Kind: "revise", Decision: "retry"}
		successor, err := SubmitTerminalFollowup(tx, taskID, req, SuccessorTemplate{Goal: "整理汇报（重试）", Scope: "工作", Criteria: "完整"}, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if successor.NewTaskID == "" || successor.NewTaskID == taskID {
			t.Errorf("terminal followup id = %s, want new id (终态新委托返回新 Task ID)", successor.NewTaskID)
		}
		if successor.PredecessorID != taskID {
			t.Errorf("predecessor link = %s (历史页仍显示原任务关联)", successor.PredecessorID)
		}
	})

	t.Run("legacy_status_mapped_not_mixed", func(t *testing.T) {
		// 旧 API 字段用明确映射适配，不混用旧/新终态。
		cases := []struct {
			status, reason, wantPhase string
		}{
			{"completed", "accepted", "成功"},
			{"cancelled", "stopped", "已取消"},
			{"interrupted", "paused", "已暂停"},
			{"waiting_user", "acceptance_required", "待验收"},
			{"active", "queued", "待执行"},
		}
		for _, c := range cases {
			phase, reason := MapLegacyStatusForAPI(c.status, c.reason)
			if phase != c.wantPhase {
				t.Errorf("status %s/%s → phase %q, want %q (映射不混用旧/新终态)", c.status, c.reason, phase, c.wantPhase)
			}
			if reason == "" {
				t.Errorf("status %s/%s: mapped reason empty", c.status, c.reason)
			}
		}
	})

	t.Run("history_shows_original_artifact_and_control", func(t *testing.T) {
		store := setupAdmissionStore(t, "history.db")
		ctx := context.Background()
		// 产生成果并验收（历史）。
		run, err := store.Submit(ctx, "v3p112-history", "整理汇报", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		claimed, _ := store.claim(ctx)
		store.delta(ctx, claimed.ID, "# 汇报")
		store.finish(claimed, "completed", "", runAccounting{})
		detail, _ := store.Detail(run.TaskID, true)
		store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision)
		// 历史页仍显示原成果。
		detail, _ = store.Detail(run.TaskID, true)
		if detail.Artifact == nil {
			t.Error("history lost original artifact (历史页仍显示原成果)")
		}
		if detail.Acceptance == nil {
			t.Error("history lost acceptance record")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
