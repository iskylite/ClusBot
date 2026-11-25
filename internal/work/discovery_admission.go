package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type DiscoveryCandidate struct {
	ID           string                `json:"id"`
	Source       string                `json:"source"`
	Scope        string                `json:"scope"`
	Serial       string                `json:"serial,omitempty"`
	Address      string                `json:"address"`
	SeenAt       int64                 `json:"seen_at"`
	State        string                `json:"state"`
	Asset        string                `json:"asset,omitempty"`
	CheckTask    string                `json:"check_task_id,omitempty"`
	Evidence     *DiscoveryObservation `json:"evidence,omitempty"`
	A2UISnapshot string                `json:"-"`
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
const discoveryCandidateSelect = `SELECT c.candidate_id,c.source,c.scope,c.serial,
d.address,d.seen_at,c.state,c.admitted_ref,c.check_task_id,coalesce(ev.evidence,''),coalesce(ev.fingerprint,'')`
const discoveryCandidateJoins = ` FROM discovery_candidates c JOIN discovery_observations d ON d.rowid=(
 SELECT x.rowid FROM discovery_observations x
 JOIN discovery_candidates accepted ON accepted.candidate_id=x.candidate_id
 JOIN discovery_plans p ON p.plan_id=x.plan_id
 JOIN discovery_occurrences o ON o.plan_id=x.plan_id AND o.scheduled_at=x.scheduled_at
 WHERE x.candidate_id=c.candidate_id AND p.source=c.source AND p.scope=c.scope AND o.status='done'
 ORDER BY x.scheduled_at DESC,(x.address=accepted.address AND x.seen_at=accepted.seen_at) DESC,x.seen_at,x.plan_id,x.ordinal LIMIT 1
)
LEFT JOIN discovery_observation_evidence ev ON ev.plan_id=d.plan_id AND ev.scheduled_at=d.scheduled_at AND ev.ordinal=d.ordinal`
const scopedDiscoveryCandidate = discoveryCandidateSelect + discoveryCandidateJoins
const snapshotDiscoveryCandidate = discoveryCandidateSelect + `,d.plan_id,d.scheduled_at,d.ordinal` + discoveryCandidateJoins

func (s *Store) ListDiscoveryCandidates(ctx context.Context, actor string) ([]DiscoveryCandidate, error) {
	stamp := now()
	rows, err := s.db.QueryContext(ctx, snapshotDiscoveryCandidate+`
WHERE EXISTS(SELECT 1 FROM authorizations a WHERE a.subject=? AND a.action='discover' AND a.object_ref=c.source
AND a.scope_label=c.scope AND a.revoked_at=0 AND a.valid_from<=? AND a.expires_at>?
AND CASE WHEN json_valid(a.param_constraints) THEN json_extract(a.param_constraints,'$.source')=c.source
AND json_extract(a.param_constraints,'$.scope')=c.scope
AND coalesce(json_extract(a.param_constraints,'$.binding_fingerprint'),'')=coalesce(ev.fingerprint,'') ELSE 0 END)
ORDER BY d.seen_at DESC,c.candidate_id LIMIT 500`, actor, stamp, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscoveryCandidate{}
	for rows.Next() {
		var c DiscoveryCandidate
		var rawEvidence, fingerprint, plan string
		var schedule, ordinal int64
		if err := rows.Scan(&c.ID, &c.Source, &c.Scope, &c.Serial, &c.Address, &c.SeenAt, &c.State, &c.Asset, &c.CheckTask,
			&rawEvidence, &fingerprint, &plan, &schedule, &ordinal); err != nil {
			return nil, err
		}
		if rawEvidence != "" && json.Unmarshal([]byte(rawEvidence), &c.Evidence) != nil {
			return nil, ErrDiscoveryInput
		}
		c.A2UISnapshot = digestDiscoveryCandidate(c, plan, schedule, ordinal, fingerprint, rawEvidence)
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) AdmitDiscoveryCandidate(ctx context.Context, actor string, req DiscoveryAdmission) (DiscoveryCandidate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	defer tx.Rollback()
	candidate, err := admitDiscoveryCandidateTx(ctx, tx, actor, req)
	if err != nil {
		return candidate, err
	}
	return candidate, tx.Commit()
}

func admitDiscoveryCandidateTx(ctx context.Context, tx *sql.Tx, actor string, req DiscoveryAdmission) (DiscoveryCandidate, error) {
	var c DiscoveryCandidate
	if actor == "" || req.Candidate == "" || req.ExpectedSeenAt <= 0 || req.ConfirmedSerial == "" {
		return c, ErrDiscoveryInput
	}
	for _, v := range []string{req.Owner, req.Location, req.Role} {
		if strings.TrimSpace(v) != v || len(v) == 0 || len(v) > 256 {
			return c, ErrDiscoveryInput
		}
	}
	var rawEvidence, fingerprint string
	err := tx.QueryRowContext(ctx, scopedDiscoveryCandidate+" WHERE c.candidate_id=?", req.Candidate).
		Scan(&c.ID, &c.Source, &c.Scope, &c.Serial, &c.Address, &c.SeenAt, &c.State, &c.Asset, &c.CheckTask, &rawEvidence, &fingerprint)
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	if rawEvidence != "" && (json.Unmarshal([]byte(rawEvidence), &c.Evidence) != nil || c.Evidence == nil) {
		return c, ErrDiscoveryInput
	}
	stamp := now()
	ok, err := discoveryGranted(tx, actor, c.Source, c.Scope, false, stamp, fingerprint)
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
		return c, nil
	}
	if c.SeenAt < stamp-int64(24*time.Hour/time.Millisecond) || c.SeenAt > stamp {
		return c, ErrDiscoveryInput
	}
	// An address is never used as the identity or as evidence to auto-merge.
	asset := "node:discovered:" + c.ID[:32]
	identity := "discovery:" + c.Source + ":" + c.Serial
	kind := "node"
	if c.Evidence != nil {
		ev := c.Evidence
		if !strings.EqualFold(ev.Serial, c.Serial) || ev.Address != c.Address || ev.ProviderTaskID == "" || ev.Sequence == 0 ||
			(ev.Kind != "node" && ev.Kind != "switch" && ev.Kind != "chassis") || (ev.SSHEndpoint == "" && ev.BMCEndpoint == "") {
			return c, ErrDiscoveryInput
		}
		kind = ev.Kind
		asset = kind + ":discovered:" + c.ID[:32]
		identity = "discovery:" + ev.SourceIdentity + ":" + kind + ":" + strings.ToUpper(ev.Serial)
	}
	var existing bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM ops_assets WHERE object_ref=? OR identity=?)", asset, identity).Scan(&existing); err != nil {
		return c, err
	}
	if existing {
		return c, ErrConflict
	}
	if _, err = upsertAsset(tx, OpsAsset{ObjectRef: asset, Kind: kind, Identity: identity, State: AssetStateRecheck, CurrentOwner: req.Owner}, stamp); err != nil {
		return c, err
	}
	if _, err = tx.Exec(`INSERT INTO discovery_admissions VALUES(?,?,?,?,?,?,?,?)`, c.ID, asset, req.Owner, req.Location, req.Role, c.Address, actor, stamp); err != nil {
		return c, err
	}
	if c.Evidence != nil {
		if _, err = tx.Exec("INSERT INTO discovery_admission_evidence VALUES(?,?,?)", c.ID, rawEvidence, fingerprint); err != nil {
			return c, err
		}
	}
	personal, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return c, err
	}
	if personal {
		if _, err := grantAuthorization(tx, Authorization{
			Subject: actor, Action: "read", ObjectRef: asset, ValidFrom: stamp,
			Owner: actor, Source: "discovery-admission",
		}, stamp); err != nil {
			return c, err
		}
	}
	c.State, c.Asset = "admitted", asset
	var hasPlan bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM discovery_plans WHERE actor=? AND enabled=1 AND source=? AND scope=? AND check_after_admit=1)`, actor, c.Source, c.Scope).Scan(&hasPlan); err != nil {
		return c, err
	}
	checkGranted, err := discoveryGranted(tx, actor, c.Source, c.Scope, true, stamp, fingerprint)
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
	return c, nil
}

func guardDiscoveryAdmissionCheck(tx *sql.Tx, task, principal string, stamp int64) error {
	if strings.HasPrefix(principal, "policy:discovery-drift:") {
		return guardDiscoveryDriftCheck(tx, task, stamp)
	}

	if !strings.HasPrefix(principal, "policy:discovery-check:") {
		return nil
	}
	var actor, source, scope, state, fingerprint string
	err := tx.QueryRow(`SELECT a.admitted_by,c.source,c.scope,c.state,coalesce(e.fingerprint,'') FROM discovery_candidates c
JOIN discovery_admissions a ON a.candidate_id=c.candidate_id
LEFT JOIN discovery_admission_evidence e ON e.candidate_id=c.candidate_id WHERE c.check_task_id=?`, task).
		Scan(&actor, &source, &scope, &state, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) || state != "admitted" {
		return ErrAuthorizationDenied
	}
	if err != nil {
		return err
	}
	ok, err := discoveryGranted(tx, actor, source, scope, true, stamp, fingerprint)
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

// DiscoveryAdmittedEndpoint returns the endpoint recorded in the reviewed
// admission evidence. It never returns the newest scan address in its place;
// address drift is an observation, not authorization to move an endpoint.
func (s *Store) DiscoveryAdmittedEndpoint(ctx context.Context, objectRef, protocol string) (string, error) {
	q := s.db
	if s.readPool != nil && s.readPool.db != nil {
		q = s.readPool.db
	}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT e.evidence FROM discovery_admissions a
JOIN discovery_admission_evidence e ON e.candidate_id=a.candidate_id WHERE a.object_ref=?`, objectRef).Scan(&raw)
	if err != nil {
		return "", err
	}
	var evidence DiscoveryObservation
	if json.Unmarshal([]byte(raw), &evidence) != nil {
		return "", ErrDiscoveryInput
	}
	var endpoint string
	switch protocol {
	case "ssh":
		endpoint = evidence.SSHEndpoint
	case "redfish":
		endpoint = evidence.BMCEndpoint
	default:
		return "", ErrDiscoveryInput
	}
	if !validObservedEndpoint(endpoint) {
		return "", ErrDiscoveryInput
	}
	return endpoint, nil
}
