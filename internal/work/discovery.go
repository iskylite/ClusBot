package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"
)

var ErrDiscoveryInput = errors.New("发现配置、来源或候选不受支持")

const discoverySchema = `
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

func migrateDiscoveryStorage(tx *sql.Tx) error { _, err := tx.Exec(discoverySchema); return err }

func migrateDiscoveryTaskLookup(tx *sql.Tx) error {
	_, err := tx.Exec("CREATE INDEX IF NOT EXISTS discovery_occurrences_by_task ON discovery_occurrences(task_id) WHERE task_id IS NOT NULL")
	return err
}

type DiscoveryScanSource interface {
	AllowsDiscovery(id, scope string) bool
	ScanDiscovery(ctx context.Context, id, scope string) ([]DiscoveryObservation, error)
}
type DiscoveryObservation struct {
	Serial         string    `json:"serial,omitempty"`
	Address        string    `json:"address"`
	Kind           string    `json:"kind,omitempty"`
	SSHEndpoint    string    `json:"ssh_endpoint,omitempty"`
	BMCEndpoint    string    `json:"bmc_endpoint,omitempty"`
	SourceIdentity string    `json:"source_identity,omitempty"`
	ProviderTaskID string    `json:"provider_task_id,omitempty"`
	ProviderTarget string    `json:"provider_target,omitempty"`
	Sequence       uint64    `json:"sequence,omitempty"`
	ObservedAt     time.Time `json:"observed_at,omitempty"`
}
type DiscoveryPlanRequest struct {
	ID                  string              `json:"id"`
	Source              string              `json:"source"`
	Scope               string              `json:"scope"`
	PeriodSeconds       int64               `json:"period_seconds"`
	FirstAt             int64               `json:"first_at_unix_ms"`
	CheckAfterAdmit     bool                `json:"check_after_admit"`
	CheckOnAddressDrift bool                `json:"check_on_address_drift"`
	Scan                *DiscoveryScopeSpec `json:"scan,omitempty"`
}
type DiscoveryGrant struct {
	Source              string              `json:"source"`
	Scope               string              `json:"scope"`
	ExpiresAt           int64               `json:"expires_at_unix_ms"`
	AllowAdmissionCheck bool                `json:"allow_admission_check"`
	AllowDriftCheck     bool                `json:"allow_drift_check"`
	Scan                *DiscoveryScopeSpec `json:"scan,omitempty"`
}

func (s *Store) GrantDiscovery(ctx context.Context, actor string, g DiscoveryGrant, source DiscoveryScanSource) (string, error) {
	if actor == "" || g.ExpiresAt <= now() {
		return "", ErrDiscoveryInput
	}
	binding, err := pinDiscoverySource(ctx, source, g.Source, g.Scope, g.Scan)
	if err != nil {
		return "", err
	}
	params := struct {
		DiscoveryGrant
		Fingerprint string `json:"binding_fingerprint,omitempty"`
	}{g, binding.fingerprint()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	a, err := grantAuthorization(tx, Authorization{Subject: actor, Action: "discover", ObjectRef: g.Source, ScopeLabel: g.Scope, ExpiresAt: g.ExpiresAt,
		Owner: actor, Source: "explicit-discovery-settings", ParamConstraints: toJSON(params)}, now())
	if err != nil {
		return "", err
	}
	return a.AuthID, tx.Commit()
}
func discoveryGranted(tx *sql.Tx, actor, source, scope string, check bool, stamp int64, fingerprints ...string) (bool, error) {
	var ok bool
	var fingerprint any
	if len(fingerprints) != 0 {
		fingerprint = fingerprints[0]
	}
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='discover' AND object_ref=? AND scope_label=?
 AND revoked_at=0 AND valid_from<=? AND expires_at>? AND json_valid(param_constraints)
 AND json_extract(param_constraints,'$.source')=? AND json_extract(param_constraints,'$.scope')=?
 AND (?=0 OR json_extract(param_constraints,'$.allow_admission_check')=1)
 AND (? IS NULL OR coalesce(json_extract(param_constraints,'$.binding_fingerprint'),'')=?))`,
		actor, source, scope, stamp, stamp, source, scope, check, fingerprint, fingerprint).Scan(&ok)
	return ok, err
}
func (s *Store) PublishDiscoveryPlan(ctx context.Context, actor string, p DiscoveryPlanRequest, source DiscoveryScanSource) error {
	stamp := now()
	if actor == "" || !validBenchKey(p.ID) || p.PeriodSeconds < 60 ||
		p.PeriodSeconds > 7*86400 || p.FirstAt < stamp || p.FirstAt > stamp+int64(31*24*time.Hour/time.Millisecond) {
		return ErrDiscoveryInput
	}
	binding, err := pinDiscoverySource(ctx, source, p.Source, p.Scope, p.Scan)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	allowed, err := discoveryGranted(tx, actor, p.Source, p.Scope, false, stamp, binding.fingerprint())
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
	if binding != nil {
		if _, err = tx.Exec("INSERT INTO discovery_plan_bindings VALUES(?,?)", p.ID, toJSON(binding)); err != nil {
			return err
		}
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
	binding                          *discoverySourceBinding
	coverage                         *discoveryScanCoverage
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
	wait := 10 * time.Second
	if d.binding != nil {
		wait = 2 * time.Minute
	}
	call, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var observed []DiscoveryObservation
	var scanErr error
	if adapter, ok := source.(scopedDiscoverySource); ok {
		observed, d.coverage, scanErr = adapter.scanPinnedDiscovery(call, d.source, d.scope, d.binding, fmt.Sprintf("%s:%d:%s", d.plan, d.at, d.task))
	} else if d.binding != nil {
		scanErr = ErrDiscoveryInput
	} else {
		observed, scanErr = source.ScanDiscovery(call, d.source, d.scope)
	}
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
	var rawBinding string
	err = tx.QueryRow(`SELECT p.plan_id,p.actor,p.source,p.scope,p.next_at,p.period_ms,p.revision,coalesce(b.binding,'') FROM discovery_plans p
 LEFT JOIN discovery_plan_bindings b ON b.plan_id=p.plan_id
 WHERE p.enabled=1 AND p.next_at<=? ORDER BY p.next_at,p.plan_id LIMIT 1`, stamp).
		Scan(&d.plan, &d.actor, &d.source, &d.scope, &d.at, &period, &d.revision, &rawBinding)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, false, err
	}
	if err == nil {
		if rawBinding != "" && json.Unmarshal([]byte(rawBinding), &d.binding) != nil {
			return d, false, ErrDiscoveryInput
		}
		next := d.at + ((stamp-d.at)/period+1)*period
		if _, err = tx.Exec("UPDATE discovery_plans SET next_at=? WHERE plan_id=?", next, d.plan); err != nil {
			return d, false, err
		}
		authorized, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp, d.binding.fingerprint())
		if err != nil {
			return d, false, err
		}
		if stamp-d.at >= period || !authorized || (d.binding == nil && !source.AllowsDiscovery(d.source, d.scope)) {
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
	err = tx.QueryRow(`SELECT p.plan_id,p.actor,p.source,p.scope,o.task_id,o.scheduled_at,o.generation,o.plan_revision,p.revision,p.enabled,coalesce(b.binding,'')
 FROM discovery_occurrences o JOIN discovery_plans p ON p.plan_id=o.plan_id JOIN tasks t ON t.id=o.task_id
 LEFT JOIN discovery_plan_bindings b ON b.plan_id=p.plan_id
 WHERE o.status IN ('queued','running') AND o.next_attempt<=? AND t.status='active' ORDER BY o.scheduled_at,p.plan_id LIMIT 1`, stamp).
		Scan(&d.plan, &d.actor, &d.source, &d.scope, &d.task, &d.at, &d.generation, &d.revision, &current, &enabled, &rawBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, tx.Commit()
	}
	if err != nil {
		return d, false, err
	}
	if rawBinding != "" && json.Unmarshal([]byte(rawBinding), &d.binding) != nil {
		return d, false, ErrDiscoveryInput
	}
	ok, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp, d.binding.fingerprint())
	if err != nil {
		return d, false, err
	}
	if !ok || !enabled || d.revision != current || (d.binding == nil && !source.AllowsDiscovery(d.source, d.scope)) {
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
	if st.State == RuntimeTaskPending {
		if _, err = transitionTaskState(tx, d.task, EventClaim, TaskGuard{Actor: "system", ExpectedStorageRevision: st.StorageRevision}, stamp); err != nil {
			return d, false, err
		}
	} else if st.State != RuntimeTaskRunning {
		return d, false, ErrState
	}
	d.generation++
	lease := int64(30000)
	if d.binding != nil {
		lease = 150000
	}
	if _, err = tx.Exec(`UPDATE discovery_occurrences SET status='running',generation=?,next_attempt=? WHERE plan_id=? AND scheduled_at=?`,
		d.generation, stamp+lease, d.plan, d.at); err != nil {
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
	ok, err := discoveryGranted(tx, d.actor, d.source, d.scope, false, stamp, d.binding.fingerprint())
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
	// Use the same validated addresses for conflict detection and persistence,
	// preserving the original source ordinal for each accepted audit row.
	addresses := make([]string, len(observed))
	for i, o := range observed {
		address, parseErr := netip.ParseAddr(o.Address)
		if parseErr != nil || address.Zone() != "" || len(o.Serial) > 128 || strings.TrimSpace(o.Serial) != o.Serial {
			continue
		}
		address = address.Unmap()
		if address.IsUnspecified() || address.IsMulticast() {
			continue
		}
		ip := address.String()
		addresses[i] = ip
		if o.Serial == "" {
			continue
		}
		if prev, ok := seen[o.Serial]; ok && prev != ip {
			conflicted[o.Serial] = true
		}
		seen[o.Serial] = ip
	}
	accepted := 0
	for i, o := range observed {
		ip := addresses[i]
		if ip == "" {
			continue
		}
		identity := []string{d.source, "serial", o.Serial}
		if d.binding != nil {
			if o.SourceIdentity != "mcp:"+d.binding.ConnectionID || o.Serial == "" || o.Kind == "" || o.ProviderTaskID == "" || o.Sequence == 0 {
				continue
			}
			// Revision changes require a fresh authorization, but do not create
			// another identity for the same provider and physical device.
			identity = []string{o.SourceIdentity, o.Kind, strings.ToUpper(o.Serial)}
		}
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
		var priorScheduled int64
		// Compare the durable local scan slots, not their receipt time. The HTTP
		// adapter does not provide when the device was actually measured.
		priorErr := tx.QueryRow(`SELECT c.state,c.admitted_ref,c.scope,coalesce(o.address,c.address),coalesce(o.scheduled_at,0)
FROM discovery_candidates c LEFT JOIN discovery_observations o ON o.rowid=(
 SELECT x.rowid FROM discovery_observations x JOIN discovery_plans p ON p.plan_id=x.plan_id
 JOIN discovery_candidates accepted ON accepted.candidate_id=x.candidate_id
 WHERE x.candidate_id=c.candidate_id AND p.source=c.source AND p.scope=c.scope
 ORDER BY x.scheduled_at DESC,(x.address=accepted.address AND x.seen_at=accepted.seen_at) DESC,x.seen_at,x.plan_id,x.ordinal LIMIT 1
) WHERE c.candidate_id=?`, candidate).Scan(&priorState, &asset, &priorScope, &priorAddress, &priorScheduled)
		switch {
		case errors.Is(priorErr, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO discovery_candidates(candidate_id,source,scope,serial,address,seen_at,state) VALUES(?,?,?,?,?,?,?)`,
				candidate, d.source, d.scope, o.Serial, ip, stamp, state)
		case priorErr != nil:
			return priorErr
		case priorScope != d.scope || (d.at == priorScheduled && (priorAddress != ip || state == "conflict")):
			_, err = tx.Exec(`UPDATE discovery_candidates SET state='conflict' WHERE candidate_id=? AND state<>'conflict'`, candidate)
		case d.at > priorScheduled:
			if priorState == "conflict" {
				state = "conflict"
			} else if asset != "" && state == "observed" {
				state = "admitted"
			}
			_, err = tx.Exec(`UPDATE discovery_candidates SET address=?,seen_at=?,state=? WHERE candidate_id=?`, ip, stamp, state, candidate)
			if err == nil && state == "admitted" && priorState == "admitted" && priorScheduled > 0 && priorAddress != ip {
				err = evaluateDiscoveryDrift(tx, d, candidate, priorAddress, ip, asset, stamp)
			}
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO discovery_observations VALUES(?,?,?,?,?,?)`, d.plan, d.at, i, candidate, ip, stamp); err != nil {
			return err
		}

		if d.binding != nil {
			if _, err = tx.Exec("INSERT INTO discovery_observation_evidence VALUES(?,?,?,?,?)", d.plan, d.at, i, toJSON(o), d.binding.fingerprint()); err != nil {
				return err
			}
		}
		accepted++
	}
	summary := fmt.Sprintf("已登记 %d 条有来源的观测；未自动纳管", accepted)
	if d.coverage != nil {
		if _, err = tx.Exec("INSERT INTO discovery_scan_coverages VALUES(?,?,?)", d.plan, d.at, toJSON(d.coverage)); err != nil {
			return err
		}
		if d.coverage.Outcome == "partial" {
			summary = fmt.Sprintf("部分发现：登记 %d 条可信观测，%d 个最终记录；未观测到的资产不能判断不存在", accepted, d.coverage.Total)
		}
	}
	if err = finishDiscovery(tx, d, true, summary, stamp); err != nil {
		return err
	}
	return tx.Commit()
}
func finishDiscovery(tx *sql.Tx, d discoveryTick, success bool, summary string, stamp int64) error {
	status, legacy, final := "failed", "interrupted", RuntimeTaskFailed
	if success {
		status, legacy, final = "done", "completed", RuntimeTaskSucceeded
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
