package work

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

type DiscoveryObservationPage struct {
	PlanID     string                     `json:"plan_id"`
	Source     string                     `json:"source"`
	Scope      string                     `json:"scope"`
	Items      []DiscoveryObservationView `json:"items"`
	NextBefore string                     `json:"next_before,omitempty"`
}

type DiscoveryObservationView struct {
	CandidateID  string `json:"candidate_id"`
	Address      string `json:"address"`
	SeenAt       int64  `json:"seen_at_unix_ms"`
	ScheduledAt  int64  `json:"scheduled_at_unix_ms"`
	PlanRevision int    `json:"plan_revision"`
	ScanTaskID   string `json:"scan_task_id,omitempty"`
}

// ListDiscoveryObservations returns source-reported address observations from
// completed scans of this plan. This is not a health/capacity signal, an
// authoritative management address, or a claim that unobserved assets are gone.
func (s *Store) ListDiscoveryObservations(ctx context.Context, actor, plan, before string, limit int) (DiscoveryObservationPage, error) {
	if actor == "" {
		return DiscoveryObservationPage{}, ErrAuthorizationDenied
	}
	if !validBenchKey(plan) || limit < 1 || limit > 100 {
		return DiscoveryObservationPage{}, ErrDiscoveryInput
	}
	var at, ordinal int64
	if before != "" {
		left, right, ok := strings.Cut(before, ":")
		var aerr, oerr error
		at, aerr = strconv.ParseInt(left, 10, 64)
		ordinal, oerr = strconv.ParseInt(right, 10, 64)
		if !ok || aerr != nil || oerr != nil || at <= 0 || ordinal < 0 || ordinal > 4095 || before != fmt.Sprintf("%d:%d", at, ordinal) {
			return DiscoveryObservationPage{}, ErrDiscoveryInput
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryObservationPage{}, err
	}
	defer tx.Rollback()
	out := DiscoveryObservationPage{PlanID: plan, Items: []DiscoveryObservationView{}}
	if err := tx.QueryRowContext(ctx, "SELECT source,scope FROM discovery_plans WHERE actor=? AND plan_id=?", actor, plan).Scan(&out.Source, &out.Scope); err != nil {
		return DiscoveryObservationPage{}, err
	}
	allowed, err := discoveryGranted(tx, actor, out.Source, out.Scope, false, now())
	if err != nil {
		return DiscoveryObservationPage{}, err
	}
	if !allowed {
		return DiscoveryObservationPage{}, sql.ErrNoRows
	}

	// Take address and observation time from the immutable scan row, never
	// from the current mutable candidate. Disabled plans retain history.
	query := `SELECT d.candidate_id,d.address,d.seen_at,d.scheduled_at,d.ordinal,o.plan_revision,coalesce(o.task_id,'')
FROM discovery_observations d JOIN discovery_occurrences o
ON o.plan_id=d.plan_id AND o.scheduled_at=d.scheduled_at
WHERE d.plan_id=? AND o.status='done'`
	args := []any{plan}
	if before != "" {
		query += " AND (d.scheduled_at,d.ordinal) < (?,?)"
		args = append(args, at, ordinal)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY d.scheduled_at DESC,d.ordinal DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return DiscoveryObservationPage{}, err
	}
	defer rows.Close()
	var last string
	for rows.Next() {
		if len(out.Items) == limit {
			out.NextBefore = last
			break
		}
		var item DiscoveryObservationView
		if err := rows.Scan(&item.CandidateID, &item.Address, &item.SeenAt, &item.ScheduledAt, &ordinal, &item.PlanRevision, &item.ScanTaskID); err != nil {
			return DiscoveryObservationPage{}, err
		}
		last = fmt.Sprintf("%d:%d", item.ScheduledAt, ordinal)
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}
