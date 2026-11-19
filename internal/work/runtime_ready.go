package work

import "database/sql"

const runtimeReadyIndex = `CREATE INDEX IF NOT EXISTS work_units_by_step ON work_units(step_id)`

// These predicates use the authoritative task and step rows in the same
// transaction as selection. The task_revisions table contains immutable
// contract snapshots; tasks.revision also changes on ordinary status writes.
const runnableTask = `
t.status='active'
AND NOT EXISTS (
  SELECT 1 FROM task_states ts WHERE ts.task_id=t.id
  AND ts.state NOT IN ('pending','running')
)
`

const runnableStep = `
s.state IN ('ready','blocked')
AND s.contract_revision=(SELECT max(r.revision) FROM task_revisions r WHERE r.task_id=s.task_id)
AND NOT EXISTS (
  SELECT 1 FROM waits wt WHERE wt.task_id=s.task_id AND wt.state='open'
  AND (wt.step_id IS NULL OR wt.step_id=s.step_id)
)
AND NOT EXISTS (
  SELECT 1 FROM step_dependencies d
  LEFT JOIN steps upstream ON upstream.step_id=d.depends_on_step_id
  WHERE d.step_id=s.step_id AND (
    upstream.step_id IS NULL OR upstream.task_id<>s.task_id
    OR upstream.contract_revision<>s.contract_revision
    OR (NOT (upstream.optional=1 AND upstream.state='cancelled') AND (
      upstream.state<>'completed'
      OR (d.evidence_revision<>0 AND upstream.evidence_revision<>d.evidence_revision)
      OR (s.evidence_revision<>0 AND upstream.evidence_revision<>s.evidence_revision)
    ))
  )
)
`

// selectReadyUnits is shared by batch selection and the atomic claim path.
// Ineligible rows are filtered before LIMIT; they cannot starve other tasks.
func selectReadyUnits(tx *sql.Tx, kind WorkUnitKind, scope string, stamp int64, limit int) ([]WorkUnit, error) {
	rows, err := tx.Query(`
SELECT w.unit_id,w.task_id,w.step_id,w.kind,w.lease_generation,w.ready_at,w.state,w.input_ref,w.created_at,w.updated_at
FROM work_units w JOIN tasks t ON t.id=w.task_id
LEFT JOIN steps s ON s.step_id=w.step_id AND s.task_id=w.task_id
WHERE w.kind=? AND w.state='ready' AND w.ready_at<=?
AND (?='' OR t.scope_label=?)
AND `+runnableTask+`
AND (
  (w.step_id IS NULL AND NOT EXISTS (
    SELECT 1 FROM waits wt WHERE wt.task_id=w.task_id AND wt.state='open' AND wt.step_id IS NULL
  )) OR (s.step_id IS NOT NULL AND `+runnableStep+`)
)
AND (w.kind<>'action' OR (
  NOT EXISTS (SELECT 1 FROM task_states ts WHERE ts.task_id=t.id AND ts.reconcile_required=1)
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.task_id=t.id AND o.state='reconcile')
))
ORDER BY w.ready_at,w.unit_id LIMIT ?`, string(kind), stamp, scope, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var units []WorkUnit
	for rows.Next() {
		var u WorkUnit
		var stepID sql.NullString
		if err := rows.Scan(&u.UnitID, &u.TaskID, &stepID, &u.Kind, &u.LeaseGeneration,
			&u.ReadyAt, &u.State, &u.InputRef, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.StepID = stepID.String
		units = append(units, u)
	}
	return units, rows.Err()
}
