package work

import (
	"context"
	"database/sql"
)

func migrateAccountConnectionCalls(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS account_connection_calls (
	call_id TEXT PRIMARY KEY REFERENCES connection_calls(id) ON DELETE CASCADE,
	principal_id TEXT NOT NULL REFERENCES accounts(principal_id)
);
CREATE INDEX IF NOT EXISTS account_connection_calls_by_owner ON account_connection_calls(principal_id,call_id);`)
	return err
}

const accountConnectionCallScope = ` AND EXISTS (
	SELECT 1 FROM account_connection_calls ac JOIN accounts a ON a.principal_id=ac.principal_id
	WHERE ac.call_id=connection_calls.id AND ac.principal_id=? AND a.status='active'
)`

func (s *Store) AccountConnectionEvidence(ctx context.Context, principal, callID string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT evidence FROM connection_calls
WHERE id=? AND status IN ('completed','tool_error') AND evidence IS NOT NULL AND `+exactMCPAuthority("connection_calls")+accountConnectionCallScope, callID, principal).Scan(&content)
	return content, err
}
