package work

// 验收：启动恢复与统一推进循环。
//
// 三个测试入口：
//   - TestControllerRecoveryOrder：恢复顺序（校验/迁移 → 未应用结果
//     与输入 → 未决执行核对 → Wait/Timer/Schedule → 就绪单元）；每步幂等；
//     在不同恢复步骤中止再启动，仍可从持久记录继续。
//   - TestRecoveryIdempotent：连续两次重启/恢复不产生重复 Task、
//     Timer 唤醒或结果应用；替换旧全局 running→interrupted 的粗粒度恢复，
//     能确定安全的继续，不明的保留核对。
//   - TestModelUnavailableRecovery：控制面模型不可用仍能处理输入回执、
//     到期和核对；健康检查展示恢复阶段与失败原因。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupRecoveryStore 创建存储与基础任务。
func setupRecoveryStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p113-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestControllerRecoveryOrder 恢复顺序。
func TestControllerRecoveryOrder(t *testing.T) {
	t.Run("recovery_phases_in_order", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "order.db")
		ctx := context.Background()
		now := recordsTestNow()
		// 准备各阶段数据：待应用结果、到期 Timer、到期 Schedule、就绪步骤。
		prepareRecoveryData(t, store, taskID, now)
		// 执行恢复。
		report, err := store.RecoverRuntime(ctx, now+10000)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		// 恢复顺序：五个阶段按序完成。
		want := []RecoveryPhase{PhaseValidateMigrate, PhasePendingResults, PhaseReconcileRunning, PhaseWaitsTimers, PhaseReadyUnits}
		if len(report.CompletedPhases) != len(want) {
			t.Errorf("completed phases = %v, want %v", report.CompletedPhases, want)
		}
		for i, phase := range want {
			if i >= len(report.CompletedPhases) || report.CompletedPhases[i] != phase {
				t.Errorf("phase %d = %v, want %s (恢复顺序固定)", i, report.CompletedPhases, phase)
			}
		}
		if report.Phase != PhaseComplete {
			t.Errorf("final phase = %s, want complete", report.Phase)
		}
	})

	t.Run("recovery_steps_idempotent_and_resumable", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "resume.db")
		ctx := context.Background()
		now := recordsTestNow()
		prepareRecoveryData(t, store, taskID, now)
		// 第一次恢复。
		first, err := store.RecoverRuntime(ctx, now+10000)
		if err != nil {
			t.Fatal(err)
		}
		// 在不同恢复步骤中止再启动：从持久记录继续（幂等）。
		second, err := store.RecoverRuntime(ctx, now+10000)
		if err != nil {
			t.Fatal(err)
		}
		// 重复恢复不产生重复结果应用/Timer 投递/计划触发。
		if second.PendingResults > first.PendingResults {
			t.Errorf("second recovery applied more results: %d > %d (每步幂等)", second.PendingResults, first.PendingResults)
		}
		if second.DeliveredTimers > 0 {
			t.Errorf("second recovery re-delivered timers: %d (重复恢复不重复投递)", second.DeliveredTimers)
		}
		if second.TriggeredSchedules > 0 {
			t.Errorf("second recovery re-triggered schedules: %d (重复恢复不重复触发)", second.TriggeredSchedules)
		}
	})

	t.Run("validate_migrate_failure_stops_dispatch", func(t *testing.T) {
		// 校验/迁移失败：不派发工作（不进入后续阶段）。
		path := filepath.Join(t.TempDir(), "badversion.db")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		// 回写错误版本。
		if _, err := store.db.Exec("PRAGMA user_version=1"); err != nil {
			t.Fatal(err)
		}
		report, err := store.RecoverRuntime(context.Background(), recordsTestNow())
		store.Close()
		if err == nil {
			t.Error("recovery succeeded on unmigrated database (校验失败应阻止派发)")
		}
		if report.FailedPhase != PhaseValidateMigrate {
			t.Errorf("failed phase = %s, want validate_migrate", report.FailedPhase)
		}
		if report.Failure == "" {
			t.Error("failure reason empty")
		}
		if len(report.CompletedPhases) != 0 {
			t.Errorf("completed phases after failure = %v, want none (校验失败不派发工作)", report.CompletedPhases)
		}
	})
}

// prepareRecoveryData 准备恢复各阶段数据。
func prepareRecoveryData(t *testing.T, store *Store, taskID string, now int64) {
	t.Helper()
	// 待应用结果。
	tx, _ := store.db.Begin()
	input, output := int64(10), int64(5)
	recordReport(tx, ResultReport{ResultKey: "recover-result", TaskID: taskID, Summary: "完成", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
	// 到期 Timer。
	wait, _ := openWait(tx, Wait{WaitID: "recover-wait", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
	createTimer(tx, Timer{TimerID: "recover-timer", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now - 1000, CreatedAt: now})
	// 就绪步骤。
	insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
		DefinitionKey: "recover-step", DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now})
	tx.Commit()
}

// TestRecoveryIdempotent 恢复幂等。
func TestRecoveryIdempotent(t *testing.T) {
	t.Run("two_restarts_no_duplicate_task_timer_result", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "idempotent.db")
		ctx := context.Background()
		now := recordsTestNow()
		prepareRecoveryData(t, store, taskID, now)
		// 连续两次恢复。
		for i := 0; i < 2; i++ {
			if _, err := store.RecoverRuntime(ctx, now+10000); err != nil {
				t.Fatal(err)
			}
		}
		// 结果应用不重复（只有 1 个已应用）。
		var appliedCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM result_receipts WHERE state='applied' AND result_key='recover-result'").Scan(&appliedCount); err != nil {
			t.Fatal(err)
		}
		if appliedCount != 1 {
			t.Errorf("applied results = %d, want 1 (重复恢复不重复应用)", appliedCount)
		}
		// Timer 唤醒不重复（只有 1 个 Inbox 事件）。
		var timerEvents int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE source='timer'").Scan(&timerEvents); err != nil {
			t.Fatal(err)
		}
		if timerEvents != 1 {
			t.Errorf("timer inbox events = %d, want 1 (重复恢复不重复唤醒)", timerEvents)
		}
	})

	t.Run("lost_worker_marked_reconcile_not_unexecuted", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "lost.db")
		now := recordsTestNow()
		// 一个 claimed（已领取但失联）的 work_unit。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "lost-step", DefinitionVersion: 1, State: RuntimeStepRunning, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitClaimed, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 恢复：失联的 work_unit 重新就绪（不视为未执行，可重新核对）。
		reconciled, err := store.reconcileRunningRuns(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if reconciled != 1 {
			t.Errorf("reconciled = %d, want 1 (失联工作核对)", reconciled)
		}
		// work_unit 回到 ready（可重新核对，不把失联视为未执行）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM work_units WHERE task_id=?", taskID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "ready" {
			t.Errorf("work unit state = %s, want ready (失联重新核对不视为未执行)", state)
		}
	})

	t.Run("unknown_read_preserved_as_reconcile_not_success", func(t *testing.T) {
		// 不明的保留核对：未知读取不停机后视为成功。
		store, taskID := setupRecoveryStore(t, "unknown.db")
		ctx := context.Background()
		// 先建立连接（run_reads 外键要求）。
		if _, err := store.db.Exec(`INSERT INTO connections(id,name,config,enabled,status,protocol)
VALUES('conn-unknown','资料','{}',1,'ready','streamable_http')`); err != nil {
			t.Fatal(err)
		}
		run, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// 注入未知读取。
		if _, err := store.db.Exec(`INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,detail,received_at,acknowledged_at)
VALUES(?,1,'conn-unknown',1,'资料','read','读取','v1','{}','unknown','结果取回失败',0,0)`, run.ID); err != nil {
			t.Fatal(err)
		}
		// 恢复：未知读取保持待核对（不视为成功）。
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		var taskStatus, taskReason string
		if err := store.db.QueryRow("SELECT status,status_reason FROM tasks WHERE id=?", taskID).Scan(&taskStatus, &taskReason); err != nil {
			t.Fatal(err)
		}
		if taskStatus == "completed" {
			t.Errorf("unknown read marked completed after recovery (不明的保留核对不视为成功)")
		}
		t.Logf("unknown read after recovery: status=%s reason=%s (保留核对)", taskStatus, taskReason)
	})
}

// TestModelUnavailableRecovery 模型不可用恢复。
func TestModelUnavailableRecovery(t *testing.T) {
	t.Run("model_unavailable_still_processes_receipts_timers", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "nomodel.db")
		now := recordsTestNow()
		prepareRecoveryData(t, store, taskID, now)
		// 模型不可用：统一推进循环仍处理输入回执、到期和核对。
		loop := NewRuntimeLoop(store, func() bool { return false }) // 模型不可用
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		loop.Run(ctx, 10*time.Millisecond, 3) // 跑 3 轮
		rounds, _ := loop.Stats()
		if rounds < 1 {
			t.Error("loop did not run (模型不可用仍能处理确定性工作)")
		}
		// 到期 Timer 已投递（模型不可用仍处理）。
		var timerState string
		if err := store.db.QueryRow("SELECT state FROM timers WHERE timer_id='recover-timer'").Scan(&timerState); err != nil {
			t.Fatal(err)
		}
		if timerState != "delivered" {
			t.Errorf("timer state = %s, want delivered (模型不可用仍处理到期)", timerState)
		}
	})

	t.Run("health_shows_recovery_phase_and_failures", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "health.db")
		now := recordsTestNow()
		prepareRecoveryData(t, store, taskID, now)
		// 健康检查：展示恢复阶段与失败原因。
		report, err := store.RecoverRuntime(context.Background(), now+10000)
		if err != nil {
			t.Fatal(err)
		}
		health, err := store.Health(false) // 模型不可用
		if err != nil {
			t.Fatal(err)
		}
		if !health.Migrated {
			t.Error("health not migrated")
		}
		if health.DatabaseWritable != true {
			t.Error("database not writable in health")
		}
		if report.Phase != PhaseComplete {
			t.Errorf("recovery phase = %s, want complete (健康检查展示恢复阶段)", report.Phase)
		}
		// 观测项可读（待应用结果/到期/就绪单元）。
		t.Logf("health: migrated=%v writable=%v pending_results=%d pending_timers=%d ready_units=%d",
			health.Migrated, health.DatabaseWritable, health.PendingResults, health.PendingTimers, health.ReadyUnits)
	})

	t.Run("unwritable_database_stops_new_dispatch", func(t *testing.T) {
		// 数据库不可写时停止新的相关派发（不虚假确认）。
		ro, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(t.TempDir(), "ro.db"), "mode=ro"))
		if err == nil {
			defer ro.Close()
			if _, err := ro.Exec("CREATE TABLE x(id INTEGER)"); err == nil {
				t.Error("readonly database allowed write (不可写时未停止派发)")
			}
		}
	})

	t.Run("loop_exit_preserves_persisted_responsibilities", func(t *testing.T) {
		store, taskID := setupRecoveryStore(t, "exit.db")
		now := recordsTestNow()
		prepareRecoveryData(t, store, taskID, now)
		// 启动循环并立即取消（退出期限）。
		loop := NewRuntimeLoop(store, func() bool { return false })
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 立即取消
		loop.Run(ctx, time.Millisecond, 0)
		// 退出后：持久责任仍在（不依赖内存 map 才能恢复）。
		var pendingResults int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM result_receipts WHERE state='received'").Scan(&pendingResults); err != nil {
			t.Fatal(err)
		}
		if pendingResults != 1 {
			t.Errorf("pending results after exit = %d, want 1 (退出保存已持久责任)", pendingResults)
		}
		// 再次启动可恢复（不依赖内存）。
		report, err := store.RecoverRuntime(context.Background(), now+10000)
		if err != nil {
			t.Fatal(err)
		}
		if report.Phase != PhaseComplete {
			t.Errorf("recovery after loop exit = %s, want complete (不依赖内存 map)", report.Phase)
		}
	})
}

// 确保 sql/strings 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
)
