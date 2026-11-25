package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type DiscoveryCandidateObservationPage struct {
	CandidateID  string                          `json:"candidate_id"`
	EvidenceKind string                          `json:"evidence_kind"`
	Items        []DiscoveryCandidateObservation `json:"items"`
	NextBefore   string                          `json:"next_before,omitempty"`
}

type DiscoveryCandidateObservation struct {
	DiscoveryObservationView
	PlanID                string `json:"plan_id"`
	Source                string `json:"source"`
	Scope                 string `json:"scope"`
	AddressConflictInScan bool   `json:"address_conflict_in_scan"`
}

// Only currently authorized receipts of plans owned by the current actor
// are visible. Neither the candidate's mutable address nor observations from
// another scope are used to assess an address conflict.
const visibleCandidateObservationSQL = `
FROM discovery_observations d
JOIN discovery_plans p ON p.plan_id=d.plan_id
JOIN discovery_occurrences o ON o.plan_id=d.plan_id AND o.scheduled_at=d.scheduled_at
LEFT JOIN discovery_observation_evidence ev ON ev.plan_id=d.plan_id AND ev.scheduled_at=d.scheduled_at AND ev.ordinal=d.ordinal
WHERE d.candidate_id=? AND p.actor=? AND o.status='done'
AND EXISTS (
 SELECT 1 FROM authorizations a WHERE a.subject=p.actor AND a.action='discover' AND a.object_ref=p.source
 AND a.scope_label=p.scope AND a.revoked_at=0 AND a.valid_from<=? AND a.expires_at>?
 AND CASE WHEN json_valid(a.param_constraints) THEN json_extract(a.param_constraints,'$.source')=p.source
 AND json_extract(a.param_constraints,'$.scope')=p.scope
 AND coalesce(json_extract(a.param_constraints,'$.binding_fingerprint'),'')=coalesce(ev.fingerprint,'') ELSE 0 END
)`

func (s *Store) ListDiscoveryCandidateObservations(ctx context.Context, actor, candidate, before string, limit int) (DiscoveryCandidateObservationPage, error) {
	if actor == "" || !validBenchmarkSHA(candidate) || limit < 1 || limit > 100 {
		return DiscoveryCandidateObservationPage{}, ErrDiscoveryInput
	}
	var seen, scheduled, ordinal int64
	var plan string
	if before != "" {
		fields := strings.Split(before, ":")
		if len(fields) != 4 {
			return DiscoveryCandidateObservationPage{}, ErrDiscoveryInput
		}
		var err1, err2, err3 error
		seen, err1 = strconv.ParseInt(fields[0], 10, 64)
		scheduled, err2 = strconv.ParseInt(fields[1], 10, 64)
		plan = fields[2]
		ordinal, err3 = strconv.ParseInt(fields[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || seen < 1 || scheduled < 1 || !validBenchKey(plan) ||
			ordinal < 0 || ordinal > 4095 || before != fmt.Sprintf("%d:%d:%s:%d", seen, scheduled, plan, ordinal) {
			return DiscoveryCandidateObservationPage{}, ErrDiscoveryInput
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryCandidateObservationPage{}, err
	}
	defer tx.Rollback()
	stamp := now()
	args := []any{candidate, actor, stamp, stamp}
	var visible bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 "+visibleCandidateObservationSQL+")", args...).Scan(&visible); err != nil {
		return DiscoveryCandidateObservationPage{}, err
	}
	if !visible {
		return DiscoveryCandidateObservationPage{}, sql.ErrNoRows
	}
	q := `SELECT d.candidate_id,d.address,d.seen_at,d.scheduled_at,o.plan_revision,coalesce(o.task_id,''),
p.plan_id,p.source,p.scope,d.ordinal,
EXISTS(SELECT 1 FROM discovery_observations sibling
 WHERE sibling.plan_id=d.plan_id AND sibling.scheduled_at=d.scheduled_at
 AND sibling.candidate_id=d.candidate_id AND sibling.address<>d.address),coalesce(ev.evidence,'')
` + visibleCandidateObservationSQL
	if before != "" {
		q += " AND (d.seen_at,d.scheduled_at,d.plan_id,d.ordinal) < (?,?,?,?)"
		args = append(args, seen, scheduled, plan, ordinal)
	}
	rows, err := tx.QueryContext(ctx, q+" ORDER BY d.seen_at DESC,d.scheduled_at DESC,d.plan_id DESC,d.ordinal DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return DiscoveryCandidateObservationPage{}, err
	}
	defer rows.Close()
	page := DiscoveryCandidateObservationPage{CandidateID: candidate, EvidenceKind: "reported_addresses_only",
		Items: []DiscoveryCandidateObservation{}}
	var last string
	for rows.Next() {
		if len(page.Items) == limit {
			page.NextBefore = last
			break
		}
		var item DiscoveryCandidateObservation
		var rawEvidence string
		if err := rows.Scan(&item.CandidateID, &item.Address, &item.SeenAt, &item.ScheduledAt, &item.PlanRevision, &item.ScanTaskID,
			&item.PlanID, &item.Source, &item.Scope, &ordinal, &item.AddressConflictInScan, &rawEvidence); err != nil {
			return DiscoveryCandidateObservationPage{}, err
		}
		if rawEvidence != "" && json.Unmarshal([]byte(rawEvidence), &item.Evidence) != nil {
			return DiscoveryCandidateObservationPage{}, ErrDiscoveryInput
		}
		last = fmt.Sprintf("%d:%d:%s:%d", item.SeenAt, item.ScheduledAt, item.PlanID, ordinal)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}
