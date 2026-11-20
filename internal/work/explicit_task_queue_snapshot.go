package work

import (
	"context"
	"database/sql"
)

// ExplicitTaskQueueSnapshot contains only the given principal's explicit-new-
// task submissions. Its counts are not instance-wide occupancy.
type ExplicitTaskQueueSnapshot struct {
	PendingReceipts    int    `json:"pending_receipts"`
	LinkedQueued       int    `json:"linked_queued"`
	LinkedRunning      int    `json:"linked_running"`
	OldestReceiptAgeMS *int64 `json:"oldest_receipt_age_ms"`
	OldestQueuedAgeMS  *int64 `json:"oldest_queued_age_ms"`
}

func (s *Store) ReadExplicitTaskQueue(ctx context.Context, principal string) (ExplicitTaskQueueSnapshot, error) {
	var out ExplicitTaskQueueSnapshot
	if principal == "" {
		return out, ErrAuthorizationDenied
	}
	db := s.db
	if s.readPool != nil {
		db = s.readPool.db
	}
	var oldestReceipt, oldestQueued sql.NullInt64
	// One SELECT gives both stages the same SQLite read snapshot. As in the
	// admission gate, each linked Run counts once and no linked receipt is
	// also counted in the pending stage. Ownership is required at either stage.
	err := db.QueryRowContext(ctx, `
WITH pending AS (
  SELECT count(*) AS n,min(received_at) AS oldest
  FROM inbox_events
  WHERE source='chat' AND explicit_task_admission=1 AND principal=?
    AND run_id IS NULL AND state IN ('received','applying')
), active AS (
  SELECT count(*) FILTER (WHERE r.status='queued') AS queued,
    count(*) FILTER (WHERE r.status='running') AS running,
    min(r.created_at) FILTER (WHERE r.status='queued') AS oldest
  FROM runs r WHERE r.status IN ('queued','running') AND EXISTS (
    SELECT 1 FROM inbox_events i
    WHERE i.source='chat' AND i.explicit_task_admission=1 AND i.principal=?
      AND i.run_id IS NOT NULL AND i.run_id=r.id
  )
)
SELECT pending.n,active.queued,active.running,pending.oldest,active.oldest
FROM pending CROSS JOIN active`, principal, principal).
		Scan(&out.PendingReceipts, &out.LinkedQueued, &out.LinkedRunning, &oldestReceipt, &oldestQueued)
	if err != nil {
		return ExplicitTaskQueueSnapshot{}, err
	}
	stamp := now()
	age := func(when sql.NullInt64) *int64 {
		if !when.Valid {
			return nil
		}
		ms := max(0, stamp-max(0, when.Int64))
		return &ms
	}
	out.OldestReceiptAgeMS = age(oldestReceipt)
	out.OldestQueuedAgeMS = age(oldestQueued)
	return out, nil
}
