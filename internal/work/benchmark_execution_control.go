package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const benchmarkSkipThisRun = "skip_this_run"
const benchmarkSkippedThisRun = "skipped_this_run"
const benchmarkHoldBeforeDispatch = "hold_before_dispatch"
const benchmarkResumeBeforeDispatch = "resume_before_dispatch"
const benchmarkDispatchHeld = "held_before_dispatch"
const benchmarkDispatchEligible = "released_before_dispatch"
const benchmarkChatControlPrefix = "chat-execution:"

var ErrBenchmarkCaseNotPending = errors.New("测项已不再等待派发，未跳过；请查看当前执行状态")
var ErrBenchmarkDispatchNotPending = errors.New("测项已不再等待派发，未改变暂缓状态；请查看当前执行状态")

func migrateBenchmarkExecutionControls(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS benchmark_execution_controls (
 task_id TEXT NOT NULL REFERENCES benchmark_runs(task_id) ON DELETE CASCADE,
 actor TEXT NOT NULL, request_id TEXT NOT NULL, step_id TEXT NOT NULL,
 request_digest TEXT NOT NULL, action TEXT NOT NULL, state TEXT NOT NULL,
 plan_revision INTEGER NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(task_id,actor,request_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS benchmark_case_one_execution_skip
ON benchmark_execution_controls(step_id) WHERE action='skip_this_run';`)
	return err
}

// The gate is scheduling state, not an external operation or resource hold.
// The append-only receipts outlive a removed step; the gate itself does not.
func migrateBenchmarkDispatchGates(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "benchmark_execution_controls", "control_version",
		"ALTER TABLE benchmark_execution_controls ADD COLUMN control_version INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS benchmark_dispatch_gates (
 step_id TEXT PRIMARY KEY REFERENCES benchmark_cases(step_id) ON DELETE CASCADE,
 control_version INTEGER NOT NULL CHECK (control_version>0),
 held INTEGER NOT NULL CHECK (held IN (0,1))
);`)
	return err
}

type BenchmarkDispatchControl struct {
	State          string `json:"state"`
	ControlVersion int    `json:"control_version"`
}

type BenchmarkExecutionControlInput struct {
	Action                 string `json:"action"`
	ExpectedPlanRevision   int    `json:"expected_plan_revision"`
	ExpectedControlVersion *int   `json:"expected_control_version,omitempty"`
	RequestID              string `json:"client_request_id"`
	Reason                 string `json:"reason"`
}

type BenchmarkCaseExecutionControl struct {
	Action         string `json:"action"`
	State          string `json:"state"`
	PlanRevision   int    `json:"plan_revision"`
	ControlVersion int    `json:"control_version,omitempty"`
}

type BenchmarkExecutionControlReceipt struct {
	TaskID string `json:"task_id"`
	StepID string `json:"step_id"`
	BenchmarkCaseExecutionControl
}

// ApplyBenchmarkExecutionControl changes dispatch disposition, not the plan
// or acceptance requirements. It never calls or releases an external job.
func (s *Store) ApplyBenchmarkExecutionControl(ctx context.Context, actor, task, step string, in BenchmarkExecutionControlInput) (BenchmarkExecutionControlReceipt, error) {
	if strings.HasPrefix(in.RequestID, benchmarkChatControlPrefix) {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	defer tx.Rollback()
	out, err := applyBenchmarkExecutionControlTx(ctx, tx, actor, task, step, in)
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	return out, tx.Commit()
}

func applyBenchmarkExecutionControlTx(ctx context.Context, tx *sql.Tx, actor, task, step string, in BenchmarkExecutionControlInput) (BenchmarkExecutionControlReceipt, error) {
	if actor == "" {
		return BenchmarkExecutionControlReceipt{}, sql.ErrNoRows
	}
	in.Reason = strings.TrimSpace(in.Reason)
	gateAction := in.Action == benchmarkHoldBeforeDispatch || in.Action == benchmarkResumeBeforeDispatch
	if task == "" || step == "" || (!gateAction && in.Action != benchmarkSkipThisRun) || in.ExpectedPlanRevision < 1 ||
		(gateAction && in.ExpectedControlVersion == nil) || (in.ExpectedControlVersion != nil && *in.ExpectedControlVersion < 0) ||
		len(in.RequestID) < 8 || len(in.RequestID) > 128 || strings.TrimSpace(in.RequestID) != in.RequestID ||
		in.Reason == "" || utf8.RuneCountInString(in.Reason) > 500 {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkInput
	}
	fingerprintFields := []any{step, in.Action, in.ExpectedPlanRevision, in.Reason}
	if in.ExpectedControlVersion != nil {
		fingerprintFields = append(fingerprintFields, *in.ExpectedControlVersion)
	}
	fingerprint := digest(toJSON(fingerprintFields))

	var profile, state string
	var active, profileVersion int
	var stopping bool
	if err := tx.QueryRowContext(ctx, `SELECT b.profile_key,b.profile_version,b.plan_revision,(b.stop_requested OR owner.status='cancelled'),t.state
FROM benchmark_runs b JOIN task_states t ON t.task_id=b.task_id JOIN tasks owner ON owner.id=b.task_id
WHERE b.task_id=? AND b.actor=?`, task, actor).Scan(&profile, &profileVersion, &active, &stopping, &state); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}

	out := BenchmarkExecutionControlReceipt{TaskID: task, StepID: step,
		BenchmarkCaseExecutionControl: BenchmarkCaseExecutionControl{Action: in.Action, State: benchmarkSkippedThisRun, PlanRevision: active}}
	var past string
	err := tx.QueryRowContext(ctx, `SELECT request_digest,step_id,action,state,plan_revision,control_version
FROM benchmark_execution_controls WHERE task_id=? AND actor=? AND request_id=?`, task, actor, in.RequestID).
		Scan(&past, &out.StepID, &out.Action, &out.State, &out.PlanRevision, &out.ControlVersion)
	if err == nil {
		if past != fingerprint {
			return BenchmarkExecutionControlReceipt{}, ErrConflict
		}
		return out, nil // stable historical receipt; no second control decision
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if active != in.ExpectedPlanRevision {
		return BenchmarkExecutionControlReceipt{}, fmt.Errorf("%w：测试任务当前方案修订为 %d", ErrTaskRevision, active)
	}

	var phase, operation, key, spec, targetsRaw, grantsRaw string
	var hasBoundWork bool
	if err := tx.QueryRowContext(ctx, `SELECT b.phase,b.operation_id,b.case_key,b.case_spec,b.frozen_targets,b.authorization_refs,
(EXISTS(SELECT 1 FROM operations o WHERE o.task_id=b.task_id AND o.step_id=b.step_id)
 OR EXISTS(SELECT 1 FROM benchmark_target_holds h WHERE h.step_id=b.step_id)
 OR EXISTS(SELECT 1 FROM waits w WHERE w.task_id=b.task_id AND w.step_id=b.step_id))
FROM benchmark_cases b WHERE b.task_id=? AND b.step_id=?`, task, step).
		Scan(&phase, &operation, &key, &spec, &targetsRaw, &grantsRaw, &hasBoundWork); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if stopping || state != "pending" && state != "running" && state != "paused" || phase != "queued" || operation != "" || hasBoundWork {
		if gateAction {
			return BenchmarkExecutionControlReceipt{}, ErrBenchmarkDispatchNotPending
		}
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkCaseNotPending
	}
	var held bool
	err = tx.QueryRowContext(ctx, "SELECT control_version,held FROM benchmark_dispatch_gates WHERE step_id=?", step).Scan(&out.ControlVersion, &held)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if (in.ExpectedControlVersion == nil && out.ControlVersion != 0) ||
		(in.ExpectedControlVersion != nil && *in.ExpectedControlVersion != out.ControlVersion) ||
		(gateAction && held == (in.Action == benchmarkHoldBeforeDispatch)) {
		return BenchmarkExecutionControlReceipt{}, fmt.Errorf("%w：暂缓状态已变化，请刷新后重试", ErrConflict)
	}
	var c BenchmarkCase
	if spec != "" {
		if err := json.Unmarshal([]byte(spec), &c); err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
	} else {
		p, err := readBenchmarkProfile(tx, profile, profileVersion)
		if err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		for _, def := range p.Cases {
			if def.Key == key {
				c = def
				break
			}
		}
	}
	if c.Key == "" || c.Key != key {
		return BenchmarkExecutionControlReceipt{}, ErrState
	}
	stamp := now()
	var targets []ticketTarget
	var previous map[string]string
	if err := json.Unmarshal([]byte(targetsRaw), &targets); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if err := json.Unmarshal([]byte(grantsRaw), &previous); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if len(targets) == 0 || len(targets) != len(c.Targets) {
		return BenchmarkExecutionControlReceipt{}, ErrState
	}
	for _, target := range targets {
		a, err := readAsset(tx, target.Ref)
		if err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		if a.Revision != target.Revision || a.OwnershipEpoch != target.Epoch {
			return BenchmarkExecutionControlReceipt{}, ErrAuthorizationDenied
		}
		grant, err := benchmarkAuthorization(tx, actor, target.Ref, c, stamp)
		if err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		if grant != previous[target.Ref] {
			return BenchmarkExecutionControlReceipt{}, ErrAuthorizationDenied
		}
	}
	if gateAction {
		hold := in.Action == benchmarkHoldBeforeDispatch
		out.ControlVersion++
		out.State = benchmarkDispatchEligible
		if hold {
			out.State = benchmarkDispatchHeld
		}
		// The transaction shares the same write boundary as the claim, so a
		// gate cannot be reported installed after dispatch registration.
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_dispatch_gates(step_id,control_version,held)
VALUES(?,?,?) ON CONFLICT(step_id) DO UPDATE SET control_version=excluded.control_version,held=excluded.held`, step, out.ControlVersion, hold); err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		if err := appendBenchmarkExecutionControl(ctx, tx, actor, in.RequestID, fingerprint, out, stamp); err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		summary := "恢复为待派发，仍须满足依赖和执行准入"
		if hold {
			summary = "暂缓尚未派发的测项，验收要求未修改"
		}
		if err := recordAudit(tx, AuditEvent{TaskID: task, StepID: step, Actor: actor, Event: "benchmark_case_" + out.State,
			Summary: summary, OccurredAt: stamp}); err != nil {
			return BenchmarkExecutionControlReceipt{}, err
		}
		return out, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE benchmark_cases SET phase='skipped',report=?
WHERE step_id=? AND task_id=? AND phase='queued' AND operation_id=''`, toJSON(map[string]string{"reason": in.Reason}), step, task)
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	} else if n != 1 {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkCaseNotPending
	}
	if _, err := tx.ExecContext(ctx, "UPDATE steps SET state=?,updated_at=? WHERE step_id=? AND task_id=?", V3StepCancelled, stamp, step, task); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if err := appendBenchmarkExecutionControl(ctx, tx, actor, in.RequestID, fingerprint, out, stamp); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if err := recordAudit(tx, AuditEvent{TaskID: task, StepID: step, Actor: actor, Event: "benchmark_case_skipped_this_run",
		Summary: "明确跳过本次执行，验收要求未修改", OccurredAt: stamp}); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if err := finalizeBenchmarkIfSettled(tx, task, stamp); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	return out, nil
}

func appendBenchmarkExecutionControl(ctx context.Context, tx *sql.Tx, actor, requestID, fingerprint string, out BenchmarkExecutionControlReceipt, stamp int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO benchmark_execution_controls
(task_id,actor,request_id,step_id,request_digest,action,state,plan_revision,created_at,control_version) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		out.TaskID, actor, requestID, out.StepID, fingerprint, out.Action, out.State, out.PlanRevision, stamp, out.ControlVersion)
	return err
}
