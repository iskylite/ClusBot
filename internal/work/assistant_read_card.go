package work

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type assistantReadReceiptPolicy struct {
	source string
	kind   string
	limit  int
}

type AssistantReadCardAction struct {
	Actor, Conversation, Run, Target, Request string
	Revision                                  int64
}

func (a AssistantReadCardAction) key() string {
	return digest(toJSON([]string{a.Actor, a.Conversation, a.Request}))
}

func (a AssistantReadCardAction) fingerprint() string {
	return digest(toJSON([]any{a.Run, a.Target, a.Revision}))
}

// Only receipts are shared between the two read cards. Their data scope,
// target validation and response content stay in their individual transactions.
func beginAssistantReadReceipt(ctx context.Context, tx *sql.Tx, policy assistantReadReceiptPolicy, a AssistantReadCardAction, restored bool) (bool, error) {
	var fingerprint, state string
	err := tx.QueryRowContext(ctx, `SELECT payload_digest,state FROM inbox_events WHERE source=? AND event_id=? AND principal=?`,
		policy.source, a.key(), a.Actor).Scan(&fingerprint, &state)
	if err == nil {
		if fingerprint != a.fingerprint() {
			return false, ErrConflict
		}
		switch state {
		case "applied":
			if !restored {
				return false, ErrState
			}
			return true, nil
		case "failed":
			return false, ErrA2UIActionFailed
		default:
			return false, ErrA2UIActionPending
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var attempts, pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(summary=? AND state='applying' AND received_at>?),0)
FROM inbox_events WHERE source=? AND principal=? AND run_id=?`, a.Target, now()-int64(30*time.Second/time.Millisecond), policy.source, a.Actor, a.Run).Scan(&attempts, &pending); err != nil {
		return false, err
	}
	if pending != 0 && !restored {
		return false, ErrA2UIActionPending
	}
	if attempts >= policy.limit {
		return false, ErrA2UIActionFailed
	}
	stamp := now()
	state, invocationCount, appliedAt := "applying", 1, int64(0)
	if restored {
		// Bind even a fresh browser key that is returning a cached receipt;
		// it must not later be repurposed to select a different task.
		state, invocationCount, appliedAt = "applied", 0, stamp
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_events(source,event_id,source_seq,principal,kind,summary,source_version,payload_digest,payload_ref,run_id,received_at,state,attempts,applied_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, policy.source, a.key(), a.Revision, a.Actor, policy.kind, a.Target, a.Conversation,
		a.fingerprint(), a.Run, a.Run, stamp, state, invocationCount, appliedAt); err != nil {
		return false, err
	}
	return restored, nil
}

func (s *Store) failAssistantReadReceipt(ctx context.Context, policy assistantReadReceiptPolicy, a AssistantReadCardAction) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_events SET state='failed',failure='action_not_completed'
WHERE source=? AND event_id=? AND principal=? AND payload_digest=? AND state='applying'`, policy.source, a.key(), a.Actor, a.fingerprint())
	return err
}

func finishAssistantReadReceipt(ctx context.Context, tx *sql.Tx, policy assistantReadReceiptPolicy, a AssistantReadCardAction) error {
	change, err := tx.ExecContext(ctx, `UPDATE inbox_events SET state='applied',applied_at=? WHERE source=? AND event_id=? AND principal=? AND payload_digest=? AND state='applying'`,
		now(), policy.source, a.key(), a.Actor, a.fingerprint())
	if err != nil {
		return err
	}
	if n, err := change.RowsAffected(); err != nil || n != 1 {
		return ErrConflict
	}
	return nil
}
