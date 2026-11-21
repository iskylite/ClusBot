package work

import "context"

type ConnectionCall struct {
	ID                 string `json:"id"`
	ConnectionID       string `json:"connection_id"`
	ConnectionRevision int    `json:"connection_revision"`
	ToolName           string `json:"tool_name"`
	ToolVersion        string `json:"tool_version"`
	Status             string `json:"status"`
	CreatedAt          int64  `json:"created_at"`
}

// ConnectionCalls 返回连接最近调用记录（默认 50 条）。
func (s *Store) ConnectionCalls(ctx context.Context, connectionID string, limit int) ([]ConnectionCall, error) {
	return s.connectionCalls(ctx, connectionID, limit, "")
}

func (s *Store) AccountConnectionCalls(ctx context.Context, principal, connectionID string, limit int) ([]ConnectionCall, error) {
	if principal == "" {
		return nil, ErrAccountDenied
	}
	return s.connectionCalls(ctx, connectionID, limit, principal)
}

func (s *Store) connectionCalls(ctx context.Context, connectionID string, limit int, principal string) ([]ConnectionCall, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	query := `SELECT id, connection_id, connection_revision, tool_name, tool_version, status, created_at
FROM connection_calls WHERE connection_id=?`
	params := []any{connectionID}
	if principal != "" {
		query += accountConnectionCallScope
		params = append(params, principal)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(params, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ConnectionCall{}
	for rows.Next() {
		var c ConnectionCall
		if err := rows.Scan(&c.ID, &c.ConnectionID, &c.ConnectionRevision, &c.ToolName, &c.ToolVersion, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}
