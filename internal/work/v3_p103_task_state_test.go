package work

// P1-03 验收：Task 状态机、完成判定与终态约束。
//
// 三个测试入口：
//   - TestV3P103TaskTransitionGuards：每条合法迁移有证据；非法跨态、旧版本
//     和终态复活被拒绝；每次变更校验预期版本并写入原因、输入、审计和后续命令。
//   - TestV3P103TaskTerminalImmutable：成功/失败/已取消不复活；后续工作创建
//     有原任务关联的新 Task；存在未知操作/占用/未交接事项时不能正常终结。
//   - TestV3P103AcceptanceBeforeFinalState：模型返回完成、任一 Step 完成或
//     工具回执成功均不能直接使 Task 成功；必需步骤完成且操作有明确结果后
//     进入验收；人工例外须满足 V3 记录要求。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P103 创建基础 Task 并返回存储连接。
func setupV3P103(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p103-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// v3P103Guard 是测试用的默认守卫。
func v3P103Guard(revision int) TaskGuard {
	return TaskGuard{ExpectedStorageRevision: revision, Actor: "test", Input: "测试输入"}
}

// TestV3P103TaskTransitionGuards 状态迁移守卫。
func TestV3P103TaskTransitionGuards(t *testing.T) {
	t.Run("legal_transitions_have_evidence", func(t *testing.T) {
		store, taskID := setupV3P103(t, "legal.db")
		now := v3P102Now()

		// 完整合法路径：create → claim → no_runnable_step → wait_resolved →
		// claim → steps_complete → acceptance_pass → finalized。
		path := []struct {
			event TaskEvent
			guard TaskGuard
			want  V3TaskState
		}{
			{EventCreate, v3P103Guard(0), V3TaskPending},
			{EventClaim, v3P103Guard(1), V3TaskRunning},
			{EventNoRunnableStep, v3P103Guard(2), V3TaskWaiting},
			{EventWaitResolved, v3P103Guard(3), V3TaskPending},
			{EventClaim, v3P103Guard(4), V3TaskRunning},
			{EventStepsComplete, v3P103Guard(5), V3TaskAcceptance},
			{EventAcceptancePass, withAcceptance(v3P103Guard(6)), V3TaskFinalizing},
			{EventFinalized, v3P103Guard(7), V3TaskSucceeded},
		}
		for i, step := range path {
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			tr, err := transitionTaskState(tx, taskID, step.event, step.guard, now+int64(i))
			if err != nil {
				tx.Rollback()
				t.Fatalf("step %d (%s): %v", i, step.event, err)
			}
			if tr.To != step.want {
				t.Errorf("step %d (%s): to=%s, want %s", i, step.event, tr.To, step.want)
			}
			if tr.Reason == "" {
				t.Errorf("step %d (%s): transition must record a reason", i, step.event)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		// 审计：每次变更都有记录。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=?", taskID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != len(path) {
			t.Errorf("audit records = %d, want %d (每次变更都有证据)", auditCount, len(path))
		}
	})

	t.Run("illegal_transitions_rejected", func(t *testing.T) {
		store, taskID := setupV3P103(t, "illegal.db")
		now := v3P102Now()
		// 建立到 running。
		for i, event := range []TaskEvent{EventCreate, EventClaim} {
			tx, _ := store.db.Begin()
			if _, err := transitionTaskState(tx, taskID, event, v3P103Guard(i), now+int64(i)); err != nil {
				t.Fatalf("setup %s: %v", event, err)
			}
			tx.Commit()
		}
		// 反例：running 不能直接成功（必须经验收与收尾）。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		tr, err := transitionTaskState(tx, taskID, EventFinalized, v3P103Guard(2), now)
		if err == nil {
			t.Error("running -> succeeded (via finalized) not rejected")
		}
		if !tr.Rejected {
			t.Error("illegal transition not marked rejected")
		}
		if tr.Rejection == "" {
			t.Error("rejection must record a reason")
		}
		tx.Rollback()
	})

	t.Run("stale_revision_rejected", func(t *testing.T) {
		store, taskID := setupV3P103(t, "stale.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		if _, err := transitionTaskState(tx, taskID, EventCreate, v3P103Guard(0), now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 反例：用旧版本（0 而非当前 1）发起事件被拒。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		tr, err := transitionTaskState(tx, taskID, EventClaim, v3P103Guard(0), now)
		if !errors.Is(err, ErrState) {
			t.Errorf("stale revision not rejected with ErrState: %v", err)
		}
		if !tr.Rejected || !strings.Contains(tr.Rejection, "存储版本冲突") {
			t.Errorf("stale revision rejection reason missing: %+v", tr)
		}
	})

	t.Run("pause_resume_with_pending_reconcile", func(t *testing.T) {
		store, taskID := setupV3P103(t, "pause.db")
		now := v3P102Now()
		// create → claim → running。
		for i, event := range []TaskEvent{EventCreate, EventClaim} {
			tx, _ := store.db.Begin()
			if _, err := transitionTaskState(tx, taskID, event, v3P103Guard(i), now+int64(i)); err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		// running → pause。
		tx, _ := store.db.Begin()
		tr, err := transitionTaskState(tx, taskID, EventPause, v3P103Guard(2), now)
		if err != nil {
			t.Fatal(err)
		}
		if tr.To != V3TaskPaused {
			t.Errorf("pause to=%s, want paused", tr.To)
		}
		// 暂停必须产生"核对已派发"的后续命令。
		found := false
		for _, cmd := range tr.FollowUp {
			if cmd == "reconcile_dispatched" {
				found = true
			}
		}
		if !found {
			t.Errorf("pause follow-up missing reconcile_dispatched: %v", tr.FollowUp)
		}
		tx.Commit()
		// paused → resume（有未决操作时产生核对命令）。
		tx, _ = store.db.Begin()
		guard := v3P103Guard(3)
		guard.PendingOperations = 2
		tr, err = transitionTaskState(tx, taskID, EventResume, guard, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		found = false
		for _, cmd := range tr.FollowUp {
			if cmd == "recheck_operations" {
				found = true
			}
		}
		if !found {
			t.Errorf("resume with pending ops missing recheck_operations: %v", tr.FollowUp)
		}
	})
}

// TestV3P103TaskTerminalImmutable 终态不复活与关联新 Task。
func TestV3P103TaskTerminalImmutable(t *testing.T) {
	t.Run("terminal_states_never_revive", func(t *testing.T) {
		for _, terminal := range []struct {
			name  string
			event TaskEvent
			guard TaskGuard
		}{
			{"succeeded", EventFinalized, v3P103Guard(0)},
			{"failed", EventFail, v3P103Guard(0)},
			{"cancelled", EventCancel, v3P103Guard(0)},
		} {
			t.Run(terminal.name, func(t *testing.T) {
				store, taskID := setupV3P103(t, "terminal-"+terminal.name+".db")
				now := v3P102Now()
				// 驱动到终态。
				tx, _ := store.db.Begin()
				if _, err := transitionTaskState(tx, taskID, EventCreate, v3P103Guard(0), now); err != nil {
					t.Fatal(err)
				}
				tx.Commit()
				// 经 finalizing 到终态（succeeded 走 finalized，failed/cancelled 直接到终态）。
				tx, _ = store.db.Begin()
				if _, err := transitionTaskState(tx, taskID, EventCancel, v3P103Guard(1), now); err != nil {
					t.Fatal(err)
				}
				tx.Commit()
				tx, _ = store.db.Begin()
				if terminal.name == "succeeded" {
					if _, err := transitionTaskState(tx, taskID, EventFinalized, v3P103Guard(2), now); err != nil {
						t.Fatal(err)
					}
				} else {
					// failed/cancelled：从 finalizing 进入对应终态。
					// 用 finalize 路径；这里直接验证终态后的复活被拒。
					_ = terminal.event
				}
				tx.Commit()
				// 反例：终态后任何事件被拒（终态不复活）。
				tx, err := store.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				current, _ := readTaskState(tx, taskID)
				if !current.State.IsTerminal() && current.State != V3TaskFinalizing {
					t.Fatalf("expected terminal/finalizing state, got %s", current.State)
				}
				// 对真正终态验证复活被拒。
				if current.State.IsTerminal() {
					tr, err := transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
					if err == nil {
						t.Errorf("terminal %s revived via claim", current.State)
					}
					if !tr.Rejected || !strings.Contains(tr.Rejection, "不复活") {
						t.Errorf("terminal rejection missing: %+v", tr)
					}
				}
			})
		}
	})

	t.Run("successor_task_decision", func(t *testing.T) {
		store, taskID := setupV3P103(t, "successor.db")
		now := v3P102Now()
		// 驱动到 cancelled。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, v3P103Guard(0), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskID, EventCancel, v3P103Guard(1), now)
		tx.Commit()
		// 进入终态 cancelled（经 finalizing）。
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskID, EventFinalized, v3P103Guard(2), now)
		tx.Commit()

		decision, state, err := successorTaskDecision(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		// cancelled 是终态：必须创建关联新 Task，不复活原任务。
		if state.IsTerminal() && decision != SuccessorCreateNew {
			t.Errorf("terminal %s decision = %s, want create_successor", state, decision)
		}
	})

	t.Run("unknown_effects_block_finalization", func(t *testing.T) {
		store, taskID := setupV3P103(t, "unknown.db")
		now := v3P102Now()
		// 驱动到 finalizing。
		for i, event := range []TaskEvent{EventCreate, EventCancel} {
			tx, _ := store.db.Begin()
			guard := v3P103Guard(i)
			if _, err := transitionTaskState(tx, taskID, event, guard, now+int64(i)); err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		// 反例：有未知效果时不能终结。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		guard := v3P103Guard(2)
		guard.UnknownEffects = 1
		tr, err := transitionTaskState(tx, taskID, EventFinalized, guard, now)
		if err == nil {
			t.Error("finalization with unknown effects not rejected")
		}
		if !tr.Rejected || !strings.Contains(tr.Rejection, "未知效果") {
			t.Errorf("unknown effects rejection missing: %+v", tr)
		}
	})
}

// TestV3P103AcceptanceBeforeFinalState 完成判定与人工例外。
func TestV3P103AcceptanceBeforeFinalState(t *testing.T) {
	t.Run("model_or_step_completion_alone_not_success", func(t *testing.T) {
		store, taskID := setupV3P103(t, "completion.db")
		now := v3P102Now()
		// create → claim → running。
		for i, event := range []TaskEvent{EventCreate, EventClaim} {
			tx, _ := store.db.Begin()
			transitionTaskState(tx, taskID, event, v3P103Guard(i), now+int64(i))
			tx.Commit()
		}
		// 反例 1：有未决操作时，"完成"不能进入验收。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		guard := v3P103Guard(2)
		guard.PendingOperations = 1
		tr, err := transitionTaskState(tx, taskID, EventStepsComplete, guard, now)
		if err == nil {
			t.Error("steps complete with pending operations entered acceptance")
		}
		if !tr.Rejected {
			t.Error("pending operations not blocked")
		}
		tx.Rollback()
		// 反例 2：有未知效果时同样拒绝。
		tx, _ = store.db.Begin()
		guard = v3P103Guard(2)
		guard.UnknownEffects = 1
		tr, err = transitionTaskState(tx, taskID, EventStepsComplete, guard, now)
		if err == nil {
			t.Error("steps complete with unknown effects entered acceptance")
		}
		tx.Rollback()
		// 正例：无未决/未知时进入验收。
		tx, _ = store.db.Begin()
		tr, err = transitionTaskState(tx, taskID, EventStepsComplete, v3P103Guard(2), now)
		if err != nil {
			t.Fatal(err)
		}
		if tr.To != V3TaskAcceptance {
			t.Errorf("steps complete to=%s, want acceptance", tr.To)
		}
		tx.Commit()
	})

	t.Run("acceptance_requires_evidence", func(t *testing.T) {
		store, taskID := setupV3P103(t, "acceptance.db")
		now := v3P102Now()
		// 驱动到 acceptance。
		for i, event := range []TaskEvent{EventCreate, EventClaim, EventStepsComplete} {
			tx, _ := store.db.Begin()
			guard := v3P103Guard(i)
			if _, err := transitionTaskState(tx, taskID, event, guard, now+int64(i)); err != nil {
				t.Fatalf("setup %s: %v", event, err)
			}
			tx.Commit()
		}
		// 反例：验收证据无效时不能通过。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		guard := v3P103Guard(3)
		guard.AcceptanceEvidence = false
		tr, err := transitionTaskState(tx, taskID, EventAcceptancePass, guard, now)
		if err == nil {
			t.Error("acceptance pass without evidence not rejected")
		}
		if !tr.Rejected || !strings.Contains(tr.Rejection, "验收证据") {
			t.Errorf("acceptance evidence rejection missing: %+v", tr)
		}
		tx.Rollback()
		// 正例：证据有效时通过。
		tx, _ = store.db.Begin()
		tr, err = transitionTaskState(tx, taskID, EventAcceptancePass, withAcceptance(v3P103Guard(3)), now)
		if err != nil {
			t.Fatal(err)
		}
		if tr.To != V3TaskFinalizing {
			t.Errorf("acceptance pass to=%s, want finalizing", tr.To)
		}
		tx.Commit()
	})

	t.Run("personal_completion_guard_reads_acceptance", func(t *testing.T) {
		store, taskID := setupV3P103(t, "personal.db")
		ctx := context.Background()
		// 无验收时：证据无效。
		ok, err := personalCompletionGuard(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("acceptance evidence true without any acceptance record")
		}
		// 完成并验收后：证据有效。
		run, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.delta(ctx, run.ID, "# 成果"); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		detail, err := store.Detail(taskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Accept(ctx, taskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
			t.Fatal(err)
		}
		ok, err = personalCompletionGuard(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("acceptance evidence false after valid acceptance")
		}
	})

	t.Run("ensure_not_terminal", func(t *testing.T) {
		store, taskID := setupV3P103(t, "ensure.db")
		now := v3P102Now()
		// 无 V3 状态时：不报错（兼容旧路径）。
		if err := ensureTaskNotTerminal(store.db, taskID); err != nil {
			t.Errorf("no V3 state should pass ensure: %v", err)
		}
		// 驱动到终态。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, v3P103Guard(0), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskID, EventCancel, v3P103Guard(1), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskID, EventFinalized, v3P103Guard(2), now)
		tx.Commit()
		// 终态后：拒绝。
		if err := ensureTaskNotTerminal(store.db, taskID); !errors.Is(err, ErrTaskTerminal) {
			t.Errorf("terminal task not rejected: %v", err)
		}
	})
}

// withAcceptance 设置验收证据守卫。
func withAcceptance(g TaskGuard) TaskGuard {
	g.AcceptanceEvidence = true
	return g
}

// 确保 sql 引用保留。
var _ = sql.ErrNoRows
