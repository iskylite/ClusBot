package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const MaxWorkerPlanSteps = 4

var ErrWorkerPlanBudget = errors.New("本轮剩余预算不足以分配给子任务和汇总")

type workerPlanningKey struct{}

func WorkerCanPlan(ctx context.Context) bool {
	enabled, _ := ctx.Value(workerPlanningKey{}).(bool)
	return enabled
}

const workerPlanEligible = `SELECT EXISTS(
SELECT 1 FROM runs r JOIN tasks t ON t.id=r.task_id
JOIN run_bindings b ON b.run_id=r.id AND b.generation=r.execution_generation
WHERE r.id=? AND r.execution_generation=0 AND r.status='running' AND r.control=''
AND t.revision=b.task_revision
AND b.assignment_generation=(SELECT max(generation) FROM assignments WHERE task_id=t.id)
AND NOT EXISTS(SELECT 1 FROM runs earlier WHERE earlier.task_id=t.id AND earlier.rowid<r.rowid)
AND NOT EXISTS(SELECT 1 FROM workflow_children c WHERE c.task_id=t.id OR c.parent_id=t.id)
AND NOT EXISTS(SELECT 1 FROM run_reads d WHERE d.run_id=r.id)
)`

func (s *Store) workerPlanContext(ctx context.Context, run Run, limits RunLimits) (context.Context, error) {
	if run.TaskID == "" || run.Generation != 0 || limits.OutputTokens < 512 || limits.ActiveTime < 3*time.Second {
		return ctx, nil
	}
	var eligible bool
	err := s.db.QueryRowContext(ctx, workerPlanEligible, run.ID).Scan(&eligible)
	if err == nil && eligible {
		ctx = context.WithValue(ctx, workerPlanningKey{}, true)
	}
	return ctx, err
}

type workerPlan struct {
	request WorkflowRequest
	output  int
	active  int64
}

func prepareWorkerPlan(tx *sql.Tx, run Run, proposal string, activeMS int64) (workerPlan, error) {
	var eligible bool
	if err := tx.QueryRow(workerPlanEligible, run.ID).Scan(&eligible); err != nil {
		return workerPlan{}, err
	}
	if !eligible {
		return workerPlan{}, ErrWorkflow
	}
	var steps []WorkflowStep
	if err := json.Unmarshal([]byte(proposal), &steps); err != nil || len(steps) < 2 || len(steps) > MaxWorkerPlanSteps {
		return workerPlan{}, ErrWorkflow
	}
	var plan workerPlan
	if err := tx.QueryRow("SELECT goal,scope_label,acceptance_criteria FROM tasks WHERE id=?", run.TaskID).
		Scan(&plan.request.Goal, &plan.request.Scope, &plan.request.Criteria); err != nil {
		return plan, err
	}
	var required bool
	for _, step := range steps {
		required = required || !step.Optional
	}
	if !required {
		return plan, ErrWorkflow
	}
	plan.request.Steps = steps
	normalized, err := normalizeWorkflow(plan.request, false)
	if err != nil {
		return plan, err
	}
	plan.request.Steps = normalized.Steps
	err = tx.QueryRow("SELECT output_budget-output_used,active_budget_ms-max(?,r.active_ms) FROM run_read_checkpoints c JOIN runs r ON r.id=c.run_id WHERE c.run_id=?",
		activeMS, run.ID).Scan(&plan.output, &plan.active)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, ErrWorkerPlanBudget
	}
	if err != nil {
		return plan, err
	}
	slots := len(steps) + 1
	plan.output /= slots
	plan.active /= int64(slots)
	if plan.output < 64 || plan.active < time.Second.Milliseconds() {
		return plan, ErrWorkerPlanBudget
	}
	return plan, nil
}

func commitWorkerPlan(ctx context.Context, tx *sql.Tx, run Run, plan workerPlan) error {
	payload, err := json.Marshal(plan.request.Steps)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO worker_plans VALUES(?,?,?,?)", run.TaskID, run.ID, run.Generation, string(payload)); err != nil {
		return err
	}
	materials, err := activeMaterials(ctx, tx, run.ConversationID)
	if err != nil {
		return err
	}
	children, err := createWorkflowChildren(tx, run, plan.request)
	if err != nil {
		return err
	}
	aggregate := Run{ID: id(), TaskID: run.TaskID, ConversationID: run.ConversationID}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,'queued',?)",
		aggregate.ID, aggregate.ConversationID, aggregate.TaskID, now()); err != nil {
		return err
	}
	for _, child := range children {
		for index, material := range materials {
			if _, err = tx.Exec(`INSERT INTO message_materials(message_seq,material_id,ordinal)
SELECT seq,?,? FROM messages WHERE run_id=? AND role='user'`, material.ID, index+1, child.ID); err != nil {
				return err
			}
		}
	}
	for _, next := range append(children, aggregate) {
		if _, err = tx.Exec("INSERT INTO worker_plan_budgets VALUES(?,?,?)", next.ID, plan.active, plan.output); err != nil {
			return err
		}
	}
	if err = addEvent(tx, run.ID, "workflow.planned", map[string]any{"children": len(children), "aggregate_run_id": aggregate.ID}); err != nil {
		return err
	}
	return addEvent(tx, aggregate.ID, "run.state_changed", map[string]string{"status": "queued"})
}

func (s *Store) applyPlanBudget(ctx context.Context, run Run, limits RunLimits) (RunLimits, error) {
	var ms int64
	var tokens int
	err := s.db.QueryRowContext(ctx, "SELECT active_ms,output_tokens FROM worker_plan_budgets WHERE run_id=?", run.ID).Scan(&ms, &tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return limits, nil
	}
	if err != nil {
		return limits, err
	}
	limits.ActiveTime = min(limits.ActiveTime, time.Duration(ms)*time.Millisecond)
	limits.OutputTokens = min(limits.OutputTokens, tokens)
	return limits, nil
}
