package work

// 验收：Outbox 与受控命令接收端。
//
// 三个测试入口：
//   - TestOutboxCommitBoundary：提交前崩溃没有可投递命令；提交后确认
//     前崩溃可恢复同一命令；命令与状态变化同事务创建。
//   - TestCommandReceiptLost：接收成功后丢确认造成重投，只受理一次；
//     不同 payload 冲突可见；确认丢失按同键有界退避重投。
//   - TestCommandReplayGuard：F02 的命令重投子例不产生第二个逻辑动作；
//     接收端再检查当前工作单元/Operation 状态，已派发或待核对走核对。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupOutboxStore 创建存储与基础任务。
func setupOutboxStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p106-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// outboxCommandFixture 构造一个测试命令。
func outboxCommandFixture(commandID, taskID, payload string) OutboxCommand {
	return OutboxCommand{
		CommandID:     commandID,
		Kind:          "dispatch",
		TargetKind:    "work_unit",
		TargetID:      "unit-1",
		TargetVersion: 1,
		TaskID:        taskID,
		PayloadDigest: digest(payload),
		Backend:       "test-backend",
		CreatedAt:     recordsTestNow(),
	}
}

// TestOutboxCommitBoundary 提交边界。
func TestOutboxCommitBoundary(t *testing.T) {
	t.Run("crash_before_commit_no_command", func(t *testing.T) {
		// 提交前崩溃：事务回滚，没有可投递命令。
		store, taskID := setupOutboxStore(t, "before.db")
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := enqueueOutbox(tx, outboxCommandFixture("cmd-1", taskID, "载荷")); err != nil {
			t.Fatal(err)
		}
		tx.Rollback() // 模拟提交前崩溃（回滚）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM outbox_commands WHERE command_id='cmd-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("command persisted after rollback: count=%d (提交前崩溃不应留下命令)", count)
		}
	})

	t.Run("crash_after_commit_recovers_command", func(t *testing.T) {
		// 提交后确认前崩溃：重启后同一命令仍可恢复投递。
		path := filepath.Join(t.TempDir(), "after.db")
		var taskID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, err := store.Submit(ctx, "v3p106-after", "整理季度汇报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			taskID = run.TaskID
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := enqueueOutbox(tx, outboxCommandFixture("cmd-recover", taskID, "载荷")); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			// 模拟崩溃：Close 不投递。
		}()
		// 重启：命令仍可投递（提交后确认前崩溃可恢复）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var state string
		if err := store.db.QueryRow("SELECT state FROM outbox_commands WHERE command_id='cmd-recover'").Scan(&state); err != nil {
			t.Fatalf("command lost after restart: %v", err)
		}
		if state != "pending" {
			t.Errorf("command state = %s, want pending (可恢复投递)", state)
		}
		_ = taskID
	})

	t.Run("command_created_with_state_change_in_same_tx", func(t *testing.T) {
		// 命令与状态变化同事务创建：要么都提交，要么都回滚。
		store, taskID := setupOutboxStore(t, "sametx.db")
		now := recordsTestNow()
		// 状态变化（Task create）+ 命令创建同事务。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, now); err != nil {
			t.Fatal(err)
		}
		if err := enqueueOutbox(tx, outboxCommandFixture("cmd-atomic", taskID, "载荷")); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// 两者都生效（同事务）。
		var stateExists, cmdExists bool
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM task_states WHERE task_id=?)", taskID).Scan(&stateExists); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox_commands WHERE command_id='cmd-atomic')").Scan(&cmdExists); err != nil {
			t.Fatal(err)
		}
		if !stateExists || !cmdExists {
			t.Errorf("atomic commit incomplete: state=%v cmd=%v", stateExists, cmdExists)
		}
	})
}

// TestCommandReceiptLost 确认丢失与重投。
func TestCommandReceiptLost(t *testing.T) {
	t.Run("receipt_lost_redelivers_but_received_once", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "lost.db")
		tx, _ := store.db.Begin()
		if err := enqueueOutbox(tx, outboxCommandFixture("cmd-lost", taskID, "载荷")); err != nil {
			t.Fatal(err)
		}
		tx.Commit()

		// 受控接收端：第一次接收成功但确认"丢失"（模拟确认未写库）。
		var receiveCount int
		receiver := &DedupReceiver{
			DB: store.db,
			OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
				receiveCount++
				return "receipt-1", nil
			},
		}
		ctx := context.Background()
		// 第一次投递：成功。
		sent, failed, _, err := dispatchDue(ctx, store.db, receiver, "test-backend", recordsTestNow(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if sent != 1 || failed != 0 {
			t.Errorf("first dispatch: sent=%d failed=%d", sent, failed)
		}
		// 模拟确认丢失：手动把状态改回 pending（接收成功但确认未生效）。
		if _, err := store.db.Exec("UPDATE outbox_commands SET state='pending', next_attempt_at=? WHERE command_id='cmd-lost'", recordsTestNow()); err != nil {
			t.Fatal(err)
		}
		// 重投：接收端去重，返回原受理结果（只受理一次）。
		sent, _, _, err = dispatchDue(ctx, store.db, receiver, "test-backend", recordsTestNow(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if receiveCount != 1 {
			t.Errorf("receiver invoked %d times, want 1 (重投只受理一次)", receiveCount)
		}
		// 去重记录存在（覆盖重投期）。
		var receiptCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM outbox_receipts WHERE command_id='cmd-lost'").Scan(&receiptCount); err != nil {
			t.Fatal(err)
		}
		if receiptCount != 1 {
			t.Errorf("receipt records = %d, want 1", receiptCount)
		}
		_ = sent
	})

	t.Run("payload_conflict_visible", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "conflict.db")
		// 接收端已受理过 command_id 但载荷不同：冲突可见（报警）。
		receiver := &DedupReceiver{
			DB: store.db,
			OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
				return "receipt", nil
			},
		}
		ctx := context.Background()
		// 第一次：受理。
		if _, err := receiver.Receive(ctx, outboxCommandFixture("cmd-conflict", taskID, "载荷A")); err != nil {
			t.Fatal(err)
		}
		// 同 ID 不同载荷：冲突。
		conflicting := outboxCommandFixture("cmd-conflict", taskID, "载荷B")
		if _, err := receiver.Receive(ctx, conflicting); err == nil {
			t.Error("payload conflict not visible")
		} else if !strings.Contains(err.Error(), "载荷冲突") {
			t.Errorf("conflict error unclear: %v", err)
		}
	})

	t.Run("failed_delivery_retries_with_backoff", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "retry.db")
		tx, _ := store.db.Begin()
		if err := enqueueOutbox(tx, outboxCommandFixture("cmd-retry", taskID, "载荷")); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 接收端失败：按同键有界退避重投。
		failReceiver := &DedupReceiver{
			DB: store.db,
			OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
				return "", errors.New("接收端暂时不可用")
			},
		}
		ctx := context.Background()
		sent, failed, _, err := dispatchDue(ctx, store.db, failReceiver, "test-backend", recordsTestNow(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if sent != 0 || failed != 1 {
			t.Errorf("failed delivery: sent=%d failed=%d", sent, failed)
		}
		// 失败状态与下次重试时间已保存（不无限重试）。
		var state string
		var nextRetry int64
		if err := store.db.QueryRow("SELECT state, next_attempt_at FROM outbox_commands WHERE command_id='cmd-retry'").Scan(&state, &nextRetry); err != nil {
			t.Fatal(err)
		}
		if state != "failed" {
			t.Errorf("state = %s, want failed", state)
		}
		if nextRetry <= recordsTestNow() {
			t.Errorf("next retry not scheduled with backoff: %d", nextRetry)
		}
	})
}

// TestCommandReplayGuard 命令重投守卫。
func TestCommandReplayGuard(t *testing.T) {
	t.Run("replay_dispatched_goes_to_reconcile_not_restart", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "replay.db")
		// F02：命令重投时，接收端再检查当前状态；已派发/待核对走核对，不重启动作。
		var actualDispatch int
		receiver := &DedupReceiver{
			DB: store.db,
			GuardDispatch: func(ctx context.Context, c OutboxCommand) (bool, error) {
				// 模拟"已派发"：守卫返回已处理。
				return true, nil
			},
			OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
				actualDispatch++
				return "dispatched", nil
			},
		}
		ctx := context.Background()
		receipt, err := receiver.Receive(ctx, outboxCommandFixture("cmd-replay", taskID, "载荷"))
		if err != nil {
			t.Fatal(err)
		}
		// 已派发：走核对路径，不重启动作（不产生第二个逻辑动作）。
		if actualDispatch != 0 {
			t.Errorf("replay restarted action (actualDispatch=%d)", actualDispatch)
		}
		if !strings.HasPrefix(receipt, "reconcile:") {
			t.Errorf("replay receipt = %q, want reconcile path", receipt)
		}
	})

	t.Run("outbox_backlog_observable", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "backlog.db")
		// 填充：pending、failed、dead 各一。
		tx, _ := store.db.Begin()
		enqueueOutbox(tx, outboxCommandFixture("cmd-pending", taskID, "a"))
		tx.Commit()
		tx, _ = store.db.Begin()
		enqueueOutbox(tx, outboxCommandFixture("cmd-failed", taskID, "b"))
		tx.Commit()
		store.db.Exec("UPDATE outbox_commands SET state='failed', next_attempt_at=? WHERE command_id='cmd-failed'", recordsTestNow()+5000)
		tx, _ = store.db.Begin()
		enqueueOutbox(tx, outboxCommandFixture("cmd-dead", taskID, "c"))
		markOutboxDead(tx, "cmd-dead", "不可恢复冲突", recordsTestNow())
		tx.Commit()

		backlog, err := readOutboxBacklog(store.db, recordsTestNow()+1000)
		if err != nil {
			t.Fatal(err)
		}
		if backlog.Pending != 1 || backlog.Failed != 1 || backlog.Dead != 1 {
			t.Errorf("backlog = %+v, want pending=1 failed=1 dead=1", backlog)
		}
		if backlog.NextRetryAt != recordsTestNow()+5000 {
			t.Errorf("next retry = %d, want %d", backlog.NextRetryAt, recordsTestNow()+5000)
		}
		if backlog.OldestPendingMS < 0 {
			t.Errorf("oldest pending age = %d", backlog.OldestPendingMS)
		}
	})

	t.Run("dead_command_not_retried", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "dead.db")
		tx, _ := store.db.Begin()
		enqueueOutbox(tx, outboxCommandFixture("cmd-dead2", taskID, "载荷"))
		if err := markOutboxDead(tx, "cmd-dead2", "不可恢复冲突", recordsTestNow()); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// dead 命令不再投递（不无限重试）。
		receiver := &DedupReceiver{DB: store.db, OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
			return "receipt", nil
		}}
		sent, failed, skipped, err := dispatchDue(context.Background(), store.db, receiver, "test-backend", recordsTestNow()+100000, 10)
		if err != nil {
			t.Fatal(err)
		}
		if sent != 0 || failed != 0 {
			t.Errorf("dead command dispatched: sent=%d failed=%d skipped=%d", sent, failed, skipped)
		}
	})

	t.Run("concurrent_dispatch_single_sender", func(t *testing.T) {
		store, taskID := setupOutboxStore(t, "concurrent.db")
		tx, _ := store.db.Begin()
		enqueueOutbox(tx, outboxCommandFixture("cmd-race", taskID, "载荷"))
		tx.Commit()
		// 并发投递：条件更新保证只有一个投递者取得（skip 其他）。
		var mu sync.Mutex
		var receiveCount int
		receiver := &DedupReceiver{DB: store.db, OnReceive: func(ctx context.Context, c OutboxCommand) (string, error) {
			mu.Lock()
			receiveCount++
			mu.Unlock()
			return "receipt", nil
		}}
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dispatchDue(context.Background(), store.db, receiver, "test-backend", recordsTestNow(), 1)
			}()
		}
		wg.Wait()
		mu.Lock()
		got := receiveCount
		mu.Unlock()
		// 接收端去重 + 条件更新：实际接收至多一次。
		if got > 1 {
			t.Errorf("concurrent dispatch received %d times, want <= 1", got)
		}
	})
}

// 确保 sql/errors 引用保留。
var (
	_ = sql.ErrNoRows
	_ = errors.Is
)
