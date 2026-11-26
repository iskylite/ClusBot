package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	accountBenchmarkHintPrefix = "benchmark-account-http:"
	accountBenchmarkHintKind   = "benchmark_account_job_hint"
	accountBenchmarkHintTTL    = int64(24 * time.Hour / time.Millisecond)
	accountBenchmarkRetryMS    = int64(10 * time.Second / time.Millisecond)
)

// Internal only: production does not register an account webhook. The sender
// must sign audience + newline + timestamp + newline + body, using the
// independent provider callback secret. Audience is derived from the
// configured endpoint, never from a callback header or request body.
func (e *BenchmarkEventReceiver) receiveAccountHint(ctx context.Context, instance *Store, timestamp, signature string, body []byte) (bool, error) {
	if e == nil || e.jobSource == "" || instance == nil || instance.benchmarkInstance != nil {
		return false, ErrAuthorizationDenied
	}
	stamp := now()
	source := accountBenchmarkHintPrefix + e.jobSource
	hint, err := e.authenticateHint(timestamp, signature, body, stamp, source)
	if err != nil {
		return false, err
	}
	tx, err := instance.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Serialize legitimate ingress with other instance writes. Retain old
	// receipts longer than the signature window, including applied receipts.
	if _, err := tx.ExecContext(ctx, "DELETE FROM inbox_events WHERE source=? AND kind=? AND received_at<?",
		source, accountBenchmarkHintKind, stamp-accountBenchmarkHintTTL); err != nil {
		return false, err
	}
	var isInstance bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_instance WHERE mode='server')
 AND NOT EXISTS(SELECT 1 FROM account_business_owner)`).Scan(&isInstance); err != nil {
		return false, err
	}
	if !isInstance {
		return false, ErrAuthorizationDenied
	}
	rec, err := receiveInbox(tx, InboxEvent{Source: source, EventID: hint.EventID, Principal: source, Kind: accountBenchmarkHintKind,
		PayloadDigest: digest(toJSON(hint)), PayloadRef: hint.JobID, ReceivedAt: stamp})
	if err != nil {
		return false, err
	}
	if rec.Quarantined {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, ErrConflict
	}
	if !rec.Duplicate {
		var full bool
		if err := tx.QueryRowContext(ctx, "SELECT count(*)>1000 FROM inbox_events WHERE source=? AND kind=? AND state='received'",
			source, accountBenchmarkHintKind).Scan(&full); err != nil {
			return false, err
		}
		if full {
			return false, ErrState
		}
	}
	return rec.Duplicate, tx.Commit()
}

type accountBenchmarkHint struct {
	event   InboxEvent
	job     instanceBenchmarkJob
	applied bool
}

func pendingAccountBenchmarkHints(ctx context.Context, instance *sql.DB, principal string, stamp int64) ([]accountBenchmarkHint, error) {
	rows, err := instance.QueryContext(ctx, `SELECT i.source,i.event_id,i.payload_digest,i.received_at,
 j.job_id,j.task_id,j.step_id,j.operation_id,j.idempotency_key,j.dispatch_digest
FROM benchmark_instance_jobs AS j JOIN inbox_events AS i
 ON i.source=?||j.source_key AND i.payload_ref=j.job_id
WHERE j.principal_id=? AND i.kind='benchmark_account_job_hint' AND i.state='received'
 AND i.received_at>=? AND i.next_retry_at<=?
ORDER BY i.received_at,i.event_id LIMIT 50`, accountBenchmarkHintPrefix, principal, stamp-accountBenchmarkHintTTL, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hints []accountBenchmarkHint
	for rows.Next() {
		var h accountBenchmarkHint
		if err := rows.Scan(&h.event.Source, &h.event.EventID, &h.event.PayloadDigest, &h.event.ReceivedAt,
			&h.job.job, &h.job.task, &h.job.step, &h.job.operation, &h.job.idempotency, &h.job.dispatchDigest); err != nil {
			return nil, err
		}
		h.job.principal = principal
		h.job.source = strings.TrimPrefix(h.event.Source, accountBenchmarkHintPrefix)
		hints = append(hints, h)
	}
	return hints, rows.Err()
}

func (s *Store) applyAccountBenchmarkHints(ctx context.Context, stamp int64) error {
	if s.benchmarkInstance == nil {
		return nil
	}
	var principal string
	if err := s.db.QueryRowContext(ctx, "SELECT principal_id FROM account_business_owner WHERE singleton=1").Scan(&principal); err != nil {
		return err
	}
	hints, err := pendingAccountBenchmarkHints(ctx, s.benchmarkInstance, principal, stamp)
	if err != nil || len(hints) == 0 {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, err := lockBenchmarkAccount(ctx, tx)
	if err != nil {
		return err
	}
	if owner != principal {
		return ErrAuthorizationDenied
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM inbox_events WHERE kind=? AND state='applied' AND received_at<?",
		accountBenchmarkHintKind, stamp-accountBenchmarkHintTTL); err != nil {
		return err
	}
	for i := range hints {
		h := &hints[i]
		h.applied, err = wakeAccountBenchmark(ctx, tx, *h, stamp)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The account state's wake and local dedupe are committed first. A crash
	// now leaves a redeliverable instance receipt, not a lost wake.
	return acknowledgeAccountBenchmarkHints(ctx, s.benchmarkInstance, hints, stamp)
}

func wakeAccountBenchmark(ctx context.Context, tx *sql.Tx, h accountBenchmarkHint, stamp int64) (bool, error) {
	job := h.job
	var phase string
	var callUntil int64
	err := tx.QueryRowContext(ctx, `SELECT b.phase,b.call_until
FROM benchmark_cases AS b JOIN benchmark_runs AS r ON r.task_id=b.task_id
 JOIN operations AS o ON o.operation_id=b.operation_id AND o.task_id=b.task_id AND o.step_id=b.step_id
WHERE r.actor=? AND b.task_id=? AND b.step_id=? AND b.operation_id=? AND b.job_id=? AND o.idempotency_key=? AND o.params_digest=?`,
		job.principal, job.task, job.step, job.operation, job.job, job.idempotency, job.dispatchDigest).Scan(&phase, &callUntil)
	if errors.Is(err, sql.ErrNoRows) {
		// Start receipt may be committed centrally but not yet in this
		// private store. Reconciliation must settle the original key.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if phase != "following" && phase != "verified" && phase != "incomplete" || phase == "following" && callUntil > stamp {
		return false, nil
	}
	entry := h.event
	entry.Principal, entry.Kind = h.event.Source, accountBenchmarkHintKind
	entry.TaskID, entry.StepID, entry.OperationID = job.task, job.step, job.operation
	entry.PayloadRef = job.job
	rec, err := receiveInbox(tx, entry)
	if err != nil {
		return false, err
	}
	if rec.Quarantined {
		return false, nil
	}
	if rec.Duplicate {
		return rec.Event.State == "applied", nil
	}
	if phase == "following" {
		if _, err := tx.ExecContext(ctx, "UPDATE benchmark_cases SET next_at=min(next_at,?) WHERE step_id=?", stamp, job.step); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE inbox_events SET state='applied',applied_at=? WHERE source=? AND event_id=?",
		stamp, h.event.Source, h.event.EventID)
	return err == nil, err
}

func acknowledgeAccountBenchmarkHints(ctx context.Context, instance *sql.DB, hints []accountBenchmarkHint, stamp int64) error {
	tx, err := instance.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range hints {
		state, appliedAt, retryAt := "received", int64(0), stamp+accountBenchmarkRetryMS
		if h.applied {
			state, appliedAt, retryAt = "applied", stamp, 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_events SET state=?,applied_at=?,next_retry_at=?
WHERE source=? AND event_id=? AND payload_digest=? AND received_at=? AND state='received' AND kind='benchmark_account_job_hint'`,
			state, appliedAt, retryAt, h.event.Source, h.event.EventID, h.event.PayloadDigest, h.event.ReceivedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
