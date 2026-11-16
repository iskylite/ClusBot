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
