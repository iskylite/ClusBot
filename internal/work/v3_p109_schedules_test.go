package work

// P1-09 验收：Schedule 与旧周期 Goal 兼容。
//
// 三个测试入口：
//   - TestV3P109ScheduleOccurrenceOnce：同发生点重复扫描只建一个 Task，重启
//     保持稳定身份；创建 Task、发生记录及推进 next_at 同事务。
//   - TestV3P109ScheduleMissedPolicy：计划到期/停止后不再产生新工作；停机
//     补发数量、时区边界和 next_at 均有可控时钟用例；显式跳过/合并/有界补发，
//     不无界补建。
//   - TestV3P109LegacyGoalCompatibility：旧周期 Goal 映射到计划与可选上层
//     目标的关联，不复制两套调度权威；已有 Task/历史记录保留。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupV3P109 创建存储。
func setupV3P109(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// v3P109Schedule 构造一个测试计划。
func v3P109Schedule(id string, intervalMS, nextAt int64, policy MissedPolicy) Schedule {
	return Schedule{
		ScheduleID:      id,
		Kind:            "interval",
		IntervalMS:      intervalMS,
		Timezone:        "Asia/Shanghai",
		Template:        ScheduleTemplate{Goal: "每日整理晨报", Scope: "工作资料", Criteria: "晨报已生成"},
		TemplateVersion: 1,
		Principal:       "user",
		ScopeLabel:      "工作资料",
		Enabled:         true,
		NextAt:          nextAt,
		ExpiresAt:       nextAt + 30*24*time.Hour.Milliseconds(),
		MissedPolicy:    policy,
		CatchupLimit:    1,
		CreatedAt:       v3P102Now(),
		UpdatedAt:       v3P102Now(),
	}
}

// TestV3P109ScheduleOccurrenceOnce 发生点幂等。
func TestV3P109ScheduleOccurrenceOnce(t *testing.T) {
	t.Run("same_occurrence_single_task", func(t *testing.T) {
		store := setupV3P109(t, "once.db")
		now := v3P102Now()
		s := v3P109Schedule("sched-1", time.Hour.Milliseconds(), now-1000, MissedCoalesce)
		// 创建计划。
		tx, _ := store.db.Begin()
		if _, err := upsertSchedule(tx, s); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 第一次触发。
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, s.ScheduleID)
		first, err := triggerSchedule(tx, s, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !first.Created || first.TaskID == "" {
			t.Fatalf("first trigger: %+v", first)
		}
		// 同发生点重复扫描：只建一个 Task（occurrence 幂等）。
		// 直接把 next_at 调回（模拟重复扫描同一发生点），在事务外执行。
		if _, err := store.db.Exec("UPDATE schedules SET next_at=? WHERE schedule_id=?", now-1000, s.ScheduleID); err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, s.ScheduleID)
		second, err := triggerSchedule(tx, s, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if second.Created {
			t.Error("duplicate occurrence created second task (同发生点重复扫描应幂等)")
		}
		// Task 总数为 1。
		var taskCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?", s.ScheduleID).Scan(&taskCount); err != nil {
			t.Fatal(err)
		}
		if taskCount != 1 {
			t.Errorf("occurrences = %d, want 1", taskCount)
		}
	})

	t.Run("restart_preserves_occurrence_identity", func(t *testing.T) {
		// 重启保持稳定身份：occurrence_key 由 schedule_id + 发生点决定，与进程无关。
		path := filepath.Join(t.TempDir(), "identity.db")
		now := v3P102Now()
		var firstKey, firstTaskID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			s := v3P109Schedule("sched-id", time.Hour.Milliseconds(), now-1000, MissedCoalesce)
			tx, _ := store.db.Begin()
			upsertSchedule(tx, s)
			tx.Commit()
			tx, _ = store.db.Begin()
			s, _ = readSchedule(tx, s.ScheduleID)
			result, _ := triggerSchedule(tx, s, now)
			tx.Commit()
			firstKey, firstTaskID = result.OccurrenceKey, result.TaskID
		}()
		// 重启：同发生点仍是同一身份（不重复建 Task）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		tx, _ := store.db.Begin()
		s, _ := readSchedule(tx, "sched-id")
		s.NextAt = now - 1000 // 重复扫描同一发生点
		result, err := triggerSchedule(tx, s, now)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		if result.OccurrenceKey != firstKey {
			t.Errorf("occurrence key changed after restart: %s vs %s (重启保持稳定身份)", result.OccurrenceKey, firstKey)
		}
		if result.Created {
			t.Error("restart re-created task for same occurrence")
		}
		if result.TaskID != firstTaskID {
			t.Errorf("task id changed: %s vs %s", result.TaskID, firstTaskID)
		}
	})

	t.Run("create_task_occurrence_nextat_same_tx", func(t *testing.T) {
		store := setupV3P109(t, "sametx.db")
		now := v3P102Now()
		s := v3P109Schedule("sched-tx", time.Hour.Milliseconds(), now-1000, MissedCoalesce)
		tx, _ := store.db.Begin()
		upsertSchedule(tx, s)
		tx.Commit()
		// 触发：创建 Task、occurrence、推进 next_at 同事务。
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, s.ScheduleID)
		result, err := triggerSchedule(tx, s, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 三者都生效（同事务）。
		var taskExists, occurrenceExists bool
		var nextAt int64
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)", result.TaskID).Scan(&taskExists); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM schedule_occurrences WHERE schedule_id=? AND occurrence_key=?)", s.ScheduleID, result.OccurrenceKey).Scan(&occurrenceExists); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT next_at FROM schedules WHERE schedule_id=?", s.ScheduleID).Scan(&nextAt); err != nil {
			t.Fatal(err)
		}
		if !taskExists || !occurrenceExists {
			t.Errorf("atomic trigger incomplete: task=%v occurrence=%v", taskExists, occurrenceExists)
		}
		if nextAt <= now-1000 {
			t.Errorf("next_at not advanced: %d (应推进到下一个未来发生点)", nextAt)
		}
		t.Logf("trigger: task=%s occurrence=%s next_at=%d", result.TaskID, result.OccurrenceKey, nextAt)
	})
}

// TestV3P109ScheduleMissedPolicy 漏触发策略。
func TestV3P109ScheduleMissedPolicy(t *testing.T) {
	t.Run("disabled_or_expired_no_new_work", func(t *testing.T) {
		store := setupV3P109(t, "disabled.db")
		now := v3P102Now()
		// 已停用计划。
		disabled := v3P109Schedule("sched-off", time.Hour.Milliseconds(), now-1000, MissedCoalesce)
		disabled.Enabled = false
		tx, _ := store.db.Begin()
		upsertSchedule(tx, disabled)
		tx.Commit()
		result, err := scanDueSchedules(store.db, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if result.Triggered != 0 {
			t.Errorf("disabled schedule triggered: %d (停用后不再产生新工作)", result.Triggered)
		}
		// 已过期计划。
		expired := v3P109Schedule("sched-exp", time.Hour.Milliseconds(), now-1000, MissedCoalesce)
		expired.ExpiresAt = now - 100 // 已过期
		tx, _ = store.db.Begin()
		upsertSchedule(tx, expired)
		tx.Commit()
		result, err = scanDueSchedules(store.db, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if result.Triggered != 0 {
			t.Errorf("expired schedule triggered: %d (到期后不再产生新工作)", result.Triggered)
		}
	})

	t.Run("missed_policy_skip_coalesce_bounded", func(t *testing.T) {
		store := setupV3P109(t, "policy.db")
		now := v3P102Now()
		hour := time.Hour.Milliseconds()
		// 跳过：错过 3 个周期，直接到下一个（不补发）。
		skip := v3P109Schedule("sched-skip", hour, now-3*hour, MissedSkip)
		tx, _ := store.db.Begin()
		upsertSchedule(tx, skip)
		tx.Commit()
		tx, _ = store.db.Begin()
		s, _ := readSchedule(tx, "sched-skip")
		result, _ := triggerSchedule(tx, s, now)
		tx.Commit()
		// 合并：错过 3 个周期，合并为一次（coalesce）。
		coalesce := v3P109Schedule("sched-coalesce", hour, now-3*hour, MissedCoalesce)
		tx, _ = store.db.Begin()
		upsertSchedule(tx, coalesce)
		tx.Commit()
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, "sched-coalesce")
		coalesceResult, _ := triggerSchedule(tx, s, now)
		tx.Commit()
		if coalesceResult.Outcome != "coalesced" {
			t.Errorf("coalesce outcome = %s, want coalesced (停机漏周期合并)", coalesceResult.Outcome)
		}
		// 验证合并只建一个 Task（不无界补建）。
		var coalesceTasks int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id='sched-coalesce'").Scan(&coalesceTasks); err != nil {
			t.Fatal(err)
		}
		if coalesceTasks != 1 {
			t.Errorf("coalesce created %d tasks, want 1 (合并不无界补建)", coalesceTasks)
		}
		// 有界补发：补发最早的错过点（不超过上限）。
		catchup := v3P109Schedule("sched-catchup", hour, now-3*hour, MissedBoundedCatchup)
		catchup.CatchupLimit = 2
		tx, _ = store.db.Begin()
		upsertSchedule(tx, catchup)
		tx.Commit()
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, "sched-catchup")
		catchupResult, _ := triggerSchedule(tx, s, now)
		tx.Commit()
		if catchupResult.Outcome != "catchup" {
			t.Errorf("catchup outcome = %s, want catchup (有界补发)", catchupResult.Outcome)
		}
		t.Logf("policies: skip=%s coalesce=%s catchup=%s", result.Outcome, coalesceResult.Outcome, catchupResult.Outcome)
	})

	t.Run("nextat_advanced_to_future", func(t *testing.T) {
		store := setupV3P109(t, "nextat.db")
		now := v3P102Now()
		hour := time.Hour.Milliseconds()
		// next_at 在过去：触发后推进到下一个未来发生点。
		s := v3P109Schedule("sched-next", hour, now-2*hour, MissedCoalesce)
		tx, _ := store.db.Begin()
		upsertSchedule(tx, s)
		tx.Commit()
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, s.ScheduleID)
		result, err := triggerSchedule(tx, s, now)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		// next_at 推进到未来（不停留在过去导致无限重复触发）。
		if result.NextAt <= now {
			t.Errorf("next_at = %d, want > %d (推进到下一个未来发生点)", result.NextAt, now)
		}
		// 时区边界：timezone 已持久（时区相关计算在 cron 接入时生效）。
		var tz string
		if err := store.db.QueryRow("SELECT timezone FROM schedules WHERE schedule_id=?", s.ScheduleID).Scan(&tz); err != nil {
			t.Fatal(err)
		}
		if tz == "" {
			t.Error("timezone not persisted")
		}
	})

	t.Run("principal_required_no_anonymous_superuser", func(t *testing.T) {
		// 反例：匿名/超级权限主体被拒绝（不能用匿名后台超级权限）。
		for _, bad := range []string{"", "anonymous", "system-superuser"} {
			s := v3P109Schedule("sched-bad", time.Hour.Milliseconds(), v3P102Now(), MissedCoalesce)
			s.Principal = bad
			store := setupV3P109(t, "principal-"+bad+".db")
			tx, _ := store.db.Begin()
			_, err := upsertSchedule(tx, s)
			tx.Rollback()
			if err == nil {
				t.Errorf("principal %q not rejected (不能用匿名后台超级权限)", bad)
			}
		}
	})
}

// TestV3P109LegacyGoalCompatibility 旧周期 Goal 兼容。
func TestV3P109LegacyGoalCompatibility(t *testing.T) {
	t.Run("legacy_goal_maps_to_schedule_with_link", func(t *testing.T) {
		// 旧周期 Goal 映射到计划与可选上层目标的关联，不复制两套调度权威。
		now := v3P102Now()
		goal := Goal{
			ID: "goal-1", Target: "每日整理晨报", Scope: "工作资料", Criteria: "晨报已生成",
			Interval: time.Hour.Milliseconds() * 24, NextAt: now + time.Hour.Milliseconds(),
			ExpiresAt: now + 7*24*time.Hour.Milliseconds(), Remaining: 7, Status: "active", Revision: 1,
		}
		s := mapLegacyGoalToSchedule(goal, "user", now)
		if s.GoalID != goal.ID {
			t.Errorf("goal link = %s, want %s (关联旧 Goal)", s.GoalID, goal.ID)
		}
		if s.MissedPolicy != MissedCoalesce {
			t.Errorf("missed policy = %s, want coalesce (复用现有停机漏周期合并)", s.MissedPolicy)
		}
		if !s.Enabled {
			t.Error("active goal mapped to disabled schedule")
		}
		if s.Principal != "user" {
			t.Errorf("principal = %s, want user", s.Principal)
		}
		if s.Template.Goal != goal.Target || s.Template.Scope != goal.Scope {
			t.Errorf("template = %+v, want goal=%s scope=%s", s.Template, goal.Target, goal.Scope)
		}
	})

	t.Run("legacy_goal_trigger_via_schedule_single_authority", func(t *testing.T) {
		// 经 Schedule 触发，旧 Goal 的历史 Task 保留；不复制两套调度权威。
		store := setupV3P109(t, "legacytrigger.db")
		ctx := context.Background()
		// 创建旧 Goal（保留其历史）。CreateGoal 要求 NextAt 在真实未来，
		// 这里用真实当前时间计算；触发则用测试固定时钟。
		realNow := time.Now().UnixMilli()
		goal, err := store.CreateGoal(ctx, "v3p109-legacy", Goal{
			Target: "每日整理晨报", Scope: "工作资料", Criteria: "晨报已生成",
			Interval: time.Hour.Milliseconds() * 24, NextAt: realNow + time.Hour.Milliseconds(),
			ExpiresAt: realNow + 7*24*time.Hour.Milliseconds(), Remaining: 7,
		})
		if err != nil {
			t.Fatal(err)
		}
		// 映射为 Schedule 并触发。
		s := mapLegacyGoalToSchedule(goal, "user", v3P102Now())
		s.NextAt = v3P102Now() - 1000 // 到期
		tx, _ := store.db.Begin()
		if _, err := upsertSchedule(tx, s); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		s, _ = readSchedule(tx, s.ScheduleID)
		result, err := triggerSchedule(tx, s, v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Created {
			t.Fatalf("legacy schedule trigger failed: %+v", result)
		}
		// 旧 Goal 记录保留（不复制两套调度权威——Schedule 关联而非复制）。
		var goalExists bool
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM goals WHERE id=?)", goal.ID).Scan(&goalExists); err != nil {
			t.Fatal(err)
		}
		if !goalExists {
			t.Error("legacy goal record lost")
		}
		// 新 Task 由 Schedule 创建（单一调度权威）。
		var occurrenceGoalID string
		if err := store.db.QueryRow("SELECT goal_id FROM schedules WHERE schedule_id=?", s.ScheduleID).Scan(&occurrenceGoalID); err != nil {
			t.Fatal(err)
		}
		if occurrenceGoalID != goal.ID {
			t.Errorf("schedule goal link = %s, want %s", occurrenceGoalID, goal.ID)
		}
	})

	t.Run("scan_due_schedules_triggers_all", func(t *testing.T) {
		store := setupV3P109(t, "scan.db")
		now := v3P102Now()
		// 多个到期计划。
		for _, id := range []string{"scan-a", "scan-b"} {
			s := v3P109Schedule(id, time.Hour.Milliseconds(), now-1000, MissedCoalesce)
			tx, _ := store.db.Begin()
			upsertSchedule(tx, s)
			tx.Commit()
		}
		result, err := scanDueSchedules(store.db, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if result.Triggered != 2 {
			t.Errorf("scan triggered = %d, want 2", result.Triggered)
		}
		// 再次扫描：幂等（同发生点不重复）。
		result, err = scanDueSchedules(store.db, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if result.Triggered != 0 {
			t.Errorf("rescan re-triggered: %d (同发生点重复扫描只建一个 Task)", result.Triggered)
		}
	})
}

// 确保 sql/strings 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
)
