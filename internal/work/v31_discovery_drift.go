package work

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const discoveryDriftSchema = `
CREATE TABLE IF NOT EXISTS discovery_drift_checks (
 candidate_id TEXT NOT NULL REFERENCES discovery_candidates(candidate_id),
 plan_id TEXT NOT NULL REFERENCES discovery_plans(plan_id), scheduled_at INTEGER NOT NULL,
 actor TEXT NOT NULL, previous_address TEXT NOT NULL, current_address TEXT NOT NULL,
 task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
 PRIMARY KEY(candidate_id,plan_id,scheduled_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS discovery_drift_by_task ON discovery_drift_checks(task_id) WHERE task_id IS NOT NULL;
`

func migrateV31DiscoveryDrift(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "discovery_plans", "check_on_address_drift", "ALTER TABLE discovery_plans ADD COLUMN check_on_address_drift INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.Exec(discoveryDriftSchema)
	return err
}

func discoveryDriftGranted(tx *sql.Tx, actor, source, scope string, stamp int64) (bool, error) {
	var ok bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='discover' AND object_ref=? AND scope_label=?
AND revoked_at=0 AND valid_from<=? AND expires_at>? AND CASE WHEN json_valid(param_constraints) THEN
json_extract(param_constraints,'$.source')=? AND json_extract(param_constraints,'$.scope')=?
AND json_extract(param_constraints,'$.allow_drift_check')=1 ELSE 0 END)`, actor, source, scope, stamp, stamp, source, scope).Scan(&ok)
	return ok, err
}

// evaluateDiscoveryDrift observes a fact, not a command. A live plan and a
// separate typed permission may cause one finite, read-only check for a known
// asset; no Goal, repair, address remap, or state transition is implied.
func evaluateDiscoveryDrift(tx *sql.Tx, d discoveryTick, candidate, oldAddress, newAddress, asset string, stamp int64) error {
	var enabled bool
	if err := tx.QueryRow("SELECT enabled=1 AND check_on_address_drift=1 FROM discovery_plans WHERE plan_id=? AND revision=?", d.plan, d.revision).Scan(&enabled); err != nil {
		return err
	}
	granted, err := discoveryDriftGranted(tx, d.actor, d.source, d.scope, stamp)
	if err != nil {
		return err
	}
	a, err := readAsset(tx, asset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var task any
	if enabled && granted && err == nil && a.State != AssetStateRetired && a.State != AssetStatePending {
		intent := fmt.Sprintf("discovery-drift:%s:%s:%d", candidate, d.plan, d.at)
		principal := "policy:discovery-drift:" + digest(intent)
		if _, err = grantAuthorization(tx, Authorization{Subject: principal, Action: "read", ObjectRef: asset, Source: "discovery-drift",
			Owner: d.actor, ValidFrom: stamp, ExpiresAt: stamp + int64(5*time.Minute/time.Millisecond)}, stamp); err != nil {
			return err
		}
		r, err := submitAssetAcceptanceQuery(tx, intent, asset, principal, stamp)
		if err != nil {
			return err
		}
		task = r.TaskID
	}
	if _, err = tx.Exec(`INSERT INTO discovery_drift_checks VALUES(?,?,?,?,?,?,?)`, candidate, d.plan, d.at, d.actor, oldAddress, newAddress, task); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{TaskID: d.task, Event: "discovery_known_address_drift", Actor: "system", DetailRef: asset,
		Summary: "发现已纳管资产的地址观测变化；未改写资产管理端点或状态", OccurredAt: stamp})
}

func guardDiscoveryDriftCheck(tx *sql.Tx, task string, stamp int64) error {
	var actor, source, scope, state string
	var enabled bool
	err := tx.QueryRow(`SELECT w.actor,c.source,c.scope,c.state,p.enabled=1 AND p.check_on_address_drift=1
FROM discovery_drift_checks w JOIN discovery_candidates c ON c.candidate_id=w.candidate_id
JOIN discovery_plans p ON p.plan_id=w.plan_id WHERE w.task_id=?`, task).Scan(&actor, &source, &scope, &state, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthorizationDenied
	}
	if err != nil {
		return err
	}
	if !enabled || state != "admitted" {
		return ErrAuthorizationDenied
	}
	allowed, err := discoveryDriftGranted(tx, actor, source, scope, stamp)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrAuthorizationDenied
	}
	return nil
}
