package work

import (
	"context"
	"path/filepath"
	"testing"
)

const testWorkerPlan = `[{"key":"source","goal":"整理材料","acceptance_criteria":"列出来源"},{"key":"verify","goal":"核对条目","acceptance_criteria":"列出差异","depends_on":["source"]}]`

func seedPlanningRun(t *testing.T, s *Store) Run {
	t.Helper()
	ctx := t.Context()
	if _, err := s.Submit(ctx, "worker-plan-task", "整理并核对条目", "", "", true); err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO run_read_checkpoints
(run_id,version,provider_binding,context_digest,output_budget,active_budget_ms,output_used,request_limit)
VALUES(?,?,'fixture','fixture',1200,12000,300,300)`, run.ID, readCheckpointVersion); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestWorkerPlanReceiptRecoversAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	run := seedPlanningRun(t, s)
	if _, err = s.db.Exec(`CREATE TRIGGER reject_worker_plan BEFORE INSERT ON workflow_children
BEGIN SELECT RAISE(ABORT,'workflow write unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	output := int64(150)
	s.finalizeRun(run, "completed", "", runAccounting{plan: testWorkerPlan, activeMS: 3000, tokens: &TokenUsage{OutputTokens: &output}})
	var retained string
	if err = s.db.QueryRow("SELECT plan FROM run_finalizations WHERE run_id=?", run.ID).Scan(&retained); err != nil || retained != testWorkerPlan {
		t.Fatalf("missing durable plan: %q, %v", retained, err)
	}
	var plans, children int
	if err = s.db.QueryRow("SELECT (SELECT count(*) FROM worker_plans),(SELECT count(*) FROM workflow_children)").Scan(&plans, &children); err != nil || plans != 0 || children != 0 {
		t.Fatalf("failed transaction leaked plan: %d, %d, %v", plans, children, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_worker_plan"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	workflow, err := s.Workflow(ctx, run.TaskID)
	if err != nil || len(workflow.Children) != 2 {
		t.Fatalf("recovered workflow: %+v, %v", workflow, err)
	}
	var count, outputBudget, activeBudget, receipts int
	if err = s.db.QueryRow(`SELECT count(*),sum(output_tokens),sum(active_ms),(SELECT count(*) FROM run_finalizations)
FROM worker_plan_budgets`).Scan(&count, &outputBudget, &activeBudget, &receipts); err != nil ||
		count != 3 || outputBudget != 1050 || activeBudget != 9000 || receipts != 0 {
		t.Fatalf("budgets or receipts: %d %d %d %d, %v", count, outputBudget, activeBudget, receipts, err)
	}
	detail, err := s.Detail(run.TaskID, true)
	if err != nil || detail.Task.StatusReason != "waiting_dependencies" || detail.Artifact != nil ||
		detail.Run.Status != "queued" || detail.Run.ID == run.ID {
		t.Fatalf("parent completed before aggregation: %+v, %v", detail, err)
	}
	ready, err := s.claim(ctx)
	if err != nil || ready.TaskID != workflow.Children[0].TaskID {
		t.Fatalf("prerequisite order lost: %+v, %v", ready, err)
	}
}

func TestWorkerPlanChecksControlAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(context.Context, *Store, Run) error
		active int64
		plan   string
		code   string
	}{
		{name: "stopped", action: func(ctx context.Context, s *Store, run Run) error {
			d, err := s.Detail(run.TaskID, true)
			if err != nil {
				return err
			}
			return s.Control(ctx, run.TaskID, "stop", d.Task.Revision)
		}, plan: testWorkerPlan, code: "stop"},
		{name: "insufficient budget", active: 11000, plan: testWorkerPlan, code: "worker_plan_rejected"},
		{name: "cyclic plan", plan: `[{"key":"a","goal":"A","acceptance_criteria":"A","depends_on":["b"]},{"key":"b","goal":"B","acceptance_criteria":"B","depends_on":["a"]}]`, code: "worker_plan_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "work.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			run := seedPlanningRun(t, s)
			if tc.action != nil {
				if err = tc.action(t.Context(), s, run); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.finish(run, "completed", "", runAccounting{plan: tc.plan, activeMS: tc.active}); err != nil {
				t.Fatal(err)
			}
			d, err := s.Detail(run.TaskID, true)
			if err != nil || d.Run.ErrorCode != tc.code || d.Artifact != nil {
				t.Fatalf("unexpected decision: %+v, %v", d, err)
			}
			var children int
			if err = s.db.QueryRow("SELECT count(*) FROM workflow_children").Scan(&children); err != nil || children != 0 {
				t.Fatalf("rejected plan created children: %d, %v", children, err)
			}
		})
	}
}
