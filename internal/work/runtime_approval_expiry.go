package work

import (
	"context"
	"database/sql"
)

const approvalExpiryBatch = 50

// expirePendingApprovals advances durable approval state only. The WHERE
// predicate and feed trigger are applied in the same SQLite write; an already
// committed human decision wins and records are never attributed to a user.
func (s *Store) expirePendingApprovals(ctx context.Context, stamp int64, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrState
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals
SET decision='expired',decided_at=expires_at,decided_by=''
WHERE decision='pending' AND expires_at>0 AND expires_at<=?
AND rowid IN (SELECT rowid FROM approvals
 WHERE decision='pending' AND expires_at>0 AND expires_at<=?
 ORDER BY expires_at,rowid LIMIT ?)`, stamp, stamp, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func expireOneApproval(tx *sql.Tx, id string, stamp int64) error {
	_, err := tx.Exec(`UPDATE approvals SET decision='expired',decided_at=expires_at,decided_by=''
WHERE approval_id=? AND decision='pending' AND expires_at>0 AND expires_at<=?`, id, stamp)
	return err
}
