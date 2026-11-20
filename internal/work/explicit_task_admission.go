package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const MaxExplicitTaskAdmissionLimit = 10000

var ErrExplicitTaskQueueFull = errors.New("显式新任务队列已满，请稍后用原请求重试")

func migrateExplicitTaskAdmissionLane(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "inbox_events", "explicit_task_admission",
		`ALTER TABLE inbox_events ADD COLUMN explicit_task_admission INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := tx.Exec(`
CREATE INDEX IF NOT EXISTS inbox_explicit_task_waiting ON inbox_events(event_id)
 WHERE source='chat' AND explicit_task_admission=1 AND run_id IS NULL AND state IN ('received','applying');
CREATE INDEX IF NOT EXISTS inbox_explicit_task_run ON inbox_events(run_id)
 WHERE source='chat' AND explicit_task_admission=1 AND run_id IS NOT NULL;
`)
	return err
}

// AdmitExplicitTask is the explicit new task entry point. It uses the same
// receipt/fingerprint as AdmitChat, but tags first admissions so the optional
// capacity check reflects this lane only. Older receipts whose entry point was
// never recorded stay untagged.
func (s *Store) AdmitExplicitTask(ctx context.Context, requestKey, principal, text, scope string, delivery Delivery, materialIDs []string, modelAvailable bool, limit int) (AdmissionResult, error) {
	if limit < 0 || limit > MaxExplicitTaskAdmissionLimit {
		return AdmissionResult{}, fmt.Errorf("显式新任务受理上限必须在 0 到 %d 之间", MaxExplicitTaskAdmissionLimit)
	}
	return s.admitChat(ctx, requestKey, principal, "", text, scope, true, delivery, materialIDs, modelAvailable, true, limit)
}

func checkExplicitTaskCapacity(tx *sql.Tx, limit int) error {
	var waiting, active int
	if err := tx.QueryRow(`SELECT count(*) FROM (
 SELECT 1 FROM inbox_events WHERE source='chat' AND explicit_task_admission=1
 AND run_id IS NULL AND state IN ('received','applying') LIMIT ?)`, limit).Scan(&waiting); err != nil {
		return err
	}
	if waiting >= limit {
		return ErrExplicitTaskQueueFull
	}
	// An accepted receipt becomes linked to its Run in the same transaction as
	// the Run is created. These two counts are disjoint so applying a receipt
	// neither creates a free slot nor double-charges the same submission.
	if err := tx.QueryRow(`SELECT count(*) FROM (
 SELECT 1 FROM runs r WHERE r.status IN ('queued','running') AND EXISTS (
  SELECT 1 FROM inbox_events i WHERE i.source='chat' AND i.explicit_task_admission=1
  AND i.run_id IS NOT NULL AND i.run_id=r.id) LIMIT ?)`, limit-waiting).Scan(&active); err != nil {
		return err
	}
	if active >= limit-waiting {
		return ErrExplicitTaskQueueFull
	}
	return nil
}
