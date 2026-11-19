package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// consumeTimerWaitFollowups only delivers local time-wait rechecks. An
// external/approval timeout is not a fulfilled condition and must be handled
// by its own policy consumer, not used as a generic task resume.
func (s *Store) consumeTimerWaitFollowups(ctx context.Context, stamp int64, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT command_id FROM outbox_commands
WHERE backend='runtime-wait' AND kind='wait_recheck' AND state IN ('pending','failed')
AND next_attempt_at<=? ORDER BY next_attempt_at,command_id LIMIT ?`, stamp, limit)
	if err != nil {
		return 0, err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, key := range keys {
		done, err := s.applyTimerWaitFollowup(ctx, key, stamp)
		if err != nil {
			return count, err
		}
		if done {
			count++
		}
	}
	return count, nil
}

func (s *Store) applyTimerWaitFollowup(ctx context.Context, key string, stamp int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var waitID, task, fingerprint, targetKind string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT target_kind,target_id,target_version,coalesce(task_id,''),payload_digest
FROM outbox_commands WHERE command_id=? AND backend='runtime-wait' AND kind='wait_recheck'
AND state IN ('pending','failed') AND next_attempt_at<=?`, key, stamp).
		Scan(&targetKind, &waitID, &version, &task, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// The local consumer and its durable receipt commit together. In
	// particular, there is no interval in which the task was awakened
	// but the outbox delivery could be lost or applied a second time.
	finish := func(receipt string, dead bool) (bool, error) {
		state, failure := "acknowledged", ""
		if dead {
			state, failure = "dead", receipt
		} else if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_receipts(command_id,receiver,payload_digest,receipt,received_at)
VALUES(?,'runtime-timer-wait',?,?,?)`, key, fingerprint, receipt, stamp); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_commands SET state=?,failure=?,attempts=attempts+1,acknowledged_at=?
WHERE command_id=?`, state, failure, stamp, key); err != nil {
			return false, err
		}
		if err := recordAudit(tx, AuditEvent{TaskID: task, Event: "timer_wait_followup", Actor: "system", Summary: receipt, OccurredAt: stamp}); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}

	event, ok := strings.CutSuffix(key, ":follow-up")
	if !ok || targetKind != "wait" || version < 1 || task == "" ||
		fingerprint != digest(fmt.Sprintf("%q:%d:wait_recheck", waitID, version)) {
		return finish("时间等待命令标识不匹配", true)
	}
	var trusted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM timers d JOIN inbox_events i ON i.source='timer' AND i.event_id=d.inbox_event_id
 JOIN wait_events e ON e.source=i.source AND e.event_id=i.event_id AND e.wait_id=d.wait_id AND e.wait_version=d.wait_version
 WHERE d.inbox_event_id=? AND d.wait_id=? AND d.wait_version=? AND d.state='delivered'
 AND i.principal='system' AND i.kind='timer_due' AND i.state='applied' AND e.closed_wait=1
)`, event, waitID, version).Scan(&trusted); err != nil {
		return false, err
	}
	if !trusted {
		return finish("没有本地计时器的有效关闭证据", true)
	}
	var currentVersion int
	var actualTask, step, kind, state, resolution string
	err = tx.QueryRowContext(ctx, "SELECT task_id,coalesce(step_id,''),wait_version,kind,state,resolution FROM waits WHERE wait_id=?", waitID).
		Scan(&actualTask, &step, &currentVersion, &kind, &state, &resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return finish("原等待已移除，不推进任务", false)
	}
	if err != nil {
		return false, err
	}
	if actualTask != task || kind != string(WaitKindTimer) {
		return finish("时间等待与任务或类型不匹配", true)
	}
	if version != currentVersion || state != "closed" || resolution != "wait_recheck" {
		return finish("已替换的等待，不推进任务", false)
	}
	cur, err := readTaskState(tx, task)
	if errors.Is(err, sql.ErrNoRows) {
		return finish("缺少任务的运行时状态，无法安全推进", true)
	}
	if err != nil {
		return false, err
	}
	deferUntil := func(reason string) (bool, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_commands SET state='failed',next_attempt_at=?,failure=?
WHERE command_id=?`, stamp+5000, reason, key); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if cur.State == V3TaskPaused {
		return deferUntil("任务已暂停，等待用户恢复")
	}
	if cur.State != V3TaskWaiting && cur.State != V3TaskPending && cur.State != V3TaskRunning {
		return finish("任务不是可正常唤醒的进行态，不改变任务状态", false)
	}
	var legacy string
	var openWait, unresolved bool
	if err := tx.QueryRowContext(ctx, `SELECT status,
EXISTS(SELECT 1 FROM waits WHERE task_id=tasks.id AND state='open'),
EXISTS(SELECT 1 FROM operations WHERE task_id=tasks.id AND state IN ('dispatched','running','reconcile'))
FROM tasks WHERE id=?`, task).Scan(&legacy, &openWait, &unresolved); err != nil {
		return false, err
	}
	if legacy != "active" {
		return finish("任务受其他控制条件限制，不推进任务", false)
	}
	if cur.ReconcileRequired || unresolved {
		// Do not re-dispatch the operation or clear its uncertainty.
		return deferUntil("仍有外部操作待核实")
	}
	if step != "" {
		var currentStep bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM steps WHERE step_id=? AND task_id=?
AND contract_revision=(SELECT max(revision) FROM task_revisions WHERE task_id=?))`, step, task, task).Scan(&currentStep); err != nil {
			return false, err
		}
		if !currentStep {
			return finish("步骤已被替换或删除，不推进新合同", false)
		}
		// Only this exact step becomes eligible for dependency checks;
		// a timer is not evidence, authorization or a direct dispatch.
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='blocked',updated_at=?
WHERE step_id=? AND task_id=? AND state='waiting'
AND contract_revision=(SELECT max(revision) FROM task_revisions WHERE task_id=?)
AND NOT EXISTS(SELECT 1 FROM waits WHERE task_id=? AND state='open' AND (step_id IS NULL OR step_id=?))`,
			stamp, step, task, task, task, step); err != nil {
			return false, err
		}
	}
	if openWait || cur.State != V3TaskWaiting {
		return finish("时间条件已记账；不改变当前任务状态", false)
	}
	if _, err := transitionTaskState(tx, task, EventWaitResolved,
		TaskGuard{Actor: "system", ExpectedStorageRevision: cur.StorageRevision}, stamp); err != nil {
		return false, err
	}
	return finish("时间条件满足；已交还运行时重新检查就绪条件", false)
}
