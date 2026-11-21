package work

import (
	"context"
	"database/sql"
)

type DiscoverySetupStatus struct {
	State   string              `json:"state"`
	Plan    *DiscoverySetupPlan `json:"plan,omitempty"`
	LastRun *DiscoverySetupRun  `json:"last_run,omitempty"`
}

type DiscoverySetupPlan struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Scope  string `json:"scope"`
	NextAt int64  `json:"next_at_unix_ms"`
}

type DiscoverySetupRun struct {
	Status           string `json:"status"`
	ScheduledAt      int64  `json:"scheduled_at_unix_ms"`
	ObservationCount *int   `json:"observation_count,omitempty"`
}

// ReadDiscoverySetupStatus projects the latest occurrence of an eligible plan.
// Readiness here describes installed configuration, without probing the source.
func (s *Store) ReadDiscoverySetupStatus(ctx context.Context, actor string, adapter DiscoveryScanSource, source, scope string) (DiscoverySetupStatus, error) {
	if actor == "" {
		return DiscoverySetupStatus{}, ErrAuthorizationDenied
	}
	filtered := source != "" || scope != ""
	if filtered && (!validBenchKey(source) || !validBenchKey(scope)) {
		return DiscoverySetupStatus{}, ErrDiscoveryInput
	}
	if adapter == nil || (filtered && !adapter.AllowsDiscovery(source, scope)) {
		return DiscoverySetupStatus{State: "source_unavailable"}, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiscoverySetupStatus{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT p.plan_id,p.source,p.scope,p.next_at,o.status,o.scheduled_at
FROM discovery_plans p LEFT JOIN discovery_occurrences o ON o.plan_id=p.plan_id AND o.scheduled_at=(
 SELECT max(last.scheduled_at) FROM discovery_occurrences last WHERE last.plan_id=p.plan_id AND last.plan_revision=p.revision
)
WHERE p.actor=? AND p.enabled=1 AND (?='' OR p.source=? AND p.scope=?)
ORDER BY o.scheduled_at DESC,p.next_at,p.plan_id`, actor, source, source, scope)
	if err != nil {
		return DiscoverySetupStatus{}, err
	}
	defer rows.Close()
	stamp := now()
	for rows.Next() {
		var plan DiscoverySetupPlan
		var status sql.NullString
		var scheduled sql.NullInt64
		if err := rows.Scan(&plan.ID, &plan.Source, &plan.Scope, &plan.NextAt, &status, &scheduled); err != nil {
			return DiscoverySetupStatus{}, err
		}
		if !adapter.AllowsDiscovery(plan.Source, plan.Scope) {
			continue
		}
		ok, err := discoveryGranted(tx, actor, plan.Source, plan.Scope, false, stamp)
		if err != nil {
			return DiscoverySetupStatus{}, err
		}
		if !ok {
			continue
		}
		out := DiscoverySetupStatus{State: "waiting_for_result", Plan: &plan}
		if !status.Valid {
			return out, nil
		}
		out.LastRun = &DiscoverySetupRun{Status: status.String, ScheduledAt: scheduled.Int64}
		switch status.String {
		case "queued", "running":
		case "failed":
			out.State = "run_failed"
		case "skipped":
			out.State = "run_skipped"
		case "done":
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM discovery_observations WHERE plan_id=? AND scheduled_at=?`,
				plan.ID, scheduled.Int64).Scan(&count); err != nil {
				return DiscoverySetupStatus{}, err
			}
			out.LastRun.ObservationCount = &count
			out.State = "observations_available"
			if count == 0 {
				out.State = "no_observations"
			}
		default:
			return DiscoverySetupStatus{}, ErrState
		}
		return out, nil
	}
	if err := rows.Err(); err != nil {
		return DiscoverySetupStatus{}, err
	}
	return DiscoverySetupStatus{State: "no_active_plan"}, nil
}
