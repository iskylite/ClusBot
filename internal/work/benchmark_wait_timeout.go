package work

import (
	"context"
	"fmt"
	"strings"
)

// applyBenchmarkTimeouts turns an authenticated timer disposition into a
// local scheduling signal for an existing job lookup/poll. It does not call
// the provider, change an operation's outcome, or resolve the wait.
func (s *Store) applyBenchmarkTimeouts(ctx context.Context, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.command_id,c.target_id,c.target_version,c.payload_digest,
b.step_id,b.task_id,b.operation_id,b.phase,b.call_until,r.stop_requested,
w.match_key,w.wait_version,w.state,o.state
FROM outbox_commands c
JOIN waits w ON w.wait_id=c.target_id AND w.task_id=c.task_id AND w.kind='external'
JOIN benchmark_cases b ON b.task_id=w.task_id AND b.step_id=w.step_id AND w.wait_id='benchmark:'||b.step_id
JOIN benchmark_runs r ON r.task_id=b.task_id
JOIN operations o ON o.operation_id=b.operation_id AND o.task_id=b.task_id AND o.step_id=b.step_id
WHERE c.backend='runtime-wait' AND c.kind='wait_timeout_reconcile' AND c.target_kind='wait'
AND c.state IN ('pending','failed') AND c.next_attempt_at<=?
ORDER BY c.next_attempt_at,c.command_id LIMIT 50`, stamp)
	if err != nil {
		return err
	}
	type candidate struct {
		key, waitID, fingerprint, step, task, operation, phase, matchKey, waitState, opState string
		version, currentVersion                                                              int
		leaseUntil                                                                           int64
		stopping                                                                             bool
	}
	var list []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.key, &c.waitID, &c.version, &c.fingerprint, &c.step, &c.task, &c.operation,
			&c.phase, &c.leaseUntil, &c.stopping, &c.matchKey, &c.currentVersion, &c.waitState, &c.opState); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range list {
		event, suffix := strings.CutSuffix(c.key, ":follow-up")
		trusted := false
		if suffix && timerKeyID(event) != "" && c.version > 0 &&
			c.fingerprint == digest(fmt.Sprintf("%q:%d:wait_timeout_reconcile", c.waitID, c.version)) {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM timers d JOIN inbox_events i ON i.source='timer' AND i.event_id=d.inbox_event_id
JOIN wait_events e ON e.source=i.source AND e.event_id=i.event_id AND e.wait_id=d.wait_id AND e.wait_version=d.wait_version
WHERE d.timer_id=? AND d.inbox_event_id=? AND d.wait_id=? AND d.wait_version=? AND d.state='delivered'
AND i.principal='system' AND i.kind='timer_due' AND i.state='applied' AND i.wait_id=d.wait_id
AND i.payload_digest=? AND e.closed_wait=0
)`, timerKeyID(event), event, c.waitID, c.version, digest(timerKeyID(event)+c.waitID)).Scan(&trusted); err != nil {
				return err
			}
		}
		disposition, detail := "acknowledged", "等待已替换、停止或有终态证据，不改变查询"
		switch {
		case !trusted:
			disposition, detail = "dead", "基准超时缺少同版本计时证据"
		case c.version != c.currentVersion || c.waitState != "open" || c.stopping:
		case c.matchKey != c.operation:
			disposition, detail = "dead", "外部等待不匹配本次基准操作"
		case (c.phase == "following" || c.phase == "submitting") &&
			(c.opState == string(V3OperationDispatched) || c.opState == string(V3OperationRunning) || c.opState == string(V3OperationReconcile)):
			if c.leaseUntil > stamp {
				if _, err := tx.ExecContext(ctx, "UPDATE outbox_commands SET next_attempt_at=? WHERE command_id=?", c.leaseUntil, c.key); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE benchmark_cases SET next_at=min(next_at,?) WHERE step_id=?", stamp, c.step); err != nil {
				return err
			}
			detail = "仅提前查询已存在的作业，未重发、未确认结果"
		}
		failure := ""
		if disposition == "acknowledged" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_receipts(command_id,receiver,payload_digest,receipt,received_at)
VALUES(?,'benchmark-timeout',?,?,?)`, c.key, c.fingerprint, detail, stamp); err != nil {
				return err
			}
		} else {
			failure = detail
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_commands SET state=?,failure=?,attempts=attempts+1,acknowledged_at=?
WHERE command_id=?`, disposition, failure, stamp, c.key); err != nil {
			return err
		}
		if err := recordAudit(tx, AuditEvent{TaskID: c.task, StepID: c.step, WaitID: c.waitID, CommandID: c.key,
			Event: "benchmark_timeout_reconcile", Actor: "system", DetailRef: c.operation, Summary: detail, OccurredAt: stamp}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
