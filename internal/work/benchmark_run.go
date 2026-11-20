package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

const v31BenchmarkRunSchema = `
CREATE TABLE IF NOT EXISTS benchmark_provider_versions (
 connection_key TEXT NOT NULL, revision INTEGER NOT NULL, fingerprint TEXT NOT NULL,
 PRIMARY KEY(connection_key,revision)
);
CREATE TABLE IF NOT EXISTS benchmark_runs (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
 actor TEXT NOT NULL, request_key TEXT NOT NULL, profile_key TEXT NOT NULL, profile_version INTEGER NOT NULL,
 fingerprint TEXT NOT NULL, created_at INTEGER NOT NULL, stop_requested INTEGER NOT NULL DEFAULT 0, UNIQUE(actor,request_key)
);
CREATE TABLE IF NOT EXISTS benchmark_cases (
 step_id TEXT PRIMARY KEY REFERENCES steps(step_id) ON DELETE CASCADE,
 task_id TEXT NOT NULL REFERENCES benchmark_runs(task_id) ON DELETE CASCADE,
 case_key TEXT NOT NULL, frozen_targets TEXT NOT NULL, authorization_refs TEXT NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN ('queued','submitting','following','verified','incomplete','skipped')),
 operation_id TEXT NOT NULL DEFAULT '', job_id TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0,
 report TEXT NOT NULL DEFAULT '', cancel_sent INTEGER NOT NULL DEFAULT 0, UNIQUE(task_id,case_key)
);
CREATE INDEX IF NOT EXISTS benchmark_cases_due ON benchmark_cases(phase,next_at,task_id);
CREATE TABLE IF NOT EXISTS benchmark_job_identities (
 connection_key TEXT NOT NULL, binding_revision INTEGER NOT NULL, provider_job_id TEXT NOT NULL,
 operation_id TEXT NOT NULL REFERENCES operations(operation_id),
 PRIMARY KEY(connection_key,binding_revision,provider_job_id)
);
CREATE TABLE IF NOT EXISTS benchmark_evidence (
 artifact_id TEXT PRIMARY KEY, step_id TEXT NOT NULL REFERENCES benchmark_cases(step_id) ON DELETE CASCADE,
 target_ref TEXT NOT NULL, provider_ref TEXT NOT NULL, sha256 TEXT NOT NULL, body BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS benchmark_target_holds (
 target_ref TEXT PRIMARY KEY, step_id TEXT NOT NULL REFERENCES benchmark_cases(step_id) ON DELETE RESTRICT
);
`

type BenchmarkProvider interface {
	// Supports is a local, side-effect-free check of the pinned binding.
	Supports(BenchmarkBinding) bool
	Start(context.Context, BenchmarkJobRequest) (string, error)
	Lookup(context.Context, BenchmarkJobRequest) (string, error)
	Get(context.Context, BenchmarkJobRequest, string) (BenchmarkJobResult, error)
}

type benchmarkCanceller interface {
	Cancel(context.Context, BenchmarkJobRequest, string) error
}

type BenchmarkJobRequest struct {
	Binding        BenchmarkBinding  `json:"binding"`
	IdempotencyKey string            `json:"idempotency_key"`
	Case           string            `json:"case"`
	Targets        []string          `json:"targets"`
	Parameters     map[string]string `json:"parameters,omitempty"`
}

type BenchmarkJobResult struct {
	JobID        string                 `json:"job_id"`
	State        string                 `json:"state"` // running/completed/failed/cancelled
	Measurements []BenchmarkMeasurement `json:"measurements,omitempty"`
}
type BenchmarkMeasurement struct {
	Target       string                             `json:"target"`
	Outcome      string                             `json:"outcome"`
	ActualConfig map[string]string                  `json:"actual_config,omitempty"`
	Metrics      map[string]BenchmarkMeasuredMetric `json:"metrics,omitempty"`
	Raw          *BenchmarkRawArtifact              `json:"raw,omitempty"`
}
type BenchmarkMeasuredMetric struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}
type BenchmarkRawArtifact struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
	Bytes  []byte `json:"bytes"`
}

type BenchmarkRunRequest struct {
	RequestKey string `json:"request_key"`
	Profile    string `json:"profile"`
	Version    int    `json:"version"`
}
type BenchmarkRunProgress struct {
	TaskID        string                  `json:"task_id"`
	Profile       string                  `json:"profile"`
	Version       int                     `json:"version"`
	PlanRevision  int                     `json:"plan_revision"`
	State         string                  `json:"state"`
	StopRequested bool                    `json:"stop_requested"`
	AssetScope    BenchmarkAssetScope     `json:"asset_scope"`
	Cases         []BenchmarkCaseProgress `json:"cases"`
}
type BenchmarkCaseProgress struct {
	Key              string                         `json:"key"`
	Phase            string                         `json:"phase"`
	JobID            string                         `json:"job_id,omitempty"`
	Report           json.RawMessage                `json:"report,omitempty"`
	StepID           string                         `json:"step_id"`
	StepState        string                         `json:"step_state"`
	Operation        *BenchmarkOperationProgress    `json:"operation,omitempty"`
	Wait             *BenchmarkWaitProgress         `json:"wait,omitempty"`
	TargetHolds      int                            `json:"target_holds"`
	Targets          []BenchmarkCaseTarget          `json:"targets"`
	ExecutionControl *BenchmarkCaseExecutionControl `json:"execution_control,omitempty"`
	DispatchControl  *BenchmarkDispatchControl      `json:"dispatch_control,omitempty"`
}

// BenchmarkAssetScope counts unique frozen target references, not GPU/CU,
// nodes inside a group, or capacity presently available on the cluster.
type BenchmarkAssetScope struct {
	Source      string   `json:"source"`
	TargetCount int      `json:"target_count"`
	TargetRefs  []string `json:"target_refs"`
}

type BenchmarkCaseTarget struct {
	Ref            string `json:"ref"`
	FrozenRevision int    `json:"frozen_revision"`
	OwnershipEpoch int    `json:"ownership_epoch"`
	LocalHold      string `json:"local_hold"`
}

type BenchmarkOperationProgress struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type BenchmarkWaitProgress struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	State   string `json:"state"`
	// RecheckAt is a control-plane recheck schedule, not a remote job deadline.
	RecheckAt int64 `json:"recheck_at,omitempty"`
}

func benchmarkAsset(tx *sql.Tx, ref string) (OpsAsset, error) {
	a, err := readAsset(tx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w：找不到已纳管测试目标 %s", ErrBenchmarkInput, ref)
	}
	if err != nil {
		return a, err
	}
	switch a.State {
	case AssetStateRecheck, AssetStatePendingDebug, AssetStateDebugging, AssetStatePendingDeliver, AssetStateMaintenance:
	default:
		return a, fmt.Errorf("%w：资产 %s 当前状态不允许性能测试", ErrState, ref)
	}
	if a.FrozenAt != 0 {
		return a, fmt.Errorf("%w：资产 %s 已冻结", ErrState, ref)
	}
	return a, nil
}

func (s *Store) SubmitBenchmarkRun(ctx context.Context, actor string, request BenchmarkRunRequest, provider BenchmarkProvider) (string, error) {
	if strings.HasPrefix(request.RequestKey, benchmarkAdmissionConfirmPrefix) {
		return "", ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	task, err := submitBenchmarkRunTx(tx, actor, request, provider, now())
	if err != nil {
		return "", err
	}
	return task, tx.Commit()
}

func submitBenchmarkRunTx(tx *sql.Tx, actor string, request BenchmarkRunRequest, provider BenchmarkProvider, stamp int64) (string, error) {
	if actor == "" {
		return "", ErrAuthorizationDenied
	}
	if len(request.RequestKey) == 0 || len(request.RequestKey) > 160 || !validBenchKey(request.Profile) || request.Version < 1 {
		return "", ErrBenchmarkInput
	}
	finger := digest(toJSON([]any{request.Profile, request.Version}))

	var old, key string
	err := tx.QueryRow(`SELECT task_id,fingerprint FROM benchmark_runs WHERE actor=? AND request_key=?`, actor, request.RequestKey).Scan(&old, &key)
	if err == nil {
		if key != finger {
			return "", ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	p, err := readBenchmarkProfile(tx, request.Profile, request.Version)
	if err != nil {
		return "", err
	}
	taskID, conversation := id(), id()
	if _, err = tx.Exec(`INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)`, conversation, "基准 "+p.Key, stamp); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,'基准测试','active','queued','方案已受理',?)`, taskID, conversation, "基准 "+p.Key, "按发布版本执行测试并核验结果", stamp); err != nil {
		return "", err
	}
	if _, err = transitionTaskState(tx, taskID, EventCreate, TaskGuard{Actor: actor}, stamp); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO benchmark_runs(task_id,actor,request_key,profile_key,profile_version,fingerprint,created_at) VALUES(?,?,?,?,?,?,?)`, taskID, actor, request.RequestKey, p.Key, p.Version, finger, stamp); err != nil {
		return "", err
	}
	ids := map[string]string{}
	for i, c := range p.Cases {
		targets, auths, err := freezeBenchmarkCase(tx, actor, c, provider, stamp)
		if err != nil {
			return "", err
		}
		step := id()
		ids[c.Key] = step
		if err := insertStep(tx, Step{StepID: step, TaskID: taskID, ContractRevision: 1, DefinitionKey: c.Key,
			DefinitionVersion: p.Version, State: V3StepWaiting, Position: i + 1, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO benchmark_cases(step_id,task_id,case_key,frozen_targets,authorization_refs,phase)
VALUES(?,?,?,?,?,'queued')`, step, taskID, c.Key, toJSON(targets), toJSON(auths)); err != nil {
			return "", err
		}
	}
	for _, c := range p.Cases {
		for _, dep := range c.DependsOn {
			if err := insertStepDependency(tx, ids[c.Key], ids[dep], 0); err != nil {
				return "", err
			}
		}
	}
	return taskID, nil
}

func freezeBenchmarkCase(tx *sql.Tx, actor string, c BenchmarkCase, provider BenchmarkProvider, stamp int64) ([]ticketTarget, map[string]string, error) {
	if provider == nil || !provider.Supports(c.Binding) {
		return nil, nil, fmt.Errorf("%w：测项 %s 没有可用连接及工具", ErrBenchmarkBinding, c.Key)
	}
	targets, auths := []ticketTarget{}, map[string]string{}
	for _, ref := range c.Targets {
		a, err := benchmarkAsset(tx, ref)
		if err != nil {
			return nil, nil, err
		}
		aid, err := benchmarkAuthorization(tx, actor, ref, c, stamp)
		if err != nil {
			return nil, nil, err
		}
		targets = append(targets, ticketTarget{Ref: ref, Revision: a.Revision, Epoch: a.OwnershipEpoch})
		auths[ref] = aid
	}
	return targets, auths, nil
}

func (s *Store) BenchmarkProgress(ctx context.Context, actor, taskID string) (BenchmarkRunProgress, error) {
	var p BenchmarkRunProgress
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	p.TaskID = taskID
	p.AssetScope = BenchmarkAssetScope{Source: "frozen_targets", TargetRefs: []string{}}
	seenTargets := map[string]bool{}
	err = tx.QueryRowContext(ctx, `SELECT r.profile_key,r.profile_version,t.state,r.plan_revision,r.stop_requested FROM benchmark_runs r JOIN task_states t ON t.task_id=r.task_id
WHERE r.task_id=? AND r.actor=?`, taskID, actor).Scan(&p.Profile, &p.Version, &p.State, &p.PlanRevision, &p.StopRequested)
	if err != nil {
		return p, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.case_key,b.phase,b.job_id,b.report,s.step_id,s.state,
o.operation_id,o.state,w.wait_id,w.wait_version,w.state,w.deadline_at,
ec.action,ec.state,ec.plan_revision,g.control_version,g.held,
(SELECT count(*) FROM benchmark_target_holds h WHERE h.step_id=b.step_id),
(SELECT json_group_array(json_object(
 'ref',v.value->>'ref','frozen_revision',v.value->>'revision','ownership_epoch',v.value->>'epoch',
 'local_hold',CASE
   WHEN EXISTS(SELECT 1 FROM benchmark_target_holds h WHERE h.target_ref=v.value->>'ref' AND h.step_id=b.step_id) THEN 'held_by_case'
   WHEN EXISTS(SELECT 1 FROM benchmark_target_holds h WHERE h.target_ref=v.value->>'ref')
     OR EXISTS(SELECT 1 FROM quota_reservations q WHERE q.target_ref=v.value->>'ref' AND q.scope='target_hold'
       AND q.exclusive=1 AND q.state IN ('reserved','settled')) THEN 'held_elsewhere'
   ELSE 'not_locally_held' END) ORDER BY v.key)
 FROM json_each(b.frozen_targets) v)
FROM benchmark_cases b JOIN steps s ON s.step_id=b.step_id AND s.task_id=b.task_id
LEFT JOIN operations o ON o.operation_id=b.operation_id AND o.task_id=b.task_id AND o.step_id=b.step_id
LEFT JOIN waits w ON w.wait_id='benchmark:'||b.step_id AND w.task_id=b.task_id AND w.step_id=b.step_id
 AND w.kind='external' AND w.match_key=o.operation_id
LEFT JOIN benchmark_execution_controls ec ON ec.step_id=b.step_id AND ec.task_id=b.task_id AND ec.action='skip_this_run'
LEFT JOIN benchmark_dispatch_gates g ON g.step_id=b.step_id AND b.phase='queued'
WHERE b.task_id=? ORDER BY s.position,b.step_id`, taskID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var c BenchmarkCaseProgress
		var raw, targetsJSON string
		var opID, opState, waitID, waitState sql.NullString
		var waitRev, waitDeadline, controlRevision, gateVersion sql.NullInt64
		var gateHeld sql.NullBool
		var controlAction, controlState sql.NullString
		if err := rows.Scan(&c.Key, &c.Phase, &c.JobID, &raw, &c.StepID, &c.StepState,
			&opID, &opState, &waitID, &waitRev, &waitState, &waitDeadline, &controlAction, &controlState, &controlRevision, &gateVersion, &gateHeld, &c.TargetHolds, &targetsJSON); err != nil {
			return p, err
		}
		if raw != "" {
			c.Report = json.RawMessage(raw)
		}
		if opID.Valid {
			c.Operation = &BenchmarkOperationProgress{ID: opID.String, State: opState.String}
		}
		if controlAction.Valid {
			c.ExecutionControl = &BenchmarkCaseExecutionControl{Action: controlAction.String, State: controlState.String, PlanRevision: int(controlRevision.Int64)}
		}
		if gateVersion.Valid && !p.StopRequested && (p.State == "pending" || p.State == "running" || p.State == "paused") {
			c.DispatchControl = &BenchmarkDispatchControl{State: benchmarkDispatchEligible, ControlVersion: int(gateVersion.Int64)}
			if gateHeld.Bool {
				c.DispatchControl.State = benchmarkDispatchHeld
			}
		}
		if waitID.Valid {
			c.Wait = &BenchmarkWaitProgress{ID: waitID.String, Version: int(waitRev.Int64), State: waitState.String, RecheckAt: waitDeadline.Int64}
		}
		if err := json.Unmarshal([]byte(targetsJSON), &c.Targets); err != nil {
			return p, err
		}
		for _, target := range c.Targets {
			if target.Ref == "" || target.FrozenRevision < 1 {
				return p, ErrState
			}
			if !seenTargets[target.Ref] {
				seenTargets[target.Ref] = true
				p.AssetScope.TargetRefs = append(p.AssetScope.TargetRefs, target.Ref)
			}
		}
		p.Cases = append(p.Cases, c)
	}
	slices.Sort(p.AssetScope.TargetRefs)
	p.AssetScope.TargetCount = len(p.AssetScope.TargetRefs)
	return p, rows.Err()
}

func (s *Store) RunBenchmarks(ctx context.Context, provider BenchmarkProvider) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.runBenchmarkOnce(ctx, provider); err != nil && ctx.Err() == nil {
			slog.Error("基准推进失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func controlBenchmark(tx *sql.Tx, taskID, action string) (bool, error) {
	var requested bool
	err := tx.QueryRow("SELECT stop_requested FROM benchmark_runs WHERE task_id=?", taskID).Scan(&requested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if action != "stop" {
		return true, fmt.Errorf("%w：基准作业目前只支持停止并对账", ErrState)
	}
	if requested {
		return true, nil
	}
	cur, err := readTaskState(tx, taskID)
	if err != nil {
		return true, err
	}
	if cur.State.IsTerminal() {
		return true, ErrState
	}
	stamp := now()
	if _, err = transitionTaskState(tx, taskID, EventCancel, TaskGuard{Actor: "user:session", ExpectedStorageRevision: cur.StorageRevision}, stamp); err != nil {
		return true, err
	}
	if _, err = tx.Exec("UPDATE benchmark_runs SET stop_requested=1 WHERE task_id=?", taskID); err != nil {
		return true, err
	}
	if _, err = tx.Exec("UPDATE tasks SET status_reason='stop_requested',reason='正在停止，仍需核对远端作业',revision=revision+1,updated_at=? WHERE id=?", stamp, taskID); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Store) ReadBenchmarkEvidence(ctx context.Context, actor, taskID, artifactID string) ([]byte, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT e.body FROM benchmark_evidence e JOIN benchmark_cases c ON c.step_id=e.step_id
JOIN benchmark_runs r ON r.task_id=c.task_id WHERE e.artifact_id=? AND r.task_id=? AND r.actor=?`, artifactID, taskID, actor).Scan(&body)
	return body, err
}
