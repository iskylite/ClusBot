package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type DiscoveryCandidate struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Scope     string `json:"scope"`
	Serial    string `json:"serial,omitempty"`
	Address   string `json:"address"`
	SeenAt    int64  `json:"seen_at"`
	State     string `json:"state"`
	Asset     string `json:"asset,omitempty"`
	CheckTask string `json:"check_task_id,omitempty"`
}
type DiscoveryAdmission struct {
	Candidate       string `json:"candidate"`
	ConfirmedSerial string `json:"confirmed_serial"`
	ExpectedSeenAt  int64  `json:"expected_seen_at"`
	Owner           string `json:"owner"`
	Location        string `json:"location"`
	Role            string `json:"role"`
}

// Historical builds could overwrite the candidate address using a different
// scope while retaining the original scope. Use only the last successful
// receipt in the original scope. Candidates with no such receipt fail closed.
const scopedDiscoveryCandidate = `SELECT c.candidate_id,c.source,c.scope,c.serial,
d.address,d.seen_at,c.state,c.admitted_ref,c.check_task_id
FROM discovery_candidates c JOIN discovery_observations d ON d.rowid=(
 SELECT x.rowid FROM discovery_observations x
 JOIN discovery_plans p ON p.plan_id=x.plan_id
 JOIN discovery_occurrences o ON o.plan_id=x.plan_id AND o.scheduled_at=x.scheduled_at
 WHERE x.candidate_id=c.candidate_id AND p.source=c.source AND p.scope=c.scope AND o.status='done'
 ORDER BY x.seen_at DESC,x.scheduled_at DESC,x.plan_id DESC,x.ordinal DESC LIMIT 1
)`

func (s *Store) ListDiscoveryCandidates(ctx context.Context, actor string) ([]DiscoveryCandidate, error) {
	stamp := now()
	rows, err := s.db.QueryContext(ctx, scopedDiscoveryCandidate+`
WHERE EXISTS(SELECT 1 FROM authorizations a WHERE a.subject=? AND a.action='discover' AND a.object_ref=c.source
AND a.scope_label=c.scope AND a.revoked_at=0 AND a.valid_from<=? AND a.expires_at>?
AND CASE WHEN json_valid(a.param_constraints) THEN json_extract(a.param_constraints,'$.source')=c.source
AND json_extract(a.param_constraints,'$.scope')=c.scope ELSE 0 END)
ORDER BY d.seen_at DESC,c.candidate_id LIMIT 500`, actor, stamp, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscoveryCandidate{}
	for rows.Next() {
		var c DiscoveryCandidate
		if err := rows.Scan(&c.ID, &c.Source, &c.Scope, &c.Serial, &c.Address, &c.SeenAt, &c.State, &c.Asset, &c.CheckTask); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) AdmitDiscoveryCandidate(ctx context.Context, actor string, req DiscoveryAdmission) (DiscoveryCandidate, error) {
	var c DiscoveryCandidate
	if actor == "" || req.Candidate == "" || req.ExpectedSeenAt <= 0 || req.ConfirmedSerial == "" {
		return c, ErrDiscoveryInput
	}
	for _, v := range []string{req.Owner, req.Location, req.Role} {
		if strings.TrimSpace(v) != v || len(v) == 0 || len(v) > 256 {
			return c, ErrDiscoveryInput
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, scopedDiscoveryCandidate+" WHERE c.candidate_id=?", req.Candidate).
		Scan(&c.ID, &c.Source, &c.Scope, &c.Serial, &c.Address, &c.SeenAt, &c.State, &c.Asset, &c.CheckTask)
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	stamp := now()
	ok, err := discoveryGranted(tx, actor, c.Source, c.Scope, false, stamp)
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	if !ok {
		return DiscoveryCandidate{}, sql.ErrNoRows
	}
	if c.Serial != req.ConfirmedSerial || c.SeenAt != req.ExpectedSeenAt || (c.State != "observed" && c.State != "admitted") {
		return c, fmt.Errorf("%w：身份或观测版本冲突，不可纳管", ErrConflict)
	}
	if c.Asset != "" {
		var owner, loc, role string
		if err = tx.QueryRow("SELECT owner,location,role FROM discovery_admissions WHERE candidate_id=?", c.ID).Scan(&owner, &loc, &role); err != nil {
			return c, err
		}
		if owner != req.Owner || loc != req.Location || role != req.Role {
			return c, ErrConflict
		}
		return c, tx.Commit()
	}
	if c.SeenAt < stamp-int64(24*time.Hour/time.Millisecond) || c.SeenAt > stamp {
		return c, ErrDiscoveryInput
	}
	// An address is never used as the identity or as evidence to auto-merge.
	asset := "node:discovered:" + c.ID[:32]
	identity := "discovery:" + c.Source + ":" + c.Serial
	var existing bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM ops_assets WHERE object_ref=? OR identity=?)", asset, identity).Scan(&existing); err != nil {
		return c, err
	}
	if existing {
		return c, ErrConflict
	}
	if _, err = upsertAsset(tx, OpsAsset{ObjectRef: asset, Kind: "node", Identity: identity, State: AssetStateRecheck, CurrentOwner: req.Owner}, stamp); err != nil {
		return c, err
	}
	if _, err = tx.Exec(`INSERT INTO discovery_admissions VALUES(?,?,?,?,?,?,?,?)`, c.ID, asset, req.Owner, req.Location, req.Role, c.Address, actor, stamp); err != nil {
		return c, err
	}
	c.State, c.Asset = "admitted", asset
	var hasPlan bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM discovery_plans WHERE actor=? AND enabled=1 AND source=? AND scope=? AND check_after_admit=1)`, actor, c.Source, c.Scope).Scan(&hasPlan); err != nil {
		return c, err
	}
	checkGranted, err := discoveryGranted(tx, actor, c.Source, c.Scope, true, stamp)
	if err != nil {
		return c, err
	}
	if hasPlan && checkGranted {
		principal := "policy:discovery-check:" + c.ID
		if _, err = grantAuthorization(tx, Authorization{Subject: principal, Action: "read", ObjectRef: asset, Source: "discovery-admission", Owner: actor,
			ValidFrom: stamp, ExpiresAt: stamp + int64(5*time.Minute/time.Millisecond)}, stamp); err != nil {
			return c, err
		}
		child, err := submitAssetAcceptanceQuery(tx, "discovery-admission:"+c.ID, asset, principal, stamp)
		if err != nil {
			return c, err
		}
		c.CheckTask = child.TaskID
	}
	if _, err = tx.Exec("UPDATE discovery_candidates SET state='admitted',admitted_ref=?,check_task_id=? WHERE candidate_id=?", asset, c.CheckTask, c.ID); err != nil {
		return c, err
	}
	if err = recordAudit(tx, AuditEvent{TaskID: c.CheckTask, Event: "discovery_asset_admitted", Actor: actor, DetailRef: asset, OccurredAt: stamp}); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func guardDiscoveryAdmissionCheck(tx *sql.Tx, task, principal string, stamp int64) error {
	if strings.HasPrefix(principal, "policy:discovery-drift:") {
		return guardDiscoveryDriftCheck(tx, task, stamp)
	}

	if !strings.HasPrefix(principal, "policy:discovery-check:") {
		return nil
	}
	var actor, source, scope, state string
	err := tx.QueryRow(`SELECT a.admitted_by,c.source,c.scope,c.state FROM discovery_candidates c
JOIN discovery_admissions a ON a.candidate_id=c.candidate_id WHERE c.check_task_id=?`, task).
		Scan(&actor, &source, &scope, &state)
	if errors.Is(err, sql.ErrNoRows) || state != "admitted" {
		return ErrAuthorizationDenied
	}
	if err != nil {
		return err
	}
	ok, err := discoveryGranted(tx, actor, source, scope, true, stamp)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAuthorizationDenied
	}
	var enabled bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM discovery_plans WHERE actor=? AND source=? AND scope=? AND enabled=1 AND check_after_admit=1)`, actor, source, scope).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return ErrAuthorizationDenied
	}
	return nil
}
