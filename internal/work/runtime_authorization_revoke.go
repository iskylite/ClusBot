package work

import "context"

// RevokeAuthorizationByID 撤销一条授权（同事务写审计与代际）。
func (s *Store) RevokeAuthorizationByID(ctx context.Context, authID, revokedBy string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := revokeAuthorization(tx, authID, revokedBy, now()); err != nil {
		return err
	}
	return tx.Commit()
}
