package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const modelRunEligible = ` FROM runs WHERE status='queued'
AND NOT EXISTS(SELECT 1 FROM workflow_blocked_tasks w WHERE w.task_id=runs.task_id)
AND NOT EXISTS(SELECT 1 FROM run_queue_order q WHERE q.after_run_id=runs.id)
AND ((? AND coalesce(runs.task_id,'')='') OR (? AND coalesce(runs.task_id,'')<>'')
OR EXISTS(SELECT 1 FROM isolated_reply_pins p WHERE p.run_id=runs.id))`
const modelRunPriority = `CASE (SELECT priority FROM tasks WHERE id=runs.task_id) WHEN 'high' THEN 0 WHEN 'low' THEN 2 ELSE 1 END`

var modelRunPrioritySlots = [...]int{0, 0, 0, 0, 1, 1, 2}

func migrateModelRunPriorityCursor(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS model_run_priority_cursor (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 next_slot INTEGER NOT NULL CHECK(next_slot BETWEEN 0 AND 6)
);
INSERT INTO model_run_priority_cursor(singleton,next_slot) VALUES(1,0) ON CONFLICT DO NOTHING;`)
	return err
}

// selectModelRun keeps the old strict priority+FIFO policy when disabled. The
// optional policy allocates priority-class turns only among already eligible
// work. Empty classes cost neither a claim nor a worker slot. The write to the
// cursor rolls back if the caller cannot commit the actual claim.
func selectModelRun(ctx context.Context, tx *sql.Tx, fair, replyReady, workerReady bool) (Run, error) {
	if !fair {
		return scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+modelRunEligible+" ORDER BY "+modelRunPriority+",rowid LIMIT 1", replyReady, workerReady))
	}
	var next int
	if err := tx.QueryRowContext(ctx, "SELECT next_slot FROM model_run_priority_cursor WHERE singleton=1").Scan(&next); err != nil {
		return Run{}, err
	}
	if next < 0 || next >= len(modelRunPrioritySlots) {
		return Run{}, fmt.Errorf("模型队列优先级游标无效：%d", next)
	}
	var tried [3]bool
	for offset := range len(modelRunPrioritySlots) {
		slot := (next + offset) % len(modelRunPrioritySlots)
		priority := modelRunPrioritySlots[slot]
		if tried[priority] {
			continue
		}
		tried[priority] = true
		run, err := scanRun(tx.QueryRowContext(ctx,
			"SELECT "+runColumns+modelRunEligible+" AND "+modelRunPriority+"=? ORDER BY rowid LIMIT 1", replyReady, workerReady, priority))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Run{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE model_run_priority_cursor SET next_slot=? WHERE singleton=1", (slot+1)%len(modelRunPrioritySlots)); err != nil {
			return Run{}, err
		}
		return run, nil
	}
	return Run{}, sql.ErrNoRows
}
