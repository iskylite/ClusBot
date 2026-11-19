package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// consumeTimerInbox only accepts events backed by the system's own timer ledger.
// External events and legacy chat are deliberately not consumed by this loop.
func (s *Store) consumeTimerInbox(ctx context.Context, stamp int64, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id FROM inbox_events
WHERE source='timer' AND kind='timer_due' AND state IN ('received','failed')
AND next_retry_at<=? ORDER BY received_at,event_id LIMIT ?`, stamp, limit)
	if err != nil {
		return 0, err
	}
	var keys []string
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
	applied := 0
	for _, key := range keys {
		didApply, err := s.consumeTimerEvent(ctx, key, stamp)
		if err != nil {
			return applied, err
		}
		if didApply {
			applied++
		}
	}
	return applied, nil
}

func (s *Store) consumeTimerEvent(ctx context.Context, key string, stamp int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	e, err := readInboxForUpdate(tx, "timer", key)
	if err != nil {
		return false, err
	}
	if e.State != "received" && e.State != "failed" {
		return false, nil
	}
	var timerID, waitID string
	var version int
	var dueAt int64
	err = tx.QueryRow(`SELECT timer_id,wait_id,wait_version,due_at FROM timers
WHERE timer_id=? AND inbox_event_id=? AND state='delivered'`, timerKeyID(key), key).
		Scan(&timerID, &waitID, &version, &dueAt)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, err
	case errors.Is(err, sql.ErrNoRows) || e.Principal != "system" || e.Kind != "timer_due" ||
		e.WaitID != waitID || e.TaskID != "" || e.StepID != "" || e.RunID != "" || e.OperationID != "" ||
		key != "timer:"+timerID || e.PayloadDigest != digest(timerID+waitID):
		return false, quarantineTimerEvent(tx, key, "到期事件与本地 Timer 身份不匹配", stamp)
	case stamp < dueAt:
		// Wall clock moved backwards after delivery. The same event becomes
		// eligible again at its persisted due time.
		if _, err := tx.Exec(`UPDATE inbox_events SET next_retry_at=? WHERE source='timer' AND event_id=?`, dueAt, key); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	var current Wait
	err = tx.QueryRow(`SELECT task_id,kind,timeout_policy,wait_version,state FROM waits WHERE wait_id=?`, waitID).
		Scan(&current.TaskID, &current.Kind, &current.TimeoutPolicy, &current.WaitVersion, &current.State)
	if errors.Is(err, sql.ErrNoRows) {
		return false, quarantineTimerEvent(tx, key, "找不到 Timer 绑定的 Wait", stamp)
	}
	if err != nil {
		return false, err
	}
	current.WaitID = waitID
	action := "wait_recheck"
	reason := "关闭开放版本"
	followup := false
	if current.Kind == WaitKindTimer {
		res, err := resolveWait(tx, waitID, version, e.Source, e.EventID, action, "", "system", stamp)
		if err != nil {
			return false, err
		}
		reason, followup = res.Reason, res.Closed
	} else {
		// A deadline is not evidence that a dependency, decision or external
		// operation was resolved. Keep the wait open, so both step eligibility
		// and a subsequent authentic result still see the same version.
		action = businessTimeoutAction(current)
		reason = "等待已到期，仍需实际结果或明确决定"
		switch {
		case current.WaitVersion != version:
			reason = "等待版本已替换，忽略旧计时器"
		case current.State != "open":
			reason = "等待已由其他事件关闭，忽略到期"
		default:
			followup = true
		}
		if _, err := tx.Exec(`INSERT INTO wait_events(wait_id,wait_version,source,event_id,closed_wait,summary,occurred_at)
VALUES(?,?,?,?,0,?,?) ON CONFLICT(source,event_id,wait_id) DO NOTHING`,
			waitID, version, e.Source, e.EventID, reason+"："+action, stamp); err != nil {
			return false, err
		}
	}
	if followup {
		if err := enqueueOutbox(tx, OutboxCommand{
			CommandID: e.EventID + ":follow-up", Kind: action, TargetKind: "wait",
			TargetID: waitID, TargetVersion: version, TaskID: current.TaskID,
			PayloadDigest: digest(fmt.Sprintf("%q:%d:%s", waitID, version, action)),
			Backend:       "runtime-wait", CreatedAt: stamp, NextAttemptAt: stamp,
		}); err != nil {
			return false, err
		}
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: current.TaskID, Event: "timer_due_processed", Actor: "system",
		Summary: reason + "：" + action, OccurredAt: stamp,
	}); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE inbox_events SET state='applied',applied_at=?,
attempts=attempts+1,failure='',next_retry_at=0 WHERE source='timer' AND event_id=?`, stamp, key); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Route first by wait class; an external operation is never treated as
// definitively failed by a wall clock, and a human decision is never supplied
// by one. The outbox records intent only, not an executed policy or message.
func businessTimeoutAction(wait Wait) string {
	switch wait.Kind {
	case WaitKindExternal:
		return "wait_timeout_reconcile"
	case WaitKindInput, WaitKindAcceptance, WaitKindPublication:
		return "wait_timeout_notify"
	default:
		return "wait_timeout_" + string(timeoutAction(wait))
	}
}

func timerKeyID(key string) string {
	const prefix = "timer:"
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return ""
	}
	return key[len(prefix):]
}

func quarantineTimerEvent(tx *sql.Tx, key, reason string, stamp int64) error {
	if _, err := tx.Exec(`UPDATE inbox_events SET state='quarantined',quarantine_reason=?
WHERE source='timer' AND event_id=?`, reason, key); err != nil {
		return err
	}
	if err := recordAudit(tx, AuditEvent{Event: "timer_due_quarantined", Actor: "system", Summary: reason, OccurredAt: stamp}); err != nil {
		return err
	}
	return tx.Commit()
}
