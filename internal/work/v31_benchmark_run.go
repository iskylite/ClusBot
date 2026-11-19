package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	TaskID  string                  `json:"task_id"`
	Profile string                  `json:"profile"`
	Version int                     `json:"version"`
	State   string                  `json:"state"`
	Cases   []BenchmarkCaseProgress `json:"cases"`
}
type BenchmarkCaseProgress struct {
	Key    string          `json:"key"`
	Phase  string          `json:"phase"`
	JobID  string          `json:"job_id,omitempty"`
	Report json.RawMessage `json:"report,omitempty"`
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
	if actor == "" {
		return "", ErrAuthorizationDenied
	}
	if len(request.RequestKey) == 0 || len(request.RequestKey) > 160 || !validBenchKey(request.Profile) || request.Version < 1 {
		return "", ErrBenchmarkInput
	}
	finger := digest(toJSON([]any{request.Profile, request.Version}))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var old, key string
	err = tx.QueryRow(`SELECT task_id,fingerprint FROM benchmark_runs WHERE actor=? AND request_key=?`, actor, request.RequestKey).Scan(&old, &key)
	if err == nil {
		if key != finger {
			return "", ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	p, err := readBenchmarkProfile(tx, request.Profile, request.Version)
	if err != nil {
		return "", err
	}
	stamp := now()
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
		if provider == nil || !provider.Supports(c.Binding) {
			return "", fmt.Errorf("%w：测项 %s 没有可用连接及工具", ErrBenchmarkBinding, c.Key)
		}
		targets, auths := []ticketTarget{}, map[string]string{}
		for _, ref := range c.Targets {
			a, err := benchmarkAsset(tx, ref)
			if err != nil {
				return "", err
			}
			aid, err := benchmarkAuthorization(tx, actor, ref, c, stamp)
			if err != nil {
				return "", err
			}
			targets = append(targets, ticketTarget{Ref: ref, Revision: a.Revision, Epoch: a.OwnershipEpoch})
			auths[ref] = aid
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
	return taskID, tx.Commit()
}

func (s *Store) BenchmarkProgress(ctx context.Context, actor, taskID string) (BenchmarkRunProgress, error) {
	var p BenchmarkRunProgress
	p.TaskID = taskID
	err := s.db.QueryRowContext(ctx, `SELECT r.profile_key,r.profile_version,t.state FROM benchmark_runs r JOIN task_states t ON t.task_id=r.task_id
WHERE r.task_id=? AND r.actor=?`, taskID, actor).Scan(&p.Profile, &p.Version, &p.State)
	if err != nil {
		return p, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.case_key,b.phase,b.job_id,b.report FROM benchmark_cases b JOIN steps s ON s.step_id=b.step_id
WHERE b.task_id=? ORDER BY s.position`, taskID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var c BenchmarkCaseProgress
		var raw string
		if err := rows.Scan(&c.Key, &c.Phase, &c.JobID, &raw); err != nil {
			return p, err
		}
		if raw != "" {
			c.Report = json.RawMessage(raw)
		}
		p.Cases = append(p.Cases, c)
	}
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
