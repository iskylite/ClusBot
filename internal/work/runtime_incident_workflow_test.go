package work

import (
	"encoding/json"
	"strings"
	"testing"
)

func incidentWorkflowStage(t *testing.T, workflow IncidentWorkflowView, key string) IncidentWorkflowStage {
	t.Helper()
	for _, stage := range workflow.Stages {
		if stage.StepKey == key {
			return stage
		}
	}
	t.Fatalf("workflow missing %q stage", key)
	return IncidentWorkflowStage{}
}

func TestIncidentWorkflowReflectsRecordedEvidence(t *testing.T) {
	store := setupIncidentsStore(t, "workflow-evidence.db")
	ctx := t.Context()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO incidents(incident_id,symptoms,created_at,updated_at)
		VALUES ('incident','节点失联',10,20)`); err != nil {
		t.Fatal(err)
	}

	wf, err := store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	observe := incidentWorkflowStage(t, wf, "observe")
	if observe.Summary != "已登记故障症状" || len(observe.Actions) != 1 || observe.Actions[0].Timestamp != 10 {
		t.Fatalf("observation should reflect incident registration: %+v", observe)
	}
	ticket := incidentWorkflowStage(t, wf, "ticket")
	if ticket.Status != "pending" || ticket.Summary != "暂无关联工单" || len(ticket.Actions) != 0 {
		t.Fatalf("missing ticket should not promise a transfer: %+v", ticket)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO incident_hypotheses
		(hypothesis_id,incident_id,statement,proposed_by,state,created_at,updated_at)
		VALUES ('hyp1','incident','模型猜测','model','supported',11,11),
		       ('hyp2','incident','另一假设','worker','candidate',12,12)`); err != nil {
		t.Fatal(err)
	}
	// A stale, unproven root-cause value must not be projected as a fact,
	// including when recovery was independently verified.
	if _, err := store.db.ExecContext(ctx, `UPDATE incidents SET recovery_verified=1, root_cause='尚未证明的根因' WHERE incident_id='incident'`); err != nil {
		t.Fatal(err)
	}
	wf, err = store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	root := incidentWorkflowStage(t, wf, "root_cause")
	if root.Status != "active" || !strings.Contains(root.Summary, "2 条假设") || !strings.Contains(root.Summary, "1 条获支持") || wf.RootCauseProven || wf.RootCause != "" {
		t.Fatalf("supported hypothesis or recovery was treated as root-cause proof: workflow=%+v stage=%+v", wf, root)
	}
	encoded, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "尚未证明的根因") || strings.Contains(string(encoded), "模型猜测") {
		t.Fatalf("unproven claim leaked into workflow: %s", encoded)
	}
	if !wf.RecoveryVerified || incidentWorkflowStage(t, wf, "acceptance").Status != "completed" {
		t.Fatalf("recovery verification should remain independent: %+v", wf)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE incidents SET root_cause_proven=1, root_cause='已证明的根因' WHERE incident_id='incident'`); err != nil {
		t.Fatal(err)
	}
	wf, err = store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	root = incidentWorkflowStage(t, wf, "root_cause")
	if root.Status != "completed" || wf.RootCause != "已证明的根因" || !strings.Contains(root.Summary, wf.RootCause) {
		t.Fatalf("proven root cause not shown: workflow=%+v stage=%+v", wf, root)
	}
}

func TestIncidentWorkflowDistinguishesProposalFromExecution(t *testing.T) {
	store := setupIncidentsStore(t, "workflow-proposal.db")
	ctx := t.Context()
	statements := []string{
		`INSERT INTO incidents(incident_id,symptoms,state,created_at,updated_at)
		 VALUES ('incident','节点失联','investigating',1,50),('other','其他','open',1,1)`,
		// Identical timestamps require a consistent last-inserted tie breaker;
		// MAX on each field would also select the older row in this example.
		`INSERT INTO incident_proposals(proposal_id,incident_id,object_refs,capability_key,evidence_refs,risk_boundary,state,proposed_by,created_at)
		 VALUES ('old','incident','[]','z-old','[]','z-old-risk','rejected','model',12),
		        ('new','incident','[]','a-new','[]','a-new-risk','proposed','model',12),
		        ('foreign','other','[]','foreign','[]','foreign-risk','proposed','model',99)`,
	}
	for _, stmt := range statements {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	wf, err := store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	heal := incidentWorkflowStage(t, wf, "self_heal")
	if heal.Status != "pending" || wf.ActiveStep != "root_cause" || heal.Summary != "暂无自愈记录，有处置提案" {
		t.Fatalf("proposal should not look like an active execution: workflow=%+v stage=%+v", wf, heal)
	}
	if len(heal.Actions) != 1 || heal.Actions[0].ActionID != "new" || heal.Actions[0].Timestamp != 12 || heal.Actions[0].Status != "proposed" || !strings.Contains(heal.Actions[0].Summary, "a-new") {
		t.Fatalf("proposal action does not reflect the latest matching row: %+v", heal.Actions)
	}
	if heal.PolicyNote != "" || heal.ProposalRiskNote != "a-new：a-new-risk" {
		t.Fatalf("proposal risk should remain paired and separate from policy: %+v", heal)
	}
	logs, err := store.IncidentStepLogs(ctx, "incident", "self_heal")
	if err != nil {
		t.Fatal(err)
	}
	if logs.Streaming {
		t.Fatal("proposal alone must not report a running self-heal log")
	}

	// A proposed self-heal ledger record still does not mean execution has
	// started. Dispatching it is a separate state transition.
	for _, stmt := range []string{
		`INSERT INTO operations(operation_id,task_id,capability_key,target_refs,params_digest,idempotency_key,registered_at,updated_at)
		 VALUES ('op','incident','restart','[]','digest','key',20,20)`,
		`INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,created_at,updated_at)
		 VALUES ('execution','policy','node','op','proposed',21,21)`,
	} {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	wf, err = store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	heal = incidentWorkflowStage(t, wf, "self_heal")
	if heal.Status != "pending" || wf.ActiveStep != "root_cause" || len(heal.Actions) != 2 || heal.Actions[0].Status != "proposed" || strings.Contains(heal.Actions[0].Summary, "自愈执行") {
		t.Fatalf("proposed self-heal ledger record was treated as active: workflow=%+v stage=%+v", wf, heal)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE self_heal_executions SET state='dispatched' WHERE execution_id='execution'`); err != nil {
		t.Fatal(err)
	}
	// Once dispatched, it is independently active; the proposal keeps its own
	// identity, timestamp, and state alongside the execution.
	wf, err = store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	heal = incidentWorkflowStage(t, wf, "self_heal")
	if heal.Status != "active" || wf.ActiveStep != "self_heal" || len(heal.Actions) != 2 || heal.Actions[0].ActionID != "execution" || heal.Actions[1].ActionID != "new" || heal.Actions[1].Status != "proposed" {
		t.Fatalf("execution and proposal lost their distinct states: workflow=%+v stage=%+v", wf, heal)
	}
	logs, err = store.IncidentStepLogs(ctx, "incident", "self_heal")
	if err != nil || !logs.Streaming {
		t.Fatalf("dispatched execution should report active stage: logs=%+v err=%v", logs, err)
	}
}

func TestIncidentWorkflowTicketUsesTicketStateAndTimestamp(t *testing.T) {
	store := setupIncidentsStore(t, "workflow-ticket.db")
	ctx := t.Context()
	for _, stmt := range []string{
		`INSERT INTO incidents(incident_id,symptoms,created_at,updated_at) VALUES ('incident','节点失联',1,99)`,
		`INSERT INTO tickets(ticket_id,external_system,external_ticket_id,incident_id,object_ref,assignee,state,created_at,updated_at)
		 VALUES ('ticket','tracker','external','incident','node','operator','failed',10,25)`,
	} {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	wf, err := store.IncidentWorkflow(ctx, "incident")
	if err != nil {
		t.Fatal(err)
	}
	ticket := incidentWorkflowStage(t, wf, "ticket")
	if ticket.Status != "blocked" || len(ticket.Actions) != 1 || ticket.Actions[0].Timestamp != 25 || ticket.Actions[0].Status != "failed" {
		t.Fatalf("ticket state or update time came from another record: %+v", ticket)
	}
}
