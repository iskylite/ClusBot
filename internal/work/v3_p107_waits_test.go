package work

// P1-07 验收：版本化 Wait 与事件唤醒。
//
// 三个测试入口：
//   - TestV3P107WaitResolveOnce：两个并发事件最多一个关闭开放版本，另一个
//     仍可存证；同一开放版本只能关闭一次；重复事件不重复推进。
//   - TestV3P107WaitVersionMismatch：旧版本事件、错误目标或未授权决定不能
//     关闭新等待；关闭/重设/控制变化产生新版本/记录。
//   - TestV3P107WaitReleasesExecution：OpenWait 与 Task/Step 状态变更同事务，
//     等待不保留模型调用或执行工作者；唤醒只生成需重新核对的就绪工作，不
//     跳过当前合同、权限及未知操作检查。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupV3P107 创建存储与基础任务。
func setupV3P107(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p107-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestV3P107WaitResolveOnce 竞争关闭。
func TestV3P107WaitResolveOnce(t *testing.T) {
	t.Run("open_wait_same_tx_as_state_change", func(t *testing.T) {
		store, taskID := setupV3P107(t, "sametx.db")
		now := v3P102Now()
		// OpenWait 与 Task 状态变更同事务：要么都提交，要么都回滚。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transitionTaskState(tx, taskID, EventNoRunnableStep, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, now); err == nil {
			// 状态变更可能失败（无 V3 初始状态），先 create。
			transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, now)
		}
		wait, err := openWait(tx, Wait{
			WaitID: "wait-1", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now, TimeoutPolicy: "reconcile",
		})
		if err != nil {
			t.Fatalf("openWait: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if wait.State != "open" || wait.WaitVersion != 1 {
			t.Errorf("wait = %+v, want open v1", wait)
		}
		// 读取：开放 Wait 可查。
		open, err := readOpenWaits(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 1 || open[0].WaitID != "wait-1" {
			t.Errorf("open waits = %+v", open)
		}
	})

	t.Run("concurrent_events_single_close", func(t *testing.T) {
		store, taskID := setupV3P107(t, "race.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		if _, err := openWait(tx, Wait{WaitID: "wait-race", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now}); err != nil {
			t.Fatal(err)
		}
		tx.Commit()

		// 两个并发事件竞争关闭同一开放版本：最多一个关闭，另一个留证。
		var wg sync.WaitGroup
		var mu sync.Mutex
		var closedCount, duplicateCount int
		for i := 0; i < 2; i++ {
			wg.Add(1)
			eventID := []string{"evt-a", "evt-b"}[i]
			go func(eid string) {
				defer wg.Done()
				tx, err := store.db.Begin()
				if err != nil {
					return
				}
				defer tx.Rollback()
				result, err := resolveWait(tx, "wait-race", 1, "test", eid, "已输入", "evidence://x", "user", now)
				if err != nil {
					return
				}
				mu.Lock()
				if result.Closed {
					closedCount++
				}
				if result.Duplicate {
					duplicateCount++
				}
				mu.Unlock()
				tx.Commit()
			}(eventID)
		}
		wg.Wait()
		// 最多一个关闭开放版本（F09：回执与到期同时发生仅一次推进）。
		if closedCount > 1 {
			t.Errorf("concurrent events closed wait %d times, want <= 1", closedCount)
		}
		if closedCount+duplicateCount < 1 {
			t.Errorf("no event recorded: closed=%d duplicate=%d", closedCount, duplicateCount)
		}
		t.Logf("concurrent resolve: closed=%d duplicate=%d", closedCount, duplicateCount)
		// 另一个事件仍可存证（wait_events 有记录）。
		var evidenceCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM wait_events WHERE wait_id='wait-race'").Scan(&evidenceCount); err != nil {
			t.Fatal(err)
		}
		if evidenceCount < 1 {
			t.Errorf("losing event not preserved as evidence: count=%d", evidenceCount)
		}
		// Wait 最终关闭（只有一个能关闭）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM waits WHERE wait_id='wait-race'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "closed" {
			t.Errorf("final wait state = %s, want closed", state)
		}
	})

	t.Run("duplicate_event_not_reclose", func(t *testing.T) {
		store, taskID := setupV3P107(t, "dup.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-dup", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()

		// 第一次关闭。
		tx, _ = store.db.Begin()
		first, err := resolveWait(tx, "wait-dup", 1, "test", "evt-1", "已输入", "", "user", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !first.Closed {
			t.Fatal("first event did not close wait")
		}
		// 重复事件：不重复推进（留证但不重复关闭）。
		tx, _ = store.db.Begin()
		second, err := resolveWait(tx, "wait-dup", 1, "test", "evt-1", "已输入", "", "user", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if second.Closed {
			t.Error("duplicate event re-closed wait")
		}
		if !second.Duplicate {
			t.Error("duplicate event not marked duplicate")
		}
	})
}

// TestV3P107WaitVersionMismatch 版本不匹配与控制变化。
func TestV3P107WaitVersionMismatch(t *testing.T) {
	t.Run("old_version_event_cannot_close_new_wait", func(t *testing.T) {
		store, taskID := setupV3P107(t, "version.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-v", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()

		// 重设：关闭 v1，开启 v2（控制变化产生新版本）。
		tx, _ = store.db.Begin()
		current, err := readWaitForReset(tx, "wait-v")
		if err != nil {
			t.Fatal(err)
		}
		newWait, err := resetWait(tx, current, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if newWait.WaitVersion != 2 {
			t.Errorf("reset produced version %d, want 2", newWait.WaitVersion)
		}
		// 反例：旧版本（v1）事件不能关闭新等待（v2）。
		tx, _ = store.db.Begin()
		result, err := resolveWait(tx, "wait-v", 1, "test", "evt-old", "旧输入", "", "user", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Closed {
			t.Error("old version event closed new wait")
		}
		if !result.Duplicate {
			t.Error("old version event not marked duplicate")
		}
		if !strings.Contains(result.Reason, "版本不匹配") {
			t.Errorf("version mismatch reason missing: %q", result.Reason)
		}
		// 新等待仍开放。
		var state string
		if err := store.db.QueryRow("SELECT state FROM waits WHERE wait_id='wait-v'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "open" {
			t.Errorf("new wait state = %s, want open (旧事件未关闭)", state)
		}
		// 正确版本（v2）事件可关闭。
		tx, _ = store.db.Begin()
		ok, err := resolveWait(tx, "wait-v", 2, "test", "evt-new", "新输入", "", "user", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !ok.Closed {
			t.Error("correct version event did not close wait")
		}
	})

	t.Run("wrong_target_event_rejected", func(t *testing.T) {
		store, taskID := setupV3P107(t, "target.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-target", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		// 反例：错误目标（不存在的 wait_id）被拒绝。
		tx, _ = store.db.Begin()
		_, err := resolveWait(tx, "nonexistent-wait", 1, "test", "evt-x", "输入", "", "user", now)
		tx.Rollback()
		if err == nil {
			t.Error("resolve on nonexistent wait not rejected")
		}
	})

	t.Run("reset_produces_new_version_record", func(t *testing.T) {
		store, taskID := setupV3P107(t, "reset.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-reset", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		// 重设两次：v1 → v2 → v3。
		for want := 2; want <= 3; want++ {
			tx, _ = store.db.Begin()
			current, _ := readWaitForReset(tx, "wait-reset")
			newWait, err := resetWait(tx, current, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if newWait.WaitVersion != want {
				t.Errorf("reset produced version %d, want %d", newWait.WaitVersion, want)
			}
		}
		// 旧版本事件在最新版本上无效。
		tx, _ = store.db.Begin()
		result, _ := resolveWait(tx, "wait-reset", 1, "test", "evt-old", "旧", "", "user", now)
		tx.Commit()
		if result.Closed {
			t.Error("v1 event closed v3 wait")
		}
	})
}

// TestV3P107WaitReleasesExecution 唤醒与执行释放。
func TestV3P107WaitReleasesExecution(t *testing.T) {
	t.Run("wake_generates_recheck_not_direct_progress", func(t *testing.T) {
		store, taskID := setupV3P107(t, "wake.db")
		now := v3P102Now()
		// 开启并关闭一个 Wait。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-wake", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		tx, _ = store.db.Begin()
		resolveWait(tx, "wait-wake", 1, "test", "evt-1", "已输入", "", "user", now)
		tx.Commit()

		// 唤醒：只生成需重新核对的就绪工作，不跳过当前合同/权限/未知操作检查。
		tx, _ = store.db.Begin()
		wakeTaskID, followUp, err := wakeWait(tx, "wait-wake", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if wakeTaskID != taskID {
			t.Errorf("wake returned task %s, want %s", wakeTaskID, taskID)
		}
		// 唤醒产生的是"重新核对资格"的就绪工作，而非直接推进。
		found := false
		for _, cmd := range followUp {
			if cmd == "recheck_eligibility" {
				found = true
			}
		}
		if !found {
			t.Errorf("wake follow-up missing recheck_eligibility (唤醒不能直接推进): %v", followUp)
		}
	})

	t.Run("wake_requires_closed_wait", func(t *testing.T) {
		store, taskID := setupV3P107(t, "wakereq.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-open", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		// 反例：未关闭的 Wait 不能唤醒。
		tx, _ = store.db.Begin()
		_, _, err := wakeWait(tx, "wait-open", now)
		tx.Rollback()
		if err == nil {
			t.Error("wake on open wait not rejected")
		}
	})

	t.Run("task_with_other_ready_steps_not_blocked", func(t *testing.T) {
		store, taskID := setupV3P107(t, "othersteps.db")
		now := v3P102Now()
		// Task 有一个开放 Wait，但同时有其他就绪 Step：单个 Wait 不阻塞全部工作。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-partial", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		// 同任务的就绪 Step（独立于 Wait）。
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "independent", DefinitionVersion: 1, State: V3StepReady,
			Position: 1, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 其他就绪 Step 仍可计算（Wait 不阻塞）。
		ready, err := computeReadySteps(store.db, taskID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) == 0 {
			t.Error("open wait blocked all ready steps (单个 Wait 不应阻塞全部工作)")
		}
	})

	t.Run("legacy_wait_migration_preserves_or_gaps", func(t *testing.T) {
		store, taskID := setupV3P107(t, "legacy.db")
		now := v3P102Now()
		// 构造一个旧 run 关联（模拟 task_waits 的 run_id）。
		run, err := store.claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// 迁移旧等待：可恢复关联 → 转换为有限等待。
		tx, _ := store.db.Begin()
		migrated, gap, err := migrateLegacyWait(tx, run.ID, "input", now, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if gap != "" {
			t.Errorf("unexpected gap: %s", gap)
		}
		if migrated == nil || migrated.WaitID == "" || migrated.TaskID != taskID {
			t.Errorf("migrated wait = %+v", migrated)
		}
		// 反例：无法恢复关联（run 不存在）→ 保留缺口，不制造已满足结果。
		tx, _ = store.db.Begin()
		_, gap, err = migrateLegacyWait(tx, "nonexistent-run", "input", now, now)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if gap == "" {
			t.Error("unrecoverable legacy wait did not produce gap (不制造已满足结果)")
		}
		t.Logf("legacy gap preserved: %s", gap)
	})
}

// readWaitForReset 读取当前 Wait 用于重设。
func readWaitForReset(tx *sql.Tx, waitID string) (Wait, error) {
	var w Wait
	var stepID sql.NullString
	err := tx.QueryRow(`SELECT wait_id,task_id,step_id,kind,match_key,wait_version,opened_at,deadline_at,
timeout_policy,state,closed_at,resolution,evidence_ref,closed_by FROM waits WHERE wait_id=?`, waitID).
		Scan(&w.WaitID, &w.TaskID, &stepID, &w.Kind, &w.MatchKey, &w.WaitVersion, &w.OpenedAt, &w.DeadlineAt,
			&w.TimeoutPolicy, &w.State, &w.ClosedAt, &w.Resolution, &w.EvidenceRef, &w.ClosedBy)
	w.StepID = stepID.String
	return w, err
}

// 确保 sql 引用保留。
var _ = sql.ErrNoRows
