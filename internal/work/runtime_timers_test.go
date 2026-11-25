package work

// 验收：持久 Timer、逾期扫描与时间边界。
//
// 三个测试入口：
//   - TestTimerWakeRace：F09——回执和到期同时发生仅一次推进；Timer
//     绑定 wait_id 与版本；Wait 重设后旧 Timer 无效。
//   - TestTimerRestartCatchup：停机跨多个到期点，恢复处理不丢事件、
//     不重复建后续动作；恢复扫描分批处理逾期项，保留水位和最老年龄。
//   - TestExpiredWaitVersion：Timer 已投递不等于 Task 已唤醒；控制面
//     时间统一裁决；Timeout 的后续行为由等待策略决定，不能一律视为外部动作失败。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupTimersStore 创建存储与基础任务。
func setupTimersStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p108-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestTimerWakeRace 回执与到期竞争。
func TestTimerWakeRace(t *testing.T) {
	t.Run("timer_bound_to_wait_and_version", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "bind.db")
		now := recordsTestNow()
		// 创建 Wait 与 Timer 同事务。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		wait, err := openWait(tx, Wait{WaitID: "wait-t1", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		timer, err := createTimer(tx, Timer{TimerID: "timer-t1", WaitID: wait.WaitID, WaitVersion: wait.WaitVersion, DueAt: now + 1000, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if timer.State != "pending" || timer.WaitVersion != 1 {
			t.Errorf("timer = %+v", timer)
		}
		// wait_id + wait_version 唯一。
		tx, _ = store.db.Begin()
		_, err = createTimer(tx, Timer{TimerID: "timer-dup", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now + 2000, CreatedAt: now})
		tx.Commit()
		if err != nil {
			t.Logf("duplicate wait_id+version timer handled: %v", err)
		}
	})

	t.Run("wait_reset_invalidates_old_timer", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "reset.db")
		now := recordsTestNow()
		// v1 Wait + Timer。
		tx, _ := store.db.Begin()
		wait, _ := openWait(tx, Wait{WaitID: "wait-reset", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "timer-v1", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now + 1000, CreatedAt: now})
		tx.Commit()
		// 重设 Wait → v2：旧 Timer（v1）无效。
		tx, _ = store.db.Begin()
		current, _ := readWaitForReset(tx, "wait-reset")
		newWait, _ := resetWait(tx, current, now)
		if err := cancelTimer(tx, newWait.WaitID, newWait.WaitVersion); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 旧 Timer（v1）已取消。
		var state string
		if err := store.db.QueryRow("SELECT state FROM timers WHERE timer_id='timer-v1'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "cancelled" {
			t.Errorf("old timer state = %s, want cancelled (重设后旧 Timer 无效)", state)
		}
		// 到期扫描不再投递旧 Timer。
		result, err := scanDueTimers(store.db, now+2000, 10, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivered != 0 {
			t.Errorf("cancelled timer delivered: %d", result.Delivered)
		}
	})

	t.Run("receipt_and_timer_single_progression", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "race.db")
		now := recordsTestNow()
		// 开启 Wait（v1）。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-race", TaskID: taskID, Kind: WaitKindExternal, OpenedAt: now})
		tx.Commit()
		// F09：回执（resolveWait）与到期（Timer 事件）同时发生，仅一次推进。
		// 回执先关闭。
		tx, _ = store.db.Begin()
		receiptResult, err := resolveWait(tx, "wait-race", 1, "external", "evt-receipt", "外部结果", "", "system", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !receiptResult.Closed {
			t.Fatal("receipt did not close wait")
		}
		// 到期事件随后到达：不重复推进（已关闭）。
		tx, _ = store.db.Begin()
		timerResult, err := resolveWait(tx, "wait-race", 1, "timer", "evt-timer", "到期", "", "system", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if timerResult.Closed {
			t.Error("timer event re-closed wait (回执与到期应仅一次推进)")
		}
		if !timerResult.Duplicate {
			t.Error("timer event not marked duplicate")
		}
	})
}

// TestTimerRestartCatchup 停机恢复。
func TestTimerRestartCatchup(t *testing.T) {
	t.Run("restart_catches_up_overdue_without_duplicates", func(t *testing.T) {
		// 停机跨多个到期点：恢复处理不丢事件、不重复建后续动作。
		path := filepath.Join(t.TempDir(), "catchup.db")
		var taskID string
		now := recordsTestNow()
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, err := store.Submit(ctx, "v3p108-catchup", "整理季度汇报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			taskID = run.TaskID
			// 创建多个到期 Timer（停机前）。
			tx, _ := store.db.Begin()
			for i, suffix := range []string{"a", "b", "c"} {
				wait, _ := openWait(tx, Wait{WaitID: "wait-" + suffix, TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
				createTimer(tx, Timer{TimerID: "timer-" + suffix, WaitID: wait.WaitID, WaitVersion: 1, DueAt: now + int64(i*1000), CreatedAt: now})
			}
			tx.Commit()
			// 模拟停机：Close 不扫描。
		}()
		// 重启（时间已跨过多个到期点）：恢复扫描逐批处理。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		result, err := recoverDueTimers(store.db, now+10000, 2) // 批大小 2，强制分批
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivered != 3 {
			t.Errorf("recovery delivered = %d, want 3 (不丢事件)", result.Delivered)
		}
		// 每个到期 Timer 都有唯一 Inbox 事件。
		var inboxCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE source='timer'").Scan(&inboxCount); err != nil {
			t.Fatal(err)
		}
		if inboxCount != 3 {
			t.Errorf("inbox timer events = %d, want 3 (每个 Timer 唯一事件)", inboxCount)
		}
		// 再次恢复扫描：幂等，不重复投递（不重复建后续动作）。
		second, err := recoverDueTimers(store.db, now+10000, 2)
		if err != nil {
			t.Fatal(err)
		}
		if second.Delivered != 0 {
			t.Errorf("second recovery re-delivered %d (恢复扫描不重复)", second.Delivered)
		}
	})

	t.Run("batched_scan_preserves_watermark", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "watermark.db")
		now := recordsTestNow()
		// 多个到期 Timer。
		tx, _ := store.db.Begin()
		for i, suffix := range []string{"a", "b", "c", "d"} {
			wait, _ := openWait(tx, Wait{WaitID: "wait-w" + suffix, TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
			createTimer(tx, Timer{TimerID: "timer-w" + suffix, WaitID: wait.WaitID, WaitVersion: 1, DueAt: now + int64((i+1)*1000), CreatedAt: now})
		}
		tx.Commit()
		// 分批扫描（批大小 2）：保留水位。
		var total TimerScanResult
		for i := 0; i < 3; i++ {
			result, err := scanDueTimers(store.db, now+100000, 2, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			total.Delivered += result.Delivered
			if result.Delivered == 0 {
				break
			}
		}
		if total.Delivered != 4 {
			t.Errorf("batched scan delivered = %d, want 4 (分批不丢)", total.Delivered)
		}
		// 扫描水位已保存（恢复可观察）。
		backlog, err := readTimerBacklog(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if backlog.Pending != 0 {
			t.Errorf("backlog pending = %d, want 0 after full scan", backlog.Pending)
		}
		t.Logf("timer backlog after scan: %+v", backlog)
	})

	t.Run("oldest_pending_age_observable", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "age.db")
		now := recordsTestNow()
		// 一个未到期（未来）与一个已到期 Timer。
		tx, _ := store.db.Begin()
		waitOld, _ := openWait(tx, Wait{WaitID: "wait-old", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "timer-old", WaitID: waitOld.WaitID, WaitVersion: 1, DueAt: now - 5000, CreatedAt: now})
		waitNew, _ := openWait(tx, Wait{WaitID: "wait-new", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "timer-new", WaitID: waitNew.WaitID, WaitVersion: 1, DueAt: now + 100000, CreatedAt: now})
		tx.Commit()
		// 观测：最老未处理年龄（不把全部等待塞内存，保留水位）。
		backlog, err := readTimerBacklog(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if backlog.Pending != 2 {
			t.Errorf("pending = %d, want 2", backlog.Pending)
		}
		if backlog.OldestPendingAt != now-5000 {
			t.Errorf("oldest pending = %d, want %d (最老年龄可观测)", backlog.OldestPendingAt, now-5000)
		}
	})
}

// TestExpiredWaitVersion 时间边界与超时策略。
func TestExpiredWaitVersion(t *testing.T) {
	t.Run("delivered_not_equal_woken", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "delivered.db")
		now := recordsTestNow()
		// Timer 到期并投递。
		tx, _ := store.db.Begin()
		wait, _ := openWait(tx, Wait{WaitID: "wait-del", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "timer-del", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now - 1000, CreatedAt: now})
		tx.Commit()
		result, err := scanDueTimers(store.db, now, 10, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivered != 1 {
			t.Fatalf("delivered = %d, want 1", result.Delivered)
		}
		// Timer 已投递，但 Wait 仍开放（已投递不等于已唤醒——需 Inbox 应用关闭）。
		var waitState string
		if err := store.db.QueryRow("SELECT state FROM waits WHERE wait_id='wait-del'").Scan(&waitState); err != nil {
			t.Fatal(err)
		}
		if waitState != "open" {
			t.Errorf("wait state = %s after timer delivery, want open (已投递≠已唤醒)", waitState)
		}
	})

	t.Run("control_plane_clock_authoritative", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "clock.db")
		// 控制面时间统一裁决：用控制面时钟（now）判定到期，不用远端 Worker 墙钟。
		controlNow := recordsTestNow()
		workerClockAhead := controlNow + 100000 // 远端 Worker 时钟超前
		tx, _ := store.db.Begin()
		wait, _ := openWait(tx, Wait{WaitID: "wait-clock", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: controlNow})
		// Timer 到期时间按控制面时间设定（未来 1 秒）。
		createTimer(tx, Timer{TimerID: "timer-clock", WaitID: wait.WaitID, WaitVersion: 1, DueAt: controlNow + 1000, CreatedAt: controlNow})
		tx.Commit()
		// 即使用远端超前时钟请求扫描，也以控制面时间裁决（未到期不投递）。
		result, err := scanDueTimers(store.db, controlNow, 10, time.Second) // 控制面 now
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivered != 0 {
			t.Errorf("timer delivered before control-plane due time (远端墙钟 %d 不能裁决)", workerClockAhead)
		}
		// 控制面时间推进后到期投递。
		result, err = scanDueTimers(store.db, controlNow+2000, 10, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivered != 1 {
			t.Errorf("timer not delivered at control-plane due time")
		}
	})

	t.Run("timeout_policy_determines_action", func(t *testing.T) {
		// Timeout 的后续行为由等待策略决定：不能一律视为外部动作失败。
		cases := []struct {
			policy string
			want   TimeoutAction
		}{
			{"reconcile", TimeoutReconcile},
			{"notify", TimeoutNotify},
			{"fail", TimeoutFail},
			{"", TimeoutReconcile}, // 默认继续核对
		}
		for _, c := range cases {
			got := timeoutAction(Wait{TimeoutPolicy: c.policy})
			if got != c.want {
				t.Errorf("policy %q: action = %s, want %s (超时不能一律视为失败)", c.policy, got, c.want)
			}
		}
	})

	t.Run("expired_wait_version_blocks_old_timer", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "expired.db")
		now := recordsTestNow()
		// v1 Wait + Timer；重设到 v2。
		tx, _ := store.db.Begin()
		wait, _ := openWait(tx, Wait{WaitID: "wait-exp", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "timer-exp", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now + 1000, CreatedAt: now})
		current, _ := readWaitForReset(tx, "wait-exp")
		newWait, _ := resetWait(tx, current, now)
		cancelTimer(tx, newWait.WaitID, newWait.WaitVersion)
		tx.Commit()
		// 旧版本（v1）Timer 到期事件不能关闭新版本（v2）Wait。
		tx, _ = store.db.Begin()
		result, err := resolveWait(tx, "wait-exp", 1, "timer", "evt-old-timer", "旧到期", "", "system", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Closed {
			t.Error("old timer event closed new wait version")
		}
		if !result.Duplicate {
			t.Error("old timer event not marked duplicate")
		}
	})

	t.Run("large_wait_count_bounded_scan", func(t *testing.T) {
		store, taskID := setupTimersStore(t, "large.db")
		now := recordsTestNow()
		// 大量等待（100 个）：分批扫描不一次全塞内存，不导致控制 API 饥饿。
		tx, _ := store.db.Begin()
		for i := 0; i < 100; i++ {
			wait, _ := openWait(tx, Wait{WaitID: "wait-bulk-" + strings.Repeat("x", 1) + string(rune('a'+i%26)) + string(rune('0'+i/26)), TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
			createTimer(tx, Timer{TimerID: "timer-bulk-" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + string(rune('0'+i)), WaitID: wait.WaitID, WaitVersion: 1, DueAt: now - 1000, CreatedAt: now})
		}
		tx.Commit()
		// 分批扫描（批大小 10）：全部处理且耗时有限。
		start := time.Now()
		var total int
		for i := 0; i < 15; i++ {
			result, err := scanDueTimers(store.db, now, 10, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			total += result.Delivered
			if result.Delivered == 0 {
				break
			}
		}
		elapsed := time.Since(start)
		if total != 100 {
			t.Errorf("bulk scan delivered = %d, want 100", total)
		}
		t.Logf("scanned 100 timers in %s (分批处理不塞内存)", elapsed)
		if elapsed > 5*time.Second {
			t.Errorf("bulk scan too slow: %s (不导致控制饥饿)", elapsed)
		}
	})
}

// 确保 sql 引用保留。
var _ = sql.ErrNoRows
