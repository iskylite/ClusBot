package work

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// ClusterInventoryNode is the current lifecycle state of one authorized node.
type ClusterInventoryNode struct {
	ObjectRef string `json:"object_ref"`
	Identity  string `json:"identity"`
	State     string `json:"state"`
	UpdatedAt int64  `json:"updated_at"`
	Health    string `json:"health"`
	HealthAt  int64  `json:"health_at"`
	PodRef    string `json:"pod_ref,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
}

// ClusterHealthCounts uses the latest recorded health, separately from the
// asset lifecycle state. An absent health record is unknown, not healthy.
type ClusterHealthCounts struct {
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
	Unknown   int `json:"unknown"`
}

type ClusterInventoryPod struct {
	ObjectRef  string              `json:"object_ref"`
	Identity   string              `json:"identity"`
	TotalNodes int                 `json:"total_nodes"`
	Health     ClusterHealthCounts `json:"health"`
	Status     string              `json:"status"`
}

type ClusterInventoryException struct {
	ObjectRef string `json:"object_ref"`
	Identity  string `json:"identity"`
	State     string `json:"state"`
	Health    string `json:"health"`
	HealthAt  int64  `json:"health_at"`
	PodRef    string `json:"pod_ref,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
}

type ClusterInventorySummary struct {
	TotalNodes           int                         `json:"total_nodes"`
	Health               ClusterHealthCounts         `json:"health"`
	HealthPercent        *float64                    `json:"health_percent"`
	TopologyUnknownNodes int                         `json:"topology_unknown_nodes"`
	Pods                 []ClusterInventoryPod       `json:"pods"`
	ExceptionTotal       int                         `json:"exception_total"`
	Exceptions           []ClusterInventoryException `json:"exceptions"`
	ComputedAt           int64                       `json:"computed_at"`
}

type ClusterInventoryPage struct {
	Items      []ClusterInventoryNode  `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Summary    ClusterInventorySummary `json:"summary"`
}

type inventorySummaryNode struct {
	ClusterInventoryNode
	parents map[string]inventoryParent
}

type inventoryParent struct {
	identity string
	readable bool
}

func addClusterHealth(counts *ClusterHealthCounts, health string) {
	switch health {
	case "healthy":
		counts.Healthy++
	case "degraded":
		counts.Degraded++
	case "unhealthy":
		counts.Unhealthy++
	default:
		counts.Unknown++
	}
}

func clusterHealthStatus(counts ClusterHealthCounts) string {
	switch {
	case counts.Unhealthy > 0:
		return "unhealthy"
	case counts.Degraded > 0:
		return "degraded"
	case counts.Unknown > 0:
		return "unknown"
	default:
		return "healthy"
	}
}

// ClusterInventory returns a keyset page plus a summary over all currently
// readable nodes, in one read transaction. POD membership is only an explicit
// effective physical relationship from an asset whose registered kind is
// exactly "pod". Both endpoints need read grants; missing, ambiguous or
// inaccessible topology is counted as unknown without revealing its identity.
// Health is the latest recorded transition value, not the lifecycle state or a
// live probe. The time of the record is retained on exception entries.
func (s *Store) ClusterInventory(ctx context.Context, principal, after string, limit int, at time.Time) (ClusterInventoryPage, error) {
	return s.ClusterInventoryForPod(ctx, principal, "", after, limit, at)
}

// ClusterInventoryForPod optionally restricts the detail page to one readable
// POD. The summary remains global and a missing or inaccessible POD is not
// distinguishable from one without visible members.
func (s *Store) ClusterInventoryForPod(ctx context.Context, principal, podRef, after string, limit int, at time.Time) (ClusterInventoryPage, error) {
	out := ClusterInventoryPage{Items: []ClusterInventoryNode{}, Summary: ClusterInventorySummary{
		Pods: []ClusterInventoryPod{}, Exceptions: []ClusterInventoryException{}, ComputedAt: at.UnixMilli(),
	}}
	if strings.TrimSpace(principal) == "" {
		return out, ErrAuthorizationDenied
	}
	if limit < 1 || limit > 100 {
		limit = 24
	}
	stamp := at.UnixMilli()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	const grant = `EXISTS (SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='read'
		AND g.object_ref=a.object_ref AND g.revoked_at=0 AND g.valid_from<=?
		AND (g.expires_at=0 OR g.expires_at>?))`
	rows, err := tx.QueryContext(ctx, `SELECT a.object_ref,a.identity,a.state,a.updated_at,
		COALESCE((SELECT t.health FROM asset_transitions t WHERE t.object_ref=a.object_ref AND t.occurred_at<=?
			ORDER BY t.occurred_at DESC,t.created_at DESC,t.transition_id DESC LIMIT 1),'unknown'),
		COALESCE((SELECT t.occurred_at FROM asset_transitions t WHERE t.object_ref=a.object_ref AND t.occurred_at<=?
			ORDER BY t.occurred_at DESC,t.created_at DESC,t.transition_id DESC LIMIT 1),0)
		FROM ops_assets a WHERE a.kind IN ('node','计算节点','GPU 节点') AND `+grant+` ORDER BY a.object_ref`,
		stamp, stamp, principal, stamp, stamp)
	if err != nil {
		return out, err
	}
	nodes := make(map[string]*inventorySummaryNode)
	for rows.Next() {
		node := &inventorySummaryNode{parents: make(map[string]inventoryParent)}
		if err := rows.Scan(&node.ObjectRef, &node.Identity, &node.State, &node.UpdatedAt, &node.Health, &node.HealthAt); err != nil {
			rows.Close()
			return out, err
		}
		nodes[node.ObjectRef] = node
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	// Select only readable child nodes. Parent authorization is checked before
	// returning a POD identity, and inaccessible parents still make membership
	// unknown (including when another readable parent conflicts).
	rows, err = tx.QueryContext(ctx, `SELECT r.target_ref,p.object_ref,p.identity,
		EXISTS (SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='read'
			AND g.object_ref=p.object_ref AND g.revoked_at=0 AND g.valid_from<=?
			AND (g.expires_at=0 OR g.expires_at>?))
		FROM ops_assets a JOIN relations r ON r.target_ref=a.object_ref
		JOIN ops_assets p ON p.object_ref=r.source_ref AND p.kind='pod'
		WHERE a.kind IN ('node','计算节点','GPU 节点') AND `+grant+`
		AND r.relation_kind IN ('physical_parent','physical_component')
		AND r.valid_from<=? AND (r.valid_to=0 OR r.valid_to>?)`,
		principal, stamp, stamp, principal, stamp, stamp, stamp, stamp)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var nodeRef, podRef, identity string
		var readable bool
		if err := rows.Scan(&nodeRef, &podRef, &identity, &readable); err != nil {
			rows.Close()
			return out, err
		}
		if node := nodes[nodeRef]; node != nil {
			node.parents[podRef] = inventoryParent{identity: identity, readable: readable}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	pods := make(map[string]*ClusterInventoryPod)
	out.Summary.TotalNodes = len(nodes)
	for _, node := range nodes {
		addClusterHealth(&out.Summary.Health, node.Health)
		var parentRef, podName string
		if len(node.parents) == 1 {
			for ref, parent := range node.parents {
				if parent.readable {
					parentRef, podName = ref, parent.identity
				}
			}
		}
		node.PodRef, node.PodName = parentRef, podName
		if parentRef == "" {
			out.Summary.TopologyUnknownNodes++
		} else {
			pod := pods[parentRef]
			if pod == nil {
				pod = &ClusterInventoryPod{ObjectRef: parentRef, Identity: podName}
				pods[parentRef] = pod
			}
			pod.TotalNodes++
			addClusterHealth(&pod.Health, node.Health)
		}
		if node.Health != "healthy" {
			out.Summary.Exceptions = append(out.Summary.Exceptions, ClusterInventoryException{
				ObjectRef: node.ObjectRef, Identity: node.Identity, State: node.State,
				Health: node.Health, HealthAt: node.HealthAt, PodRef: parentRef, PodName: podName,
			})
		}
	}
	if podRef != "" && pods[podRef] == nil {
		return out, sql.ErrNoRows
	}
	refs := make([]string, 0, len(nodes))
	for ref := range nodes {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		node := nodes[ref]
		if ref <= after || (podRef != "" && node.PodRef != podRef) {
			continue
		}
		if len(out.Items) < limit {
			out.Items = append(out.Items, node.ClusterInventoryNode)
		} else {
			out.NextCursor = out.Items[len(out.Items)-1].ObjectRef
			break
		}
	}
	if len(nodes) > 0 && out.Summary.Health.Unknown == 0 {
		percent := float64(out.Summary.Health.Healthy) * 100 / float64(len(nodes))
		out.Summary.HealthPercent = &percent
	}
	for _, pod := range pods {
		pod.Status = clusterHealthStatus(pod.Health)
		out.Summary.Pods = append(out.Summary.Pods, *pod)
	}
	sort.Slice(out.Summary.Pods, func(i, j int) bool { return out.Summary.Pods[i].ObjectRef < out.Summary.Pods[j].ObjectRef })
	priority := map[string]int{"unhealthy": 0, "degraded": 1, "unknown": 2}
	sort.Slice(out.Summary.Exceptions, func(i, j int) bool {
		a, b := out.Summary.Exceptions[i], out.Summary.Exceptions[j]
		if priority[a.Health] != priority[b.Health] {
			return priority[a.Health] < priority[b.Health]
		}
		return a.ObjectRef < b.ObjectRef
	})
	out.Summary.ExceptionTotal = len(out.Summary.Exceptions)
	if len(out.Summary.Exceptions) > 20 {
		out.Summary.Exceptions = out.Summary.Exceptions[:20]
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}
