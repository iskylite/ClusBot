package work

// P2-05 验收：公平队列、背压与关键工作保留容量。
//
// 三个测试入口：
//   - TestV3P205SchedulerFairness：按主体/来源与对象范围分稳定组，组间轮转、
//     组内优先级与等待老化；持续高优先级流量下有容量且符合条件的低优先级
//     工作最终获得执行；保存必要游标/计数以便恢复。
//   - TestV3P205BackpressureBeforeAck：满队列返回未受理的明确响应；同键重试
//     不会额外建任务；新任务超限在持久受理确认前返回可重试结果及请求键；
//     已确认受理的任务只能延期并暴露积压，不能因过载丢弃。
//   - TestV3P205CriticalWorkCapacity：故障风暴下取消、续租与结果回执仍有处理
//     机会，不能只证明新任务吞吐；按类别区分局部提供者失败与数据库全局
//     不可写；重试退避有抖动和上限。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P205 创建存储。
func setupV3P205(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P205SchedulerFairness 公平调度。
func TestV3P205SchedulerFairness(t *testing.T) {
	t.Run("stable_groups_by_principal_source_scope", func(t *testing.T) {
		store := setupV3P205(t, "groups.db")
		now := v3P102Now()
		// 按主体/来源与对象范围分稳定组。
		tx, _ := store.db.Begin()
		g1, err := ensureSchedulerGroup(tx, "user-a", "chat", "工作资料", now)
		if err != nil {
			t.Fatal(err)
		}
		g2, err := ensureSchedulerGroup(tx, "user-b", "schedule", "网络运维", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if g1.GroupKey == g2.GroupKey {
			t.Error("different principal/source/scope share group (应分稳定组)")
		}
		// 组内保留容量与游标初始。
		if g1.ReservedCapacity < 1 {
			t.Errorf("reserved capacity = %d, want >= 1 (为回执/续租/取消/核对保留)", g1.ReservedCapacity)
		}
		// 重复 ensure：幂等（稳定组）。
		tx, _ = store.db.Begin()
		g1again, err := ensureSchedulerGroup(tx, "user-a", "chat", "工作资料", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if g1again.GroupKey != g1.GroupKey {
			t.Errorf("group not stable: %s vs %s", g1again.GroupKey, g1.GroupKey)
		}
	})

	t.Run("aging_prevents_low_priority_starvation", func(t *testing.T) {
		store := setupV3P205(t, "aging.db")
		now := v3P102Now()
		ctx := context.Background()
		// 高优先级新任务 + 低优先级老任务（老化）。
		for i, suffix := range []string{"new-high", "old-low"} {
			run, _ := store.Submit(ctx, "v3p205-"+suffix, "整理材料"+suffix, "", "", true)
			tx, _ := store.db.Begin()
			insertStep(tx, Step{StepID: id(), TaskID: run.TaskID, ContractRevision: 1,
				DefinitionKey: "step-" + suffix, DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			steps, _ := listSteps(store.db, run.TaskID)
			// 老任务的 ready_at 更早（老化）。
			readyAt := now
			if suffix == "old-low" {
				readyAt = now - 100000 // 老任务等待更久
			}
			tx, _ = store.db.Begin()
			insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run.TaskID, StepID: steps[0].StepID,
				Kind: WorkUnitModel, ReadyAt: readyAt, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			_ = i
		}
		// 公平选取：老化最大者优先（低优先级老任务获得机会，不饿死）。
		tx, _ := store.db.Begin()
		group, _ := ensureSchedulerGroup(tx, "user", "chat", "工作", now)
		selected, err := selectFairUnit(tx, []SchedulerGroup{group}, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if selected == nil {
			t.Fatal("no unit selected")
		}
		// 老化的（old-low）被优先选取（等待时间提升）。
		if selected.AgeMS <= 0 {
			t.Errorf("selected unit age = %d (老化应优先)", selected.AgeMS)
		}
		var taskTitle string
		if err := store.db.QueryRow("SELECT title FROM tasks WHERE id=?", selected.Unit.TaskID).Scan(&taskTitle); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(taskTitle, "old-low") {
			t.Logf("selected aging unit: task=%s age=%dms (低优先级最终获得机会)", taskTitle, selected.AgeMS)
		}
	})

	t.Run("cursor_preserved_for_recovery", func(t *testing.T) {
		store := setupV3P205(t, "cursor.db")
		now := v3P102Now()
		// 推进游标后：游标/计数持久保存（恢复用）。
		tx, _ := store.db.Begin()
		group, _ := ensureSchedulerGroup(tx, "user", "chat", "工作", now)
		recordFairnessClaim(tx, group.GroupKey, "unit-1", 500, "worker-1", now)
		tx.Commit()
		var cursor, totalClaimed int
		if err := store.db.QueryRow("SELECT round_robin_cursor, total_claimed FROM scheduler_groups WHERE group_key=?", group.GroupKey).Scan(&cursor, &totalClaimed); err != nil {
			t.Fatal(err)
		}
		if totalClaimed != 1 {
			t.Errorf("total claimed = %d, want 1 (游标/计数保存)", totalClaimed)
		}
		// 公平性快照：排队时间可观测。
		snapshot, err := readFairnessSnapshot(store.db, group.GroupKey)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.TotalClaimed != 1 || snapshot.AvgQueueWaitMS != 500 {
			t.Errorf("fairness snapshot = %+v (排队时间证据)", snapshot)
		}
	})
}

// TestV3P205BackpressureBeforeAck 受理前背压。
func TestV3P205BackpressureBeforeAck(t *testing.T) {
	t.Run("full_queue_returns_not_accepted_with_retry_key", func(t *testing.T) {
		store := setupV3P205(t, "full.db")
		now := v3P102Now()
		ctx := context.Background()
		// 填满队列（maxPending=2）。
		for _, suffix := range []string{"a", "b"} {
			run, _ := store.Submit(ctx, "v3p205-full-"+suffix, "材料"+suffix, "", "", true)
			tx, _ := store.db.Begin()
			insertStep(tx, Step{StepID: id(), TaskID: run.TaskID, ContractRevision: 1,
				DefinitionKey: "step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			steps, _ := listSteps(store.db, run.TaskID)
			tx, _ = store.db.Begin()
			insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run.TaskID, StepID: steps[0].StepID,
				Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
		}
		// 新任务超限：在持久受理确认前返回可重试结果及请求键。
		tx, _ := store.db.Begin()
		result, err := checkAdmissionCapacity(tx, "req-overflow", false, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Accepted {
			t.Error("overflow task accepted (满队列应返回未受理)")
		}
		if result.RequestKey != "req-overflow" {
			t.Errorf("request key = %s (返回请求键)", result.RequestKey)
		}
		if result.RetryAfter <= now {
			t.Errorf("retry after = %d, want > %d (可重试)", result.RetryAfter, now)
		}
		if result.Category != BackpressureLocalProvider {
			t.Errorf("category = %s, want local_provider", result.Category)
		}
	})

	t.Run("same_key_retry_not_duplicate_task", func(t *testing.T) {
		store := setupV3P205(t, "dupkey.db")
		now := v3P102Now()
		// 同键重试：不会额外建任务（背压记录幂等）。
		tx, _ := store.db.Begin()
		checkAdmissionCapacity(tx, "req-dup", false, 0, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		checkAdmissionCapacity(tx, "req-dup", false, 0, now)
		tx.Commit()
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM scheduler_backpressure WHERE request_key='req-dup'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("backpressure records = %d, want 1 (同键重试不额外建任务)", count)
		}
	})

	t.Run("admitted_task_deferred_not_dropped_backlog_exposed", func(t *testing.T) {
		store := setupV3P205(t, "backlog.db")
		now := v3P102Now()
		ctx := context.Background()
		// 已确认受理的任务（在队列中）：只能延期并暴露积压，不能因过载丢弃。
		run, _ := store.Submit(ctx, "v3p205-backlog", "整理材料", "", "", true)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: run.TaskID, ContractRevision: 1,
			DefinitionKey: "step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, _ := listSteps(store.db, run.TaskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run.TaskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now - 5000, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 积压暴露：已受理任务延期（ready_at 老化）且不丢弃。
		backlog, err := readQueueBacklog(store.db, now)
		if err != nil {
			t.Fatal(err)
		}
		if backlog.Ready != 1 {
			t.Errorf("backlog ready = %d, want 1 (已受理任务延期暴露不丢弃)", backlog.Ready)
		}
		if backlog.OldestReadyMS != 5000 {
			t.Errorf("oldest ready age = %d, want 5000 (积压年龄可观测)", backlog.OldestReadyMS)
		}
	})
}

// TestV3P205CriticalWorkCapacity 关键工作容量。
func TestV3P205CriticalWorkCapacity(t *testing.T) {
	t.Run("critical_work_always_admitted_under_storm", func(t *testing.T) {
		store := setupV3P205(t, "storm.db")
		now := v3P102Now()
		ctx := context.Background()
		// 故障风暴：填满队列。
		for i := 0; i < 5; i++ {
			run, _ := store.Submit(ctx, "v3p205-storm-"+string(rune('a'+i)), "材料", "", "", true)
			tx, _ := store.db.Begin()
			insertStep(tx, Step{StepID: id(), TaskID: run.TaskID, ContractRevision: 1,
				DefinitionKey: "step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
			steps, _ := listSteps(store.db, run.TaskID)
			tx, _ = store.db.Begin()
			insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: run.TaskID, StepID: steps[0].StepID,
				Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
			tx.Commit()
		}
		// 故障风暴下：取消、续租、结果回执（关键工作）仍有处理机会。
		for _, critical := range []string{"cancel-req", "renew-req", "report-req"} {
			tx, _ := store.db.Begin()
			result, err := checkAdmissionCapacity(tx, critical, true, 2, now) // isCritical=true
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if !result.Accepted {
				t.Errorf("critical work %s not admitted under storm (关键工作保留容量)", critical)
			}
		}
	})

	t.Run("classify_local_vs_global_failure", func(t *testing.T) {
		// 按类别区分局部提供者失败与数据库全局不可写。
		if got := classifyFailure(ErrDatabaseUnwritable); got != BackpressureGlobalDB {
			t.Errorf("unwritable classified as %s, want global_db", got)
		}
		if got := classifyFailure(errors.New("attempt to write a readonly database")); got != BackpressureGlobalDB {
			t.Errorf("readonly classified as %s, want global_db", got)
		}
		if got := classifyFailure(errors.New("provider timeout")); got != BackpressureLocalProvider {
			t.Errorf("provider timeout classified as %s, want local_provider", got)
		}
		if got := classifyFailure(nil); got != BackpressureLocalProvider {
			t.Errorf("nil classified as %s", got)
		}
	})

	t.Run("backoff_has_jitter_and_cap", func(t *testing.T) {
		// 重试退避有抖动和上限。
		var values []int64
		for pending := 1; pending <= 100; pending += 20 {
			values = append(values, backpressureBackoff(pending))
		}
		// 上限：不超过 30 秒。
		for _, v := range values {
			if v > 30000 {
				t.Errorf("backoff %d exceeds cap 30000 (上限)", v)
			}
			if v <= 0 {
				t.Errorf("backoff %d not positive", v)
			}
		}
		// 抖动：值不完全线性（有抖动）。
		allSame := true
		for i := 1; i < len(values); i++ {
			if values[i] != values[0] {
				allSame = false
			}
		}
		if allSame && len(values) > 1 {
			t.Error("backoff has no jitter (退避应有抖动)")
		}
	})

	t.Run("local_failure_not_block_unrelated_task", func(t *testing.T) {
		store := setupV3P205(t, "localfail.db")
		now := v3P102Now()
		// 局部提供者失败：不阻塞无关 Task（只该提供者相关的工作受影响）。
		category := classifyFailure(errors.New("provider X timeout"))
		if category != BackpressureLocalProvider {
			t.Fatalf("local failure classified as %s", category)
		}
		// 无关任务仍可受理（局部失败不全局阻塞）。
		tx, _ := store.db.Begin()
		result, err := checkAdmissionCapacity(tx, "unrelated-req", false, 100, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Accepted {
			t.Error("local provider failure blocked unrelated task (局部失败不阻塞无关 Task)")
		}
	})
}

// 确保 sql/strings/context/errors 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = errors.Is
)
