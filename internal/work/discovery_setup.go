package work

import (
	"context"
	"database/sql"
	"encoding/json"
)

type DiscoverySetupStatus struct {
	State   string              `json:"state"`
	Plan    *DiscoverySetupPlan `json:"plan,omitempty"`
	LastRun *DiscoverySetupRun  `json:"last_run,omitempty"`
}

type DiscoverySetupPlan struct {
	ID     string              `json:"id"`
	Source string              `json:"source"`
	Scope  string              `json:"scope"`
	NextAt int64               `json:"next_at_unix_ms"`
	Scan   *DiscoveryScopeSpec `json:"scan,omitempty"`
}

type DiscoverySetupRun struct {
	Status           string                 `json:"status"`
	ScheduledAt      int64                  `json:"scheduled_at_unix_ms"`
	ObservationCount *int                   `json:"observation_count,omitempty"`
	Coverage         *discoveryScanCoverage `json:"coverage,omitempty"`
}

// ReadDiscoverySetupStatus projects the latest occurrence of an eligible plan.
// Readiness here describes installed configuration, without probing the source.
func (s *Store) ReadDiscoverySetupStatus(ctx context.Context, actor string, adapter DiscoveryScanSource, source, scope string) (DiscoverySetupStatus, error) {
	if actor == "" {
		return DiscoverySetupStatus{}, ErrAuthorizationDenied
	}
	filtered := source != "" || scope != ""
	if filtered && (!validDiscoveryLabel(source) || !validDiscoveryLabel(scope)) {
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
	rows, err := tx.QueryContext(ctx, `SELECT p.plan_id,p.source,p.scope,p.next_at,o.status,o.scheduled_at,coalesce(b.binding,''),coalesce(q.coverage,'')
FROM discovery_plans p LEFT JOIN discovery_occurrences o ON o.plan_id=p.plan_id AND o.scheduled_at=(
 SELECT max(last.scheduled_at) FROM discovery_occurrences last WHERE last.plan_id=p.plan_id AND last.plan_revision=p.revision
)
LEFT JOIN discovery_plan_bindings b ON b.plan_id=p.plan_id
LEFT JOIN discovery_scan_coverages q ON q.plan_id=o.plan_id AND q.scheduled_at=o.scheduled_at
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
		var rawBinding, rawCoverage string
		if err := rows.Scan(&plan.ID, &plan.Source, &plan.Scope, &plan.NextAt, &status, &scheduled, &rawBinding, &rawCoverage); err != nil {
			return DiscoverySetupStatus{}, err
		}
		if !adapter.AllowsDiscovery(plan.Source, plan.Scope) {
			continue
		}
		var bind *discoverySourceBinding
		if rawBinding != "" && json.Unmarshal([]byte(rawBinding), &bind) != nil {
			return DiscoverySetupStatus{}, ErrDiscoveryInput
		}
		if bind != nil {
			plan.Scan = &bind.Scan
		}
		confirmAvailable := func(out DiscoverySetupStatus) (DiscoverySetupStatus, error) {
			if bind == nil {
				return out, nil
			}
			// The single-writer SQLite connection must be released before
			// consulting the catalog, which can live in the same store.
			_ = rows.Close()
			_ = tx.Rollback()
			pin, err := pinDiscoverySource(ctx, adapter, plan.Source, plan.Scope, &bind.Scan)
			if err != nil || pin.fingerprint() != bind.fingerprint() {
				return DiscoverySetupStatus{State: "source_unavailable", Plan: &plan}, nil
			}
			return out, nil
		}
		ok, err := discoveryGranted(tx, actor, plan.Source, plan.Scope, false, stamp, bind.fingerprint())
		if err != nil {
			return DiscoverySetupStatus{}, err
		}
		if !ok {
			continue
		}
		out := DiscoverySetupStatus{State: "waiting_for_result", Plan: &plan}
		if !status.Valid {
			return confirmAvailable(out)
		}
		out.LastRun = &DiscoverySetupRun{Status: status.String, ScheduledAt: scheduled.Int64}
		if rawCoverage != "" && json.Unmarshal([]byte(rawCoverage), &out.LastRun.Coverage) != nil {
			return DiscoverySetupStatus{}, ErrDiscoveryInput
		}
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
		return confirmAvailable(out)
	}
	if err := rows.Err(); err != nil {
		return DiscoverySetupStatus{}, err
	}
	return DiscoverySetupStatus{State: "no_active_plan"}, nil
}
