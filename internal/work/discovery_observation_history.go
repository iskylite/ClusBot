package work

import (
	"context"
	"database/sql"
	"encoding/json"
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
	CandidateID  string                `json:"candidate_id"`
	Address      string                `json:"address"`
	SeenAt       int64                 `json:"seen_at_unix_ms"`
	ScheduledAt  int64                 `json:"scheduled_at_unix_ms"`
	PlanRevision int                   `json:"plan_revision"`
	ScanTaskID   string                `json:"scan_task_id,omitempty"`
	Evidence     *DiscoveryObservation `json:"evidence,omitempty"`
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
	var rawBinding string
	if err := tx.QueryRowContext(ctx, `SELECT p.source,p.scope,coalesce(b.binding,'') FROM discovery_plans p
LEFT JOIN discovery_plan_bindings b ON b.plan_id=p.plan_id WHERE p.actor=? AND p.plan_id=?`, actor, plan).
		Scan(&out.Source, &out.Scope, &rawBinding); err != nil {
		return DiscoveryObservationPage{}, err
	}
	var binding *discoverySourceBinding
	if rawBinding != "" && json.Unmarshal([]byte(rawBinding), &binding) != nil {
		return DiscoveryObservationPage{}, ErrDiscoveryInput
	}
	allowed, err := discoveryGranted(tx, actor, out.Source, out.Scope, false, now(), binding.fingerprint())
	if err != nil {
		return DiscoveryObservationPage{}, err
	}
	if !allowed {
		return DiscoveryObservationPage{}, sql.ErrNoRows
	}

	// Take address and observation time from the immutable scan row, never
	// from the current mutable candidate. Disabled plans retain history.
	query := `SELECT d.candidate_id,d.address,d.seen_at,d.scheduled_at,d.ordinal,o.plan_revision,coalesce(o.task_id,''),coalesce(ev.evidence,'')
FROM discovery_observations d JOIN discovery_occurrences o
ON o.plan_id=d.plan_id AND o.scheduled_at=d.scheduled_at
LEFT JOIN discovery_observation_evidence ev ON ev.plan_id=d.plan_id AND ev.scheduled_at=d.scheduled_at AND ev.ordinal=d.ordinal
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
		var rawEvidence string
		if err := rows.Scan(&item.CandidateID, &item.Address, &item.SeenAt, &item.ScheduledAt, &ordinal, &item.PlanRevision, &item.ScanTaskID, &rawEvidence); err != nil {
			return DiscoveryObservationPage{}, err
		}
		if rawEvidence != "" && json.Unmarshal([]byte(rawEvidence), &item.Evidence) != nil {
			return DiscoveryObservationPage{}, ErrDiscoveryInput
		}
		last = fmt.Sprintf("%d:%d", item.ScheduledAt, ordinal)
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}

// Do not trust or rewrite historical candidate addresses; the scoped read
// projects immutable receipts. Quarantine older candidates whose later
// cross-scope receipt may have been recorded without marking conflict.
func migrateDiscoveryObservationCandidateIndex(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS discovery_observations_by_candidate
ON discovery_observations(candidate_id,seen_at DESC,scheduled_at DESC,plan_id DESC,ordinal DESC)`); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE discovery_candidates AS c SET state='conflict'
WHERE c.state<>'conflict' AND EXISTS(
 SELECT 1 FROM discovery_observations x
 JOIN discovery_plans p ON p.plan_id=x.plan_id
 JOIN discovery_occurrences o ON o.plan_id=x.plan_id AND o.scheduled_at=x.scheduled_at
 WHERE x.candidate_id=c.candidate_id AND (p.source<>c.source OR p.scope<>c.scope) AND o.status='done'
)`)
	return err
}
