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
const benchmarkChatControlPrefix = "chat-execution:"

var ErrBenchmarkCaseNotPending = errors.New("测项已不再等待派发，未跳过；请查看当前执行状态")

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

type BenchmarkExecutionControlInput struct {
	Action               string `json:"action"`
	ExpectedPlanRevision int    `json:"expected_plan_revision"`
	RequestID            string `json:"client_request_id"`
	Reason               string `json:"reason"`
}

type BenchmarkCaseExecutionControl struct {
	Action       string `json:"action"`
	State        string `json:"state"`
	PlanRevision int    `json:"plan_revision"`
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
	if task == "" || step == "" || in.Action != benchmarkSkipThisRun || in.ExpectedPlanRevision < 1 ||
		len(in.RequestID) < 8 || len(in.RequestID) > 128 || strings.TrimSpace(in.RequestID) != in.RequestID ||
		in.Reason == "" || utf8.RuneCountInString(in.Reason) > 500 {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkInput
	}
	fingerprint := digest(toJSON([]any{step, in.Action, in.ExpectedPlanRevision, in.Reason}))

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
	err := tx.QueryRowContext(ctx, `SELECT request_digest,step_id,action,state,plan_revision
FROM benchmark_execution_controls WHERE task_id=? AND actor=? AND request_id=?`, task, actor, in.RequestID).
		Scan(&past, &out.StepID, &out.Action, &out.State, &out.PlanRevision)
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
 OR EXISTS(SELECT 1 FROM benchmark_target_holds h WHERE h.step_id=b.step_id))
FROM benchmark_cases b WHERE b.task_id=? AND b.step_id=?`, task, step).
		Scan(&phase, &operation, &key, &spec, &targetsRaw, &grantsRaw, &hasBoundWork); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if stopping || state != "pending" && state != "running" && state != "paused" || phase != "queued" || operation != "" || hasBoundWork {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkCaseNotPending
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_execution_controls
(task_id,actor,request_id,step_id,request_digest,action,state,plan_revision,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		task, actor, in.RequestID, step, fingerprint, out.Action, out.State, out.PlanRevision, stamp); err != nil {
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
