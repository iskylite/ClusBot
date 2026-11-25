package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func setupSelfHealFixture(t *testing.T, mode string, hasBinding bool) (*Store, Incident, SelfHealPolicy) {
	t.Helper()
	ctx := context.Background()
	store, conn := setupMCPHealthFixture(t, mode)
	inc, err := store.CreateIncident(ctx, "service reports failure", `{"nodes":["node-1"]}`)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := store.UpsertSelfHealPolicy(ctx, SelfHealPolicyInput{
		PolicyKey: "service.repair", Subject: "local-owner", AssetScope: `{"nodes":["node-1"]}`,
		ActionCapability: "ops.self_heal", State: "active", VerificationStandard: "independent_health",
		PerNodeMaxWrites: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasBinding {
		bindMCPHealthFixture(t, store, conn, "ops.self_heal", "repair_test_service", "action")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stamp := now()
	if _, err := upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "SN-1", State: AssetStateInService}, stamp); err != nil {
		t.Fatal(err)
	}
	for _, act := range []struct{ action, object, scope string }{
		{"ops.self_heal", "node-1", "self_heal"},
		{"dispatch", "ops.self_heal", ""},
	} {
		if _, err := tx.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,scope_label,policy_version,valid_from,expires_at,revoked_at,owner,created_at,updated_at)
VALUES(?,?,?,?,?,1,?,0,0,'local-owner',?,?)`, id(), "local-owner", act.action, act.object, act.scope, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return store, inc, pol
}

func TestExecuteIncidentSelfHealOperationLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		mode        string
		state       string
		op          RuntimeOperationState
		verifies    bool
		evidence    string
		checkErr    error
		checkCalled bool
		changeAsset bool
	}{
		{"asynchronous acceptance", "write_async", "dispatched", RuntimeOperationReconcile, true, "must not be used", nil, false, false},
		{"MCP IsError despite positive-looking body", "write_error", "dispatched", RuntimeOperationReconcile, true, "must not be used", nil, false, false},
		{"lost MCP reply", "write_lost", "dispatched", RuntimeOperationReconcile, true, "must not be used", nil, false, false},
		{"no target receipt", "write_empty", "dispatched", RuntimeOperationReconcile, true, "must not be used", nil, false, false},
		{"result for another target", "write_wrong_target", "dispatched", RuntimeOperationReconcile, true, "must not be used", nil, false, false},
		{"health coverage unknown", "write_ok", "dispatched", RuntimeOperationReconcile, false, "", ErrHealthCoverage, true, false},
		{"missing verification evidence", "write_ok", "dispatched", RuntimeOperationReconcile, true, " ", nil, true, false},
		{"asset identity changes during verification", "write_ok", "dispatched", RuntimeOperationReconcile, true, "new healthy test", nil, true, true},
		{"actual unhealthy check", "write_ok", "failed", RuntimeOperationFailed, false, "service still faulted", nil, true, false},
		{"independent positive check", "write_ok", "verified", RuntimeOperationSucceeded, true, "new healthy test", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, inc, pol := setupSelfHealFixture(t, tc.mode, true)
			calls := 0
			checker := func(_ context.Context, ref string) (bool, string, error) {
				calls++
				if ref != "node-1" {
					t.Fatalf("wrong verification target %q", ref)
				}
				if tc.changeAsset {
					if _, err := store.db.Exec(`UPDATE ops_assets SET identity='REPLACED',revision=revision+1 WHERE object_ref='node-1'`); err != nil {
						t.Fatal(err)
					}
				}
				return tc.verifies, tc.evidence, tc.checkErr
			}
			ex, err := store.ExecuteIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, pol.PolicyID, "node-1", checker)
			if err != nil || ex.State != tc.state || (calls != 0) != tc.checkCalled || calls > 1 {
				t.Fatalf("execution=%+v checker calls=%d err=%v", ex, calls, err)
			}
			op, err := readOperation(store.db, ex.OperationID)
			if err != nil || op.State != tc.op || op.ConnectionID == "" || !strings.Contains(op.TargetRefs, `"revision":1`) {
				t.Fatalf("operation=%+v want=%q error=%v", op, tc.op, err)
			}
			var incidentVerified int
			if err := store.db.QueryRow(`SELECT recovery_verified FROM incidents WHERE incident_id=?`, inc.IncidentID).Scan(&incidentVerified); err != nil || incidentVerified != 0 {
				t.Fatalf("an asset result changed incident-wide acceptance=%d (%v)", incidentVerified, err)
			}
			task, err := readTaskState(store.db, op.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			var holdState string
			if err := store.db.QueryRow(`SELECT state FROM quota_reservations WHERE target_ref=? AND task_id=? AND scope='target_hold'`, "node-1", op.TaskID).Scan(&holdState); err != nil {
				t.Fatal(err)
			}
			shouldHold := tc.op == RuntimeOperationReconcile
			if (holdState == "reserved") != shouldHold || (holdState == "released") == shouldHold {
				t.Errorf("unknown operation did not preserve exclusive target hold, or terminal receipt did not release it: %s", holdState)
			}
			switch tc.op {
			case RuntimeOperationSucceeded:
				if task.State != RuntimeTaskSucceeded {
					t.Errorf("completed repair task: %+v", task)
				}
			case RuntimeOperationFailed:
				if task.State != RuntimeTaskFailed {
					t.Errorf("failed repair task: %+v", task)
				}
			case RuntimeOperationReconcile:
				if !task.ReconcileRequired || task.State.IsTerminal() {
					t.Errorf("unsettled repair task: %+v", task)
				}
				if tc.mode == "write_async" {
					var payload string
					if err := store.db.QueryRow(`SELECT receipt FROM operation_attempts WHERE attempt_id=?`, op.CurrentAttemptID).Scan(&payload); err != nil {
						t.Fatal(err)
					}
					var receipt selfHealReceipt
					if err := json.Unmarshal([]byte(payload), &receipt); err != nil || receipt.TaskID != "mcp-repair-1" || receipt.Completed || receipt.CallID == "" {
						t.Fatalf("lost async provider ownership: %+v err=%v", receipt, err)
					}
				}
				v, err := store.ExecuteIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, pol.PolicyID, "node-1", checker)
				if err != nil || v.State != "blocked" || !strings.Contains(v.BlockReason, "未核对") {
					t.Fatalf("unknown operation was re-issued: execution=%+v err=%v", v, err)
				}
			}
			assertHealthCalls(t, store, "repair_test_service", 1)
		})
	}
}

func TestSelfHealDefaultAndObservationEntryNeverWrite(t *testing.T) {
	ctx := context.Background()
	store, inc, active := setupSelfHealFixture(t, "write_ok", true)
	p, err := store.UpsertSelfHealPolicy(ctx, SelfHealPolicyInput{PolicyKey: "draft", Subject: "local-owner", ActionCapability: "ops.self_heal"})
	if err != nil || p.State != "draft" {
		t.Fatalf("a new policy is not draft: %+v %v", p, err)
	}
	if _, err := store.ProposeIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, active.PolicyID, "node-1"); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("observe entry used write policy: %v", err)
	}
	if ex, err := store.ExecuteIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, active.PolicyID, "node-1", nil); err != nil || ex.State != "blocked" || ex.OperationID != "" {
		t.Fatalf("no checker admitted write: %+v %v", ex, err)
	}
	if _, err := store.ExecuteIncidentSelfHeal(ctx, "someone-else", inc.IncidentID, active.PolicyID, "node-1", nil); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("foreign policy used: %v", err)
	}
	p, err = store.UpsertSelfHealPolicy(ctx, SelfHealPolicyInput{
		PolicyKey: "manual-check", Subject: "local-owner", ActionCapability: "ops.self_heal", ObserveOnly: true, State: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := store.ProposeIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, p.PolicyID, "node-1")
	if err != nil || ex.State != "proposed" || ex.OperationID != "" {
		t.Fatalf("legitimate observation %+v err=%v", ex, err)
	}
	assertHealthCalls(t, store, "repair_test_service", 0)
}

func TestSelfHealBlocksMissingBindingUnverifiedScopeAndPreconditions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, editSQL, want string
		bound               bool
	}{
		{"no binding", "", "MCP 能力绑定", false},
		{"shared event", `UPDATE incidents SET impact_scope='{"nodes":["node-1","node-2"]}'`, "多个节点", true},
		{"changed asset", `UPDATE ops_assets SET frozen_at=1`, "冻结", true},
		{"unknown asset", `DELETE FROM ops_assets`, "稳定资产身份", true},
		{"policy unrelated", `UPDATE self_heal_policies SET asset_scope='{"nodes":["node-2"]}'`, "策略范围", true},
		{"valid loan despite stale asset state", `INSERT INTO loans(loan_id,object_ref,handover_id,lender,borrower,ownership_epoch_at_lend,lent_at) VALUES('active-loan','node-1','h','owner','holder',1,1)`, "借出", true},
		{"explicit identity conflict", `INSERT INTO identity_claims(claim_id,candidate_object_ref,candidate_identity,identity_kind,claimed_by,claimed_at) VALUES ('claim-a','node-1','shared-ip','ip','import',1),('claim-b','node-2','shared-ip','ip','import',2)`, "身份冲突", true},
		{"owned by a benchmark", `INSERT INTO quota_reservations(reservation_id,scope,target_ref,amount,unit,exclusive,reserved_at) VALUES('benchmark-hold','target_hold','node-1',1,'hold',1,1)`, "其他任务占用", true},
		{"unsupported text guard", `UPDATE self_heal_policies SET preconditions='["maybe ok"]'`, "不能核实", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, inc, active := setupSelfHealFixture(t, "write_ok", tc.bound)
			if tc.editSQL != "" {
				if _, err := store.db.Exec(tc.editSQL); err != nil {
					t.Fatal(err)
				}
			}
			checker := func(context.Context, string) (bool, string, error) {
				t.Fatal("checker called before proof of successful admissible write")
				return false, "", nil
			}
			ex, err := store.ExecuteIncidentSelfHeal(ctx, "local-owner", inc.IncidentID, active.PolicyID, "node-1", checker)
			if err != nil || ex.State != "blocked" || ex.OperationID != "" || !strings.Contains(ex.BlockReason, tc.want) {
				t.Fatalf("bad admission: execution=%+v err=%v", ex, err)
			}
			assertHealthCalls(t, store, "repair_test_service", 0)
		})
	}
}
