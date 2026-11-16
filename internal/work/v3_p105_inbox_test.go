package work

// P1-05 验收：通用 Inbox 与幂等应用。
//
// 三个测试入口：
//   - TestV3P105InboxDurableReceive：F01——已确认接收后中断，事件仍在；
//     应用事务回滚后可重试且没有重复状态变更；来源事件唯一约束。
//   - TestV3P105InboxApplyOnce：F03——重复只应用一次；并发处理同事件只产生
//     一个有效结果；应用时核对主体/关联/版本，单事务更新目标、审计、后续
//     Outbox 并标记已处理。
//   - TestV3P105InboxConflictIsolation：同键异内容、无可靠顺序及未知关联
//     不会静默覆盖/丢弃；同 ID 不同摘要隔离并审计；未知关联隔离待查。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupV3P105 创建存储与基础任务。
func setupV3P105(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p105-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// v3P105Event 构造一个测试事件。
func v3P105Event(source, eventID, taskID, summary string) InboxEvent {
	return InboxEvent{
		Source:        source,
		EventID:       eventID,
		Principal:     "user",
		TaskID:        taskID,
		Kind:          "chat_input",
		Summary:       summary,
		PayloadDigest: digest(summary),
		ReceivedAt:    v3P102Now(),
	}
}

// TestV3P105InboxDurableReceive 持久受理与中断恢复。
func TestV3P105InboxDurableReceive(t *testing.T) {
	t.Run("received_event_survives_restart", func(t *testing.T) {
		// F01a：已确认接收后中断，事件仍在。
		path := filepath.Join(t.TempDir(), "durable.db")
		var taskID string
		var eventID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, err := store.Submit(ctx, "v3p105-durable", "整理季度汇报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			taskID = run.TaskID
			eventID = "evt-1"
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			_, err = receiveInbox(tx, v3P105Event("chat", eventID, taskID, "整理季度汇报"))
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			// 模拟中断：Close 不应用事件。
		}()
		// 重启：事件仍在（持久受理不丢）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var state, summary string
		if err := store.db.QueryRow("SELECT state,summary FROM inbox_events WHERE source='chat' AND event_id=?", eventID).
			Scan(&state, &summary); err != nil {
			t.Fatalf("event lost after restart: %v", err)
		}
		if state != "received" {
			t.Errorf("event state = %s, want received (未应用)", state)
		}
		if summary != "整理季度汇报" {
			t.Errorf("event summary drifted: %q", summary)
		}
		_ = taskID
	})

	t.Run("source_event_id_unique", func(t *testing.T) {
		store, taskID := setupV3P105(t, "unique.db")
		// 同 (source, event_id) 第二次插入被唯一约束拒绝（除非走幂等路径）。
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("chat", "evt-dup", taskID, "内容A"))
		tx.Commit()
		// 直接 SQL 重复插入（绕过幂等）：唯一约束拒绝。
		_, err := store.db.Exec(`INSERT INTO inbox_events(source,event_id,principal,kind,summary,payload_digest,received_at,state)
VALUES('chat','evt-dup','user','chat_input','内容B','digest-b',0,'received')`)
		if err == nil {
			t.Error("duplicate (source,event_id) not rejected by unique constraint")
		}
	})

	t.Run("apply_rollback_allows_retry_without_duplicate", func(t *testing.T) {
		store, taskID := setupV3P105(t, "retry.db")
		eventID := "evt-retry"
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("chat", eventID, taskID, "内容"))
		tx.Commit()

		// 应用事务回滚：业务更新与标记一起回滚，事件仍可重试。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		var appliedInTx bool
		_, err = applyInboxEvent(tx, "chat", eventID, func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			// 模拟业务更新后故意失败（回滚）。
			appliedInTx = true
			return ApplyResult{Failure: "模拟失败"}, errors.New("模拟业务失败")
		}, v3P102Now())
		tx.Rollback() // 回滚：不产生重复状态变更。
		if err == nil {
			t.Error("failing applier not propagated")
		}
		// 回滚后事件回到可重试状态（不是 applied）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM inbox_events WHERE source='chat' AND event_id=?", eventID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "applied" {
			t.Error("rolled-back apply marked event applied")
		}
		if !appliedInTx {
			t.Error("applier not invoked")
		}
		// 重试：可再次应用。
		tx, _ = store.db.Begin()
		result, err := applyInboxEvent(tx, "chat", eventID, func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			return ApplyResult{FollowUp: []string{"notify"}}, nil
		}, v3P102Now())
		if err != nil {
			t.Fatalf("retry apply: %v", err)
		}
		tx.Commit()
		if !result.Applied {
			t.Error("retry did not apply")
		}
	})
}

// TestV3P105InboxApplyOnce 幂等应用。
func TestV3P105InboxApplyOnce(t *testing.T) {
	t.Run("duplicate_apply_only_once", func(t *testing.T) {
		store, taskID := setupV3P105(t, "applyonce.db")
		eventID := "evt-once"
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("chat", eventID, taskID, "内容"))
		tx.Commit()

		var applyCount int
		applier := func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			applyCount++
			return ApplyResult{}, nil
		}
		// 第一次应用。
		tx, _ = store.db.Begin()
		if _, err := applyInboxEvent(tx, "chat", eventID, applier, v3P102Now()); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 第二次应用：已 applied 的事件不重复应用（F03）。
		tx, _ = store.db.Begin()
		result, err := applyInboxEvent(tx, "chat", eventID, applier, v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if applyCount != 1 {
			t.Errorf("applier invoked %d times, want 1 (重复只应用一次)", applyCount)
		}
		if !result.Applied {
			t.Error("second apply should report applied (idempotent)")
		}
	})

	t.Run("concurrent_apply_single_effective", func(t *testing.T) {
		store, taskID := setupV3P105(t, "concurrent.db")
		eventID := "evt-race"
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("chat", eventID, taskID, "内容"))
		tx.Commit()

		// 并发处理同事件：单写连接串行化事务，只有第一个提交的能改变状态；
		// 后续提交看到 applied 后幂等返回，不重复应用业务（并发只产生一个
		// 有效结果，其他尝试保留可审计信息）。
		var wg sync.WaitGroup
		var mu sync.Mutex
		var businessApplied int
		applier := func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			mu.Lock()
			businessApplied++
			mu.Unlock()
			return ApplyResult{}, nil
		}
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tx, err := store.db.Begin()
				if err != nil {
					return
				}
				defer tx.Rollback()
				if _, err := applyInboxEvent(tx, "chat", eventID, applier, v3P102Now()); err == nil {
					tx.Commit()
				}
			}()
		}
		wg.Wait()
		// 业务应用函数只被实际执行一次（重复事件只应用一次）。
		mu.Lock()
		got := businessApplied
		mu.Unlock()
		if got != 1 {
			t.Errorf("business applier invoked %d times, want 1 (重复只应用一次)", got)
		}
		// 最终状态是 applied。
		var state string
		if err := store.db.QueryRow("SELECT state FROM inbox_events WHERE source='chat' AND event_id=?", eventID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "applied" {
			t.Errorf("final state = %s, want applied", state)
		}
	})

	t.Run("apply_records_audit_and_followup", func(t *testing.T) {
		store, taskID := setupV3P105(t, "audit.db")
		eventID := "evt-audit"
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("chat", eventID, taskID, "内容"))
		tx.Commit()

		// 应用时登记后续 Outbox 命令（由 applier 返回，调用方处理）。
		tx, _ = store.db.Begin()
		result, err := applyInboxEvent(tx, "chat", eventID, func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			// 在事务内写审计。
			if err := recordAudit(tx, AuditEvent{TaskID: e.TaskID, Event: "chat_applied",
				Actor: e.Principal, Summary: e.Summary, OccurredAt: v3P102Now()}); err != nil {
				return ApplyResult{}, err
			}
			return ApplyResult{FollowUp: []string{"outbox:notify", "outbox:schedule"}}, nil
		}, v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(result.FollowUp) != 2 {
			t.Errorf("follow-up commands = %v, want 2", result.FollowUp)
		}
		// 审计与事件标记同事务生效。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=? AND event='chat_applied'", taskID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("audit records = %d, want 1", auditCount)
		}
	})
}

// TestV3P105InboxConflictIsolation 冲突隔离。
func TestV3P105InboxConflictIsolation(t *testing.T) {
	t.Run("same_id_different_digest_quarantined", func(t *testing.T) {
		store, taskID := setupV3P105(t, "conflict.db")
		// 第一次受理。
		tx, _ := store.db.Begin()
		first, err := receiveInbox(tx, v3P105Event("chat", "evt-conflict", taskID, "内容A"))
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if first.Duplicate || first.Quarantined {
			t.Fatal("first receive should be fresh")
		}
		// 同 ID 不同摘要：隔离并审计，不覆盖原事件。
		tx, _ = store.db.Begin()
		second, err := receiveInbox(tx, v3P105Event("chat", "evt-conflict", taskID, "内容B"))
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !second.Quarantined {
			t.Error("same id different digest not quarantined")
		}
		// 原事件保留（未被覆盖）。
		var summary string
		if err := store.db.QueryRow("SELECT summary FROM inbox_events WHERE source='chat' AND event_id='evt-conflict'").Scan(&summary); err != nil {
			t.Fatal(err)
		}
		if summary != "内容A" {
			t.Errorf("original event overwritten: summary=%q", summary)
		}
		// 冲突已审计。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='inbox_conflict'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("conflict not audited: count=%d", auditCount)
		}
	})

	t.Run("unknown_association_quarantined_not_dropped", func(t *testing.T) {
		store, _ := setupV3P105(t, "unknown.db")
		// 关联不存在的任务：隔离待查，不静默丢弃。
		eventID := "evt-unknown"
		tx, _ := store.db.Begin()
		if _, err := receiveInbox(tx, v3P105Event("chat", eventID, "nonexistent-task", "内容")); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 应用：未知关联 → 隔离。
		tx, _ = store.db.Begin()
		result, err := applyInboxEvent(tx, "chat", eventID, func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			return ApplyResult{}, nil
		}, v3P102Now())
		tx.Rollback() // 隔离后回滚（不提交业务变更）
		if err == nil {
			t.Error("unknown association not rejected")
		}
		if result.Failure == "" || !strings.Contains(result.Failure, "未知关联") {
			t.Errorf("unknown association reason missing: %+v", result)
		}
		// 事件保留（可查询，不丢弃）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM inbox_events WHERE source='chat' AND event_id=?", eventID).Scan(&state); err != nil {
			t.Fatalf("unknown event dropped: %v", err)
		}
		t.Logf("unknown association event state=%s (隔离待查，不丢弃)", state)
	})

	t.Run("no_global_order_not_overwrite_by_arrival", func(t *testing.T) {
		store, taskID := setupV3P105(t, "order.db")
		// 无全局可信顺序：两个不同来源的事件各自独立受理，不以最后到达覆盖。
		tx, _ := store.db.Begin()
		receiveInbox(tx, v3P105Event("source-a", "evt-1", taskID, "来自A"))
		tx.Commit()
		tx, _ = store.db.Begin()
		receiveInbox(tx, v3P105Event("source-b", "evt-1", taskID, "来自B"))
		tx.Commit()
		// 两个事件都保留（source 是主键的一部分，不按到达顺序覆盖）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE event_id='evt-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("events by arrival order overwritten: count=%d, want 2", count)
		}
	})

	t.Run("chat_input_adapted_to_common_chain", func(t *testing.T) {
		store, taskID := setupV3P105(t, "adapt.db")
		// Chat 输入适配为公共 Inbox 事件（P1-05.5）。
		tx, _ := store.db.Begin()
		result, err := AdaptChatInput(tx, "chat", "req-key-1", "user", taskID, "整理汇报", digest("整理汇报"), v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Duplicate || result.Quarantined {
			t.Error("fresh chat input marked duplicate/quarantined")
		}
		// 同键重复提交：返回既有受理（幂等，避免新旧表各自确认）。
		tx, _ = store.db.Begin()
		again, err := AdaptChatInput(tx, "chat", "req-key-1", "user", taskID, "整理汇报", digest("整理汇报"), v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !again.Duplicate {
			t.Error("repeated chat input not idempotent")
		}
	})
}

// 确保 sql/errors/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = context.Background
)
