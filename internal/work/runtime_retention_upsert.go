package work

import "context"

// UpsertRetentionPolicy 新增或更新保留策略。
func (s *Store) UpsertRetentionPolicy(ctx context.Context, p RetentionPolicy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertRetentionPolicy(tx, p, now()); err != nil {
		return err
	}
	return tx.Commit()
}
