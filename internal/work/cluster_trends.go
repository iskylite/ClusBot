package work

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// ClusterTrendBucket is one UTC bucket. The asset counts are sampled at its
// end; fault counts are new fault segments whose start lies in the bucket.
type ClusterTrendBucket struct {
	StartAt          int64          `json:"start_at"`
	EndAt            int64          `json:"end_at"`
	TotalNodes       int            `json:"total_nodes"`
	InServiceNodes   int            `json:"in_service_nodes"`
	UnknownNodes     int            `json:"unknown_nodes"`
	InServicePercent *float64       `json:"in_service_percent"`
	FaultCounts      map[string]int `json:"fault_counts"`
}

type ClusterTrends struct {
	Range              string               `json:"range"`
	Timezone           string               `json:"timezone"`
	BucketUnit         string               `json:"bucket_unit"`
	ComputedAt         int64                `json:"computed_at"`
	NodeCountBasis     string               `json:"node_count_basis"`
	StateHistoryBasis  string               `json:"state_history_basis"`
	FaultCountBasis    string               `json:"fault_count_basis"`
	FaultCategoryBasis string               `json:"fault_category_basis"`
	FaultCoverage      string               `json:"fault_coverage"`
	FaultSourceStatus  string               `json:"fault_source_status"`
	FaultCategories    []string             `json:"fault_categories"`
	Buckets            []ClusterTrendBucket `json:"buckets"`
}

type clusterStateEvent struct {
	at    int64
	state string
}

type clusterNode struct {
	createdAt, updatedAt int64
	state                string
	events               []clusterStateEvent
}

// ClusterTrendStats reads only nodes with an effective object-level read grant
// for principal. Current authorization is used for the entire historical view;
// a past grant does not make data visible after revocation. Kinds are the node
// kinds used by the inventory and the domain demo, rather than every component.
//
// History combines initial/upsert state, explicit transitions and loan events.
// Some older paths did not record transitions. A mismatch with the current row
// therefore produces an unknown interval rather than an invented percentage.
// Fault segments lack separate object/category fields. Attribution prefers
// complete, consistent references to observations; otherwise only a simple
// unambiguous source:object:observation-kind key is used. These categories are
// observation types, not inferred fault causes.
func (s *Store) ClusterTrendStats(ctx context.Context, principal, window string, at time.Time) (ClusterTrends, error) {
	var out ClusterTrends
	if strings.TrimSpace(principal) == "" {
		return out, ErrAuthorizationDenied
	}
	at = at.UTC()
	stamp := at.UnixMilli()
	var count int
	var width time.Duration
	switch window {
	case "24h":
		count, width = 24, time.Hour
		out.BucketUnit = "hour"
	case "7d":
		count, width = 7, 24*time.Hour
		out.BucketUnit = "day"
	case "30d":
		count, width = 30, 24*time.Hour
		out.BucketUnit = "day"
	default:
		return out, errors.New("无效的趋势时间范围")
	}
	// Fixed UTC buckets, including the current (possibly partial) bucket.
	lastStart := at.Truncate(width)
	firstStart := lastStart.Add(-time.Duration(count-1) * width).UnixMilli()
	end := stamp + 1 // half-open interval including events at computed_at
	out.Range, out.Timezone, out.ComputedAt = window, "UTC", stamp
	out.NodeCountBasis = "authorized_registered_nodes_including_lent_and_retired"
	out.StateHistoryBasis = "recorded_events_may_be_incomplete"
	out.FaultCountBasis = "segment_start"
	out.FaultCategoryBasis = "observation_kind"
	out.FaultCoverage = "consistent_observation_refs_or_simple_keys"
	out.FaultSourceStatus = "not_connected"
	out.FaultCategories = []string{"nhc", "oob_alert", "network", "service", "other"}
	out.Buckets = make([]ClusterTrendBucket, count)
	for i := range out.Buckets {
		start := firstStart + int64(i)*width.Milliseconds()
		bucketEnd := start + width.Milliseconds()
		if bucketEnd > end {
			bucketEnd = end
		}
		counts := make(map[string]int, len(out.FaultCategories))
		for _, category := range out.FaultCategories {
			counts[category] = 0
		}
		out.Buckets[i] = ClusterTrendBucket{StartAt: start, EndAt: bucketEnd, FaultCounts: counts}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// An EXISTS semijoin avoids multiplying a node with overlapping grants.
	const authorizedNodes = `a.kind IN ('node','计算节点','GPU 节点') AND EXISTS (
		SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='read'
		AND g.object_ref=a.object_ref AND g.revoked_at=0 AND g.valid_from<=?
		AND (g.expires_at=0 OR g.expires_at>?))`
	nodes := map[string]*clusterNode{}
	rows, err := tx.QueryContext(ctx, `SELECT a.object_ref,a.created_at,a.updated_at,a.state
		FROM ops_assets a WHERE `+authorizedNodes, principal, stamp, stamp)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var ref string
		n := &clusterNode{}
		if err := rows.Scan(&ref, &n.createdAt, &n.updatedAt, &n.state); err != nil {
			rows.Close()
			return out, err
		}
		nodes[ref] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	// The object/time indexes let SQLite pick the latest baseline per node,
	// then read only changes inside the requested interval. The loan table is
	// also a source because its state changes bypass asset_transitions.
	sources := []struct {
		table, timeColumn, state string
	}{
		{"asset_state_history", "occurred_at", "state"},
		{"asset_transitions", "occurred_at", "to_state"},
		{"loans", "lent_at", "'借出'"},
		{"loans", "returned_at", "'待复核'"},
	}
	for _, source := range sources {
		// All identifiers above are fixed constants; only values are parameters.
		// CROSS JOIN fixes the join order: starting with the authorized nodes
		// uses the object/time indexes instead of rescanning every old event.
		baseline := `SELECT a.object_ref,h.` + source.timeColumn + `,` + stateExpression(source.state) + `
			FROM ops_assets a CROSS JOIN ` + source.table + ` h WHERE h.object_ref=a.object_ref AND h.` + source.timeColumn + `=(SELECT MAX(x.` + source.timeColumn + `) FROM ` + source.table + ` x
			WHERE x.object_ref=a.object_ref AND x.` + source.timeColumn + `>0 AND x.` + source.timeColumn + `<?
			)
			AND ` + authorizedNodes
		if err := readClusterEvents(ctx, tx, baseline, nodes, firstStart, principal, stamp, stamp); err != nil {
			return out, err
		}
		changes := `SELECT a.object_ref,h.` + source.timeColumn + `,` + stateExpression(source.state) + `
			FROM ops_assets a CROSS JOIN ` + source.table + ` h
			WHERE ` + authorizedNodes + ` AND h.object_ref=a.object_ref AND h.` + source.timeColumn + `>=? AND h.` + source.timeColumn + `<? AND h.` + source.timeColumn + `>0`
		if err := readClusterEvents(ctx, tx, changes, nodes, principal, stamp, stamp, firstStart, end); err != nil {
			return out, err
		}
	}

	for _, node := range nodes {
		normalizeClusterEvents(node, stamp)
		state, index := "", 0
		for i := range out.Buckets {
			b := &out.Buckets[i]
			point := b.EndAt - 1
			if node.createdAt > point {
				continue
			}
			for index < len(node.events) && node.events[index].at <= point {
				state = node.events[index].state
				index++
			}
			b.TotalNodes++
			switch state {
			case "":
				b.UnknownNodes++
			case AssetStateInService:
				b.InServiceNodes++
			}
		}
	}
	for i := range out.Buckets {
		b := &out.Buckets[i]
		if b.TotalNodes > 0 && b.UnknownNodes == 0 {
			percent := float64(b.InServiceNodes) * 100 / float64(b.TotalNodes)
			b.InServicePercent = &percent
		}
	}

	var hasFaultSegments bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fault_segments)`).Scan(&hasFaultSegments); err != nil {
		return out, err
	}
	if hasFaultSegments {
		out.FaultSourceStatus = "live"
	}

	rows, err = tx.QueryContext(ctx, `SELECT f.segment_id,f.fault_key,f.window_start,j.type,o.object_ref
		FROM fault_segments f
		LEFT JOIN json_each(CASE WHEN json_valid(f.observation_refs) THEN
			CASE WHEN json_type(f.observation_refs)='array' THEN f.observation_refs ELSE '[]' END
			ELSE '[]' END) j ON 1
		LEFT JOIN observations o ON j.type='text' AND o.observation_id=j.value
		WHERE f.window_start>=? AND f.window_start<?
		ORDER BY f.window_start,f.segment_id`, firstStart, end)
	if err != nil {
		return out, err
	}
	var fault clusterFault
	addFault := func() {
		ref, category, ok := fault.attribution()
		if !ok {
			return
		}
		node, ok := nodes[ref]
		if !ok || node.createdAt > fault.started {
			return
		}
		bucket := int((fault.started - firstStart) / width.Milliseconds())
		out.Buckets[bucket].FaultCounts[category]++
	}
	for rows.Next() {
		var id, key string
		var started int64
		var refType, objectRef sql.NullString
		if err := rows.Scan(&id, &key, &started, &refType, &objectRef); err != nil {
			rows.Close()
			return out, err
		}
		if fault.id != id {
			if fault.id != "" {
				addFault()
			}
			fault = clusterFault{id: id, key: key, started: started}
		}
		if refType.Valid {
			fault.refs++
		}
		if objectRef.Valid && objectRef.String != "" {
			fault.resolved++
			if fault.objectRef == "" {
				fault.objectRef = objectRef.String
			} else if fault.objectRef != objectRef.String {
				fault.conflict = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if fault.id != "" {
		addFault()
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

// clusterFault never treats a merely matching suffix as proof of ownership:
// source and object identifiers may both contain colons. Resolved observations
// take precedence, and mixed, unresolved, or contradictory references are not
// counted. Digest-only references are not observation IDs; for those, the
// three-part legacy key remains usable only if its separators are unambiguous.
type clusterFault struct {
	id, key, objectRef string
	started            int64
	refs, resolved     int
	conflict           bool
}

func (f clusterFault) attribution() (string, string, bool) {
	if f.resolved > 0 {
		if f.resolved != f.refs || f.conflict {
			return "", "", false
		}
		// Historical fixture keys use object/metric; their linked observation
		// identifies the node, but the metric is not an observation kind.
		if strings.HasPrefix(f.key, f.objectRef+"/") {
			return f.objectRef, "other", true
		}
		if strings.Count(f.key, ":") >= 2 {
			kind := f.key[strings.LastIndex(f.key, ":")+1:]
			suffix := ":" + f.objectRef + ":" + kind
			if kind == "" || !strings.HasSuffix(f.key, suffix) || len(f.key) <= len(suffix) {
				return "", "", false
			}
			return f.objectRef, clusterFaultCategory(kind), true
		}
		if strings.Contains(f.key, "/") {
			return "", "", false
		}
		return f.objectRef, "other", true
	}
	parts := strings.Split(f.key, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], clusterFaultCategory(parts[2]), true
}

func clusterFaultCategory(kind string) string {
	switch kind {
	case "nhc", "oob_alert", "network", "service":
		return kind
	default:
		return "other"
	}
}

func migrateClusterTrendIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS fault_segments_window_start ON fault_segments(window_start,segment_id)`)
	return err
}

func stateExpression(state string) string {
	if strings.HasPrefix(state, "'") {
		return state
	}
	return "h." + state
}

func readClusterEvents(ctx context.Context, tx *sql.Tx, query string, nodes map[string]*clusterNode, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var event clusterStateEvent
		if err := rows.Scan(&ref, &event.at, &event.state); err != nil {
			return err
		}
		if node := nodes[ref]; node != nil && event.at >= node.createdAt {
			node.events = append(node.events, event)
		}
	}
	return rows.Err()
}

func normalizeClusterEvents(node *clusterNode, stamp int64) {
	// If creation itself is the last update, the current row is evidence of
	// its initial state even for older imports without a history row.
	if node.createdAt == node.updatedAt && node.createdAt <= stamp {
		node.events = append(node.events, clusterStateEvent{at: node.createdAt, state: node.state})
	}
	sort.Slice(node.events, func(i, j int) bool { return node.events[i].at < node.events[j].at })
	// Conflicting state changes within the same millisecond have no reliable
	// order in the source tables; mark the state unknown until later evidence.
	collapsed := node.events[:0]
	for _, event := range node.events {
		if len(collapsed) != 0 && collapsed[len(collapsed)-1].at == event.at {
			if collapsed[len(collapsed)-1].state != event.state {
				collapsed[len(collapsed)-1].state = ""
			}
			continue
		}
		collapsed = append(collapsed, event)
	}
	node.events = collapsed
	if node.updatedAt > stamp || node.createdAt > stamp {
		return
	}
	if len(node.events) == 0 {
		node.events = append(node.events, clusterStateEvent{at: node.updatedAt, state: node.state})
		return
	}
	latest := node.events[len(node.events)-1]
	if latest.state == node.state {
		return
	}
	// A current state different from the last recorded event proves that an
	// intervening change is missing. Its time is unknown until the row update.
	if latest.at < node.updatedAt {
		if latest.at+1 < node.updatedAt {
			node.events = append(node.events, clusterStateEvent{at: latest.at + 1})
		}
		node.events = append(node.events, clusterStateEvent{at: node.updatedAt, state: node.state})
	} else {
		if latest.at < stamp {
			node.events = append(node.events, clusterStateEvent{at: latest.at + 1})
		}
		node.events = append(node.events, clusterStateEvent{at: stamp, state: node.state})
	}
}
