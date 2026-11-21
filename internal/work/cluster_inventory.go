package work

import (
	"context"
	"strings"
	"time"
)

// ClusterInventoryNode is the current state of one authorized cluster node.
type ClusterInventoryNode struct {
	ObjectRef string `json:"object_ref"`
	Identity  string `json:"identity"`
	State     string `json:"state"`
	UpdatedAt int64  `json:"updated_at"`
}

type ClusterInventoryPage struct {
	Items      []ClusterInventoryNode `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// ClusterInventory uses the same effective object-level read grants and node
// kinds as ClusterTrendStats. Keyset pagination bounds the returned inventory.
func (s *Store) ClusterInventory(ctx context.Context, principal, after string, limit int, at time.Time) (ClusterInventoryPage, error) {
	out := ClusterInventoryPage{Items: []ClusterInventoryNode{}}
	if strings.TrimSpace(principal) == "" {
		return out, ErrAuthorizationDenied
	}
	if limit < 1 || limit > 100 {
		limit = 24
	}
	stamp := at.UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT a.object_ref,a.identity,a.state,a.updated_at
		FROM ops_assets a WHERE a.kind IN ('node','计算节点','GPU 节点') AND a.object_ref>?
		AND EXISTS (SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='read'
		AND g.object_ref=a.object_ref AND g.revoked_at=0 AND g.valid_from<=?
		AND (g.expires_at=0 OR g.expires_at>?))
		ORDER BY a.object_ref LIMIT ?`, after, principal, stamp, stamp, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ClusterInventoryNode
		if err := rows.Scan(&item.ObjectRef, &item.Identity, &item.State, &item.UpdatedAt); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			out.NextCursor = out.Items[len(out.Items)-1].ObjectRef
			break
		}
		out.Items = append(out.Items, item)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}
