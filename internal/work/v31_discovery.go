package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"
)

var ErrDiscoveryInput = errors.New("发现配置、来源或候选不受支持")

const v31DiscoverySchema = `
CREATE TABLE IF NOT EXISTS discovery_source_versions (
 source_key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS discovery_plans (
 plan_id TEXT PRIMARY KEY, actor TEXT NOT NULL, source TEXT NOT NULL, scope TEXT NOT NULL,
 period_ms INTEGER NOT NULL, next_at INTEGER NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
 check_after_admit INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS discovery_plans_due ON discovery_plans(enabled,next_at);
CREATE TABLE IF NOT EXISTS discovery_occurrences (
 plan_id TEXT NOT NULL REFERENCES discovery_plans(plan_id), scheduled_at INTEGER NOT NULL,
 plan_revision INTEGER NOT NULL, task_id TEXT REFERENCES tasks(id), status TEXT NOT NULL,
 next_attempt INTEGER NOT NULL DEFAULT 0, generation INTEGER NOT NULL DEFAULT 0,
 summary TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(plan_id,scheduled_at)
);
CREATE INDEX IF NOT EXISTS discovery_work_due ON discovery_occurrences(status,next_attempt,scheduled_at);
CREATE TABLE IF NOT EXISTS discovery_candidates (
 candidate_id TEXT PRIMARY KEY, source TEXT NOT NULL, scope TEXT NOT NULL, serial TEXT NOT NULL,
 address TEXT NOT NULL, seen_at INTEGER NOT NULL, state TEXT NOT NULL,
 admitted_ref TEXT NOT NULL DEFAULT '',
 check_task_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS discovery_observations (
 plan_id TEXT NOT NULL, scheduled_at INTEGER NOT NULL, ordinal INTEGER NOT NULL,
 candidate_id TEXT NOT NULL REFERENCES discovery_candidates(candidate_id),
 address TEXT NOT NULL, seen_at INTEGER NOT NULL, PRIMARY KEY(plan_id,scheduled_at,ordinal)
);
CREATE TABLE IF NOT EXISTS discovery_admissions (
 candidate_id TEXT PRIMARY KEY REFERENCES discovery_candidates(candidate_id),
 object_ref TEXT NOT NULL REFERENCES ops_assets(object_ref), owner TEXT NOT NULL,
 location TEXT NOT NULL, role TEXT NOT NULL, source_address TEXT NOT NULL,
 admitted_by TEXT NOT NULL, admitted_at INTEGER NOT NULL
);
`

func migrateV31Discovery(tx *sql.Tx) error { _, err := tx.Exec(v31DiscoverySchema); return err }

type DiscoveryScanSource interface {
	AllowsDiscovery(id, scope string) bool
	ScanDiscovery(ctx context.Context, id, scope string) ([]DiscoveryObservation, error)
}
type DiscoveryObservation struct {
	Serial  string `json:"serial,omitempty"`
	Address string `json:"address"`
}
type DiscoveryPlanRequest struct {
	ID                  string `json:"id"`
	Source              string `json:"source"`
	Scope               string `json:"scope"`
	PeriodSeconds       int64  `json:"period_seconds"`
	FirstAt             int64  `json:"first_at_unix_ms"`
	CheckAfterAdmit     bool   `json:"check_after_admit"`
	CheckOnAddressDrift bool   `json:"check_on_address_drift"`
}
type DiscoveryGrant struct {
	Source              string `json:"source"`
	Scope               string `json:"scope"`
	ExpiresAt           int64  `json:"expires_at_unix_ms"`
	AllowAdmissionCheck bool   `json:"allow_admission_check"`
	AllowDriftCheck     bool   `json:"allow_drift_check"`
}

func (s *Store) GrantDiscovery(ctx context.Context, actor string, g DiscoveryGrant, source DiscoveryScanSource) (string, error) {
	if actor == "" || source == nil || !source.AllowsDiscovery(g.Source, g.Scope) || g.ExpiresAt <= now() {
		return "", ErrDiscoveryInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	a, err := grantAuthorization(tx, Authorization{Subject: actor, Action: "discover", ObjectRef: g.Source, ScopeLabel: g.Scope, ExpiresAt: g.ExpiresAt,
		Owner: actor, Source: "explicit-discovery-settings", ParamConstraints: toJSON(g)}, now())
	if err != nil {
		return "", err
	}
	return a.AuthID, tx.Commit()
}
func discoveryGranted(tx *sql.Tx, actor, source, scope string, check bool, stamp int64) (bool, error) {
	var ok bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='discover' AND object_ref=? AND scope_label=?
 AND revoked_at=0 AND valid_from<=? AND expires_at>? AND json_valid(param_constraints)
 AND json_extract(param_constraints,'$.source')=? AND json_extract(param_constraints,'$.scope')=?
 AND (?=0 OR json_extract(param_constraints,'$.allow_admission_check')=1))`, actor, source, scope, stamp, stamp, source, scope, check).Scan(&ok)
	return ok, err
}
func (s *Store) PublishDiscoveryPlan(ctx context.Context, actor string, p DiscoveryPlanRequest, source DiscoveryScanSource) error {
	stamp := now()
	if actor == "" || !validBenchKey(p.ID) || source == nil || !source.AllowsDiscovery(p.Source, p.Scope) || p.PeriodSeconds < 60 ||
		p.PeriodSeconds > 7*86400 || p.FirstAt < stamp || p.FirstAt > stamp+int64(31*24*time.Hour/time.Millisecond) {
		return ErrDiscoveryInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	allowed, err := discoveryGranted(tx, actor, p.Source, p.Scope, false, stamp)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrAuthorizationDenied
	}
	var existing DiscoveryPlanRequest
	var owner string
	err = tx.QueryRow(`SELECT actor,source,scope,period_ms/1000,check_after_admit FROM discovery_plans WHERE plan_id=?`, p.ID).
		Scan(&owner, &existing.Source, &existing.Scope, &existing.PeriodSeconds, &existing.CheckAfterAdmit)
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO discovery_plans(plan_id,actor,source,scope,period_ms,next_at,check_after_admit,check_on_address_drift) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, actor, p.Source, p.Scope, p.PeriodSeconds*1000, p.FirstAt, p.CheckAfterAdmit, p.CheckOnAddressDrift); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) DisableDiscoveryPlan(ctx context.Context, actor, plan string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE discovery_plans SET enabled=0,revision=revision+1 WHERE plan_id=? AND actor=?`, plan, actor)
	if err != nil {
		return err
	}
	affected, err := r.RowsAffected()
	if err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return err
}

type discoveryTick struct {
	plan, actor, source, scope, task string
	at                               int64
	generation, revision             int
}

func (s *Store) RunDiscovery(ctx context.Context, source DiscoveryScanSource) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.runDiscoveryOnce(ctx, source, now()); err != nil && ctx.Err() == nil {
			slog.Error("定时发现推进失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Store) runDiscoveryOnce(ctx context.Context, source DiscoveryScanSource, stamp int64) (bool, error) {
	d, worked, err := s.claimDiscovery(ctx, source, stamp)
	if err != nil || !worked || d.task == "" {
		return worked, err
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	observed, scanErr := source.ScanDiscovery(call, d.source, d.scope)
	return true, s.settleDiscovery(ctx, d, observed, scanErr, now())
}
func (s *Store) claimDiscovery(ctx context.Context, source DiscoveryScanSource, stamp int64) (discoveryTick, bool, error) {
	var d discoveryTick
	if source == nil {
		return d, false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, false, err
	}
	defer tx.Rollback()
	var period int64
	err = tx.QueryRow(`SELECT plan_id,actor,source,scope,next_at,period_ms,revision FROM discovery_plans
 WHERE enabled=1 AND next_at<=? ORDER BY next_at,plan_id LIMIT 1`, stamp).
		Scan(&d.plan, &d.actor, &d.source, &d.scope, &d.at, &period, &d.revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, false, err
	}
	if err == nil {
		next := d.at + ((stamp-d.at)/period+1)*period
		if _, err = tx.Exec("UPDATE discovery_plans SET next_at=? WHERE plan_id=?", next, d.plan); err != nil {
			return d, false, err
		}
		authorized, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp)
		if err != nil {
			return d, false, err
		}
		if stamp-d.at >= period || !authorized || !source.AllowsDiscovery(d.source, d.scope) {
			if _, err = tx.Exec(`INSERT INTO discovery_occurrences(plan_id,scheduled_at,plan_revision,status,summary) VALUES(?,?,?,'skipped',?)`,
				d.plan, d.at, d.revision, "已错过计划时点、授权失效或连接禁用"); err != nil {
				return d, false, err
			}
		} else {
			d.task = id()
			conv := id()
			if _, err = tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", conv, "定时资产发现", stamp); err != nil {
				return d, false, err
			}
			if _, err = tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at) VALUES(?,?,?,?,?,'active','queued','只读发现',?)`,
				d.task, conv, "定时资产发现", "记录发现候选，不自动纳管", d.scope, stamp); err != nil {
				return d, false, err
			}
			if _, err = transitionTaskState(tx, d.task, EventCreate, TaskGuard{Actor: d.actor}, stamp); err != nil {
				return d, false, err
			}
			key := fmt.Sprintf("%s:%d", d.plan, d.at)
			evt, err := receiveInbox(tx, InboxEvent{EventID: key, Source: "discovery-clock", Kind: "discovery_due", Principal: "system",
				TaskID: d.task, PayloadDigest: digest(toJSON([]any{d.plan, d.at, d.revision})), ReceivedAt: stamp})
			if err != nil || evt.Duplicate || evt.Quarantined {
				if err == nil {
					err = ErrConflict
				}
				return d, false, err
			}
			if _, err = tx.Exec(`INSERT INTO discovery_occurrences(plan_id,scheduled_at,plan_revision,task_id,status) VALUES(?,?,?,?,'queued')`,
				d.plan, d.at, d.revision, d.task); err != nil {
				return d, false, err
			}
			if _, err = tx.Exec("UPDATE inbox_events SET state='applied',applied_at=? WHERE source='discovery-clock' AND event_id=?", stamp, key); err != nil {
				return d, false, err
			}
		}
	}
	d = discoveryTick{}
	var current int
	var enabled bool
	err = tx.QueryRow(`SELECT p.plan_id,p.actor,p.source,p.scope,o.task_id,o.scheduled_at,o.generation,o.plan_revision,p.revision,p.enabled
 FROM discovery_occurrences o JOIN discovery_plans p ON p.plan_id=o.plan_id JOIN tasks t ON t.id=o.task_id
 WHERE o.status IN ('queued','running') AND o.next_attempt<=? AND t.status='active' ORDER BY o.scheduled_at,p.plan_id LIMIT 1`, stamp).
		Scan(&d.plan, &d.actor, &d.source, &d.scope, &d.task, &d.at, &d.generation, &d.revision, &current, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, tx.Commit()
	}
	if err != nil {
		return d, false, err
	}
	ok, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp)
	if err != nil {
		return d, false, err
	}
	if !ok || !enabled || d.revision != current || !source.AllowsDiscovery(d.source, d.scope) {
		if err := finishDiscovery(tx, d, false, "发现来源或授权已失效", stamp); err != nil {
			return d, false, err
		}
		d.task = ""
		return d, true, tx.Commit()
	}
	st, err := readTaskState(tx, d.task)
	if err != nil {
		return d, false, err
	}
	if st.State == V3TaskPending {
		if _, err = transitionTaskState(tx, d.task, EventClaim, TaskGuard{Actor: "system", ExpectedStorageRevision: st.StorageRevision}, stamp); err != nil {
			return d, false, err
		}
	} else if st.State != V3TaskRunning {
		return d, false, ErrState
	}
	d.generation++
	if _, err = tx.Exec(`UPDATE discovery_occurrences SET status='running',generation=?,next_attempt=? WHERE plan_id=? AND scheduled_at=?`,
		d.generation, stamp+30000, d.plan, d.at); err != nil {
		return d, false, err
	}
	return d, true, tx.Commit()
}
func (s *Store) settleDiscovery(ctx context.Context, d discoveryTick, observed []DiscoveryObservation, cause error, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gen, current int
	var enabled bool
	var status string
	if err = tx.QueryRow(`SELECT o.generation,p.revision,p.enabled,o.status FROM discovery_occurrences o JOIN discovery_plans p ON p.plan_id=o.plan_id
 WHERE o.plan_id=? AND o.scheduled_at=?`, d.plan, d.at).Scan(&gen, &current, &enabled, &status); err != nil {
		return err
	}
	if gen != d.generation || status != "running" {
		return nil
	}
	ok, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp)
	if err != nil {
		return err
	}
	if cause != nil || !ok || !enabled || current != d.revision || len(observed) > 4096 {
		if err = finishDiscovery(tx, d, false, "发现未完成：来源不可用、结果过大或当前授权失效", stamp); err != nil {
			return err
		}
		return tx.Commit()
	}
	seen := map[string]string{}
	conflicted := map[string]bool{}
	accepted := 0
	for _, o := range observed {
		if o.Serial == "" {
			continue
		}
		if prev, ok := seen[o.Serial]; ok && prev != o.Address {
			conflicted[o.Serial] = true
		}
		seen[o.Serial] = o.Address
	}
	for i, o := range observed {
		address, parseErr := netip.ParseAddr(o.Address)
		if parseErr != nil || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() || len(o.Serial) > 128 || strings.TrimSpace(o.Serial) != o.Serial {
			continue
		}
		ip := address.Unmap().String()
		identity := []string{d.source, "serial", o.Serial}
		state := "observed"
		if o.Serial == "" {
			identity = []string{d.source, d.scope, "address", ip}
			state = "address_only"
		}
		if conflicted[o.Serial] {
			state = "conflict"
		}
		candidate := digest(toJSON(identity))
		var priorAddress, priorState, asset, priorScope string
		var priorSeen int64
		priorErr := tx.QueryRow("SELECT address,state,admitted_ref,scope,seen_at FROM discovery_candidates WHERE candidate_id=?", candidate).
			Scan(&priorAddress, &priorState, &asset, &priorScope, &priorSeen)
		if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
			return priorErr
		}

		if _, err = tx.Exec(`INSERT INTO discovery_candidates(candidate_id,source,scope,serial,address,seen_at,state) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(candidate_id) DO UPDATE SET address=excluded.address,seen_at=excluded.seen_at,
state=CASE WHEN discovery_candidates.state='conflict' OR discovery_candidates.scope<>excluded.scope OR excluded.state='conflict'
THEN 'conflict' WHEN discovery_candidates.admitted_ref<>'' THEN 'admitted' ELSE excluded.state END
WHERE discovery_candidates.seen_at<=excluded.seen_at`, candidate, d.source, d.scope, o.Serial, ip, stamp, state); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO discovery_observations VALUES(?,?,?,?,?,?)`, d.plan, d.at, i, candidate, ip, stamp); err != nil {
			return err
		}
		if priorErr == nil && asset != "" && priorState == "admitted" && state == "observed" && priorScope == d.scope && priorAddress != ip && priorSeen <= stamp {
			if err = evaluateDiscoveryDrift(tx, d, candidate, priorAddress, ip, asset, stamp); err != nil {
				return err
			}
		}

		accepted++
	}
	if err = finishDiscovery(tx, d, true, fmt.Sprintf("已登记 %d 条有来源的观测；未自动纳管", accepted), stamp); err != nil {
		return err
	}
	return tx.Commit()
}
func finishDiscovery(tx *sql.Tx, d discoveryTick, success bool, summary string, stamp int64) error {
	status, legacy, final := "failed", "interrupted", V3TaskFailed
	if success {
		status, legacy, final = "done", "completed", V3TaskSucceeded
	}
	if _, err := tx.Exec("UPDATE discovery_occurrences SET status=?,summary=? WHERE plan_id=? AND scheduled_at=?", status, summary, d.plan, d.at); err != nil {
		return err
	}
	cur, err := readTaskState(tx, d.task)
	if err != nil {
		return err
	}
	transition := func(ev TaskEvent) error {
		_, err := transitionTaskState(tx, d.task, ev, TaskGuard{Actor: "system", ExpectedStorageRevision: cur.StorageRevision, AcceptanceEvidence: true, FinalState: final}, stamp)
		cur.StorageRevision++
		return err
	}
	if success {
		if err = transition(EventStepsComplete); err != nil {
			return err
		}
		if err = transition(EventAcceptancePass); err != nil {
			return err
		}
	} else if err = transition(EventFail); err != nil {
		return err
	}
	if err = transition(EventFinalized); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE tasks SET status=?,status_reason='discovery_result',reason=?,revision=revision+1,completed_at=?,updated_at=? WHERE id=?", legacy, summary, stamp, stamp, d.task)
	return err
}
