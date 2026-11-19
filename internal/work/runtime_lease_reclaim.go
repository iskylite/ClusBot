package work

import "context"

// ReclaimExpiredLeases 回收过期租约；已派发操作进入待核对，不自动重发。
func (s *Store) ReclaimExpiredLeases(ctx context.Context, limit int) (ReclaimResult, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReclaimResult{}, err
	}
	defer tx.Rollback()
	result, err := reclaimExpiredLeases(tx, now(), limit)
	if err != nil {
		return ReclaimResult{}, err
	}
	return result, tx.Commit()
}

// The production embedded reader may lose its local completion transaction
// without the app stopping. Recover only its known, read-only work; do not
// generalize an expired lease into proof an external action never ran.
const embeddedReadReclaimBatch = 16

func (s *Store) reclaimExpiredEmbeddedReads(ctx context.Context, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT l.lease_id,l.unit_id,l.task_id,l.lease_generation
FROM work_leases l JOIN work_units u ON u.unit_id=l.unit_id AND u.task_id=l.task_id AND u.lease_generation=l.lease_generation
JOIN worker_instances w ON w.worker_id=l.worker_id AND w.role='embedded' AND w.registered_by='host' AND w.instance_version='embedded-host'
JOIN steps step ON step.step_id=u.step_id AND step.task_id=u.task_id
WHERE l.state='active' AND l.lease_until<=? AND u.kind='deterministic' AND u.state IN ('claimed','running')
AND step.definition_key=?
AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.task_id=u.task_id)
AND NOT EXISTS(SELECT 1 FROM result_receipts r WHERE r.unit_id=u.unit_id AND r.lease_generation=l.lease_generation)
ORDER BY l.lease_until,l.lease_id LIMIT ?`, stamp, assetAcceptanceHandlerKey, embeddedReadReclaimBatch)
	if err != nil {
		return err
	}
	type expired struct {
		lease, unit, task string
		generation        int
	}
	var pending []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.lease, &item.unit, &item.task, &item.generation); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range pending {
		res, err := tx.ExecContext(ctx, `UPDATE work_leases SET state='reclaimed',reclaimed_at=?,reclaim_reason='本机只读查询租约过期'
WHERE lease_id=? AND unit_id=? AND lease_generation=? AND state='active' AND lease_until<=?`, stamp, item.lease, item.unit, item.generation, stamp)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			continue
		}
		unit, err := tx.ExecContext(ctx, "UPDATE work_units SET state='ready',updated_at=? WHERE unit_id=? AND lease_generation=? AND state IN ('claimed','running')", stamp, item.unit, item.generation)
		if err != nil {
			return err
		}
		if changed, err := unit.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return ErrLeaseConflict // rollback the lease change and its audit
		}
		if err := recordAudit(tx, AuditEvent{TaskID: item.task, UnitID: item.unit, Event: "embedded_read_lease_reclaimed", Actor: "system",
			Summary: "只读资产查询租约失效，可重新领取；旧代际的回报无效", OccurredAt: stamp}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
