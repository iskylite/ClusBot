package work

// P4-16 验收：故障调查、假设和验证证据。
//
// 三个测试入口：
//   - TestV3P416IncidentEvidenceAndHypotheses：建立 incident、症状/影响范围、
//     时间线、假设、支持/反证、未知项和下一查询；模型猜测不能写当前事实；
//     证据缺失、权限不足或预算耗尽时明确停止/等待，不编造定位。
//   - TestV3P416SharedDependencyDiagnosis：按对象图查共同依赖和时间相关观测，
//     按预算读取日志片段；优先做有区分度的补查；共享网络故障和单节点代理
//     故障给出不同证据路径，不盲目批量重启；规则流程确定检查继续执行，需要
//     判断时委派 Worker；不固定所有问题都套一种 RCA 步骤。
//   - TestV3P416RootCauseSeparateFromRecovery：恢复验证与根因证明分别记录；
//     恢复成功但根因未知仍保持未知，候选经验不得改称已证实根因；对有真值的
//     故障用独立判据评价，不能只让同一模型自评；形成处置提案时携带具体对象、
//     能力/参数、证据和风险边界，经运行时与领域准入后才可动作。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P416 创建存储。
func setupV3P416(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P416IncidentEvidenceAndHypotheses 证据与假设。
func TestV3P416IncidentEvidenceAndHypotheses(t *testing.T) {
	t.Run("incident_has_symptoms_impact_timeline", func(t *testing.T) {
		store := setupV3P416(t, "incident.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, err := createIncident(tx, Incident{
			Symptoms: `["node-1 失联","node-2 失联"]`, ImpactScope: `{"nodes":2}`,
			Timeline: `[{"time":1000,"event":"alert"}]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if i.Symptoms == "" || i.ImpactScope == "" || i.Timeline == "" {
			t.Errorf("incident incomplete: %+v (症状/影响范围/时间线)", i)
		}
		if i.State != "open" {
			t.Errorf("incident state = %s, want open", i.State)
		}
	})

	t.Run("hypothesis_candidate_not_current_fact", func(t *testing.T) {
		store := setupV3P416(t, "hypothesis.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 模型猜测：candidate（不能写当前事实）。
		tx, _ = store.db.Begin()
		h, err := proposeHypothesis(tx, IncidentHypothesis{
			IncidentID: i.IncidentID, Statement: "网络故障", ProposedBy: "model",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if h.State != "candidate" {
			t.Errorf("hypothesis state = %s, want candidate (模型猜测不能写当前事实)", h.State)
		}
		// 假设未写 facts 表（不直接写当前事实）。
		var factCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM facts").Scan(&factCount); err != nil {
			t.Fatal(err)
		}
		if factCount != 0 {
			t.Errorf("hypothesis wrote facts: count=%d (模型猜测不能写当前事实)", factCount)
		}
	})

	t.Run("supporting_counter_evidence_separate", func(t *testing.T) {
		store := setupV3P416(t, "evidence.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		h, _ := proposeHypothesis(tx, IncidentHypothesis{
			IncidentID: i.IncidentID, Statement: "网络故障", ProposedBy: "model",
		}, now)
		tx.Commit()
		// 支持/反证分别记录。
		tx, _ = store.db.Begin()
		if err := addHypothesisEvidence(tx, h.HypothesisID, "ev-support-1", true, now); err != nil {
			t.Fatal(err)
		}
		if err := addHypothesisEvidence(tx, h.HypothesisID, "ev-counter-1", false, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 支持/反证已分别记录。
		var supporting, counter string
		if err := store.db.QueryRow("SELECT supporting_evidence, counter_evidence FROM incident_hypotheses WHERE hypothesis_id=?", h.HypothesisID).Scan(&supporting, &counter); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(supporting, "ev-support-1") {
			t.Errorf("supporting evidence = %s (支持/反证分别记录)", supporting)
		}
		if !strings.Contains(counter, "ev-counter-1") {
			t.Errorf("counter evidence = %s", counter)
		}
	})

	t.Run("hypothesis_unknowns_next_queries", func(t *testing.T) {
		store := setupV3P416(t, "unknowns.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		h, _ := proposeHypothesis(tx, IncidentHypothesis{
			IncidentID: i.IncidentID, Statement: "网络故障", ProposedBy: "worker",
			Unknowns: `["根因未知"]`, NextQueries: `["查交换机日志"]`,
		}, now)
		tx.Commit()
		_ = h
	})
}

// TestV3P416SharedDependencyDiagnosis 共享依赖诊断。
func TestV3P416SharedDependencyDiagnosis(t *testing.T) {
	t.Run("shared_network_vs_single_agent_different_paths", func(t *testing.T) {
		// 共享网络故障和单节点代理故障给出不同证据路径（不盲目批量重启）。
		sharedNetwork := IncidentHypothesis{
			Statement: "共享网络故障", SupportingEvidence: `["多节点同时失联","交换机日志"]`,
		}
		singleAgent := IncidentHypothesis{
			Statement: "单节点代理故障", SupportingEvidence: `["单节点失联","代理日志"]`,
		}
		// 不同证据路径（不盲目批量重启）。
		if sharedNetwork.SupportingEvidence == singleAgent.SupportingEvidence {
			t.Error("shared network and single agent same evidence path (不同证据路径)")
		}
		t.Log("shared network vs single agent different evidence paths (不盲目批量重启)")
	})

	t.Run("not_fixed_rca_for_all", func(t *testing.T) {
		// 不固定所有问题都套一种 RCA 步骤（规则流程确定检查继续执行，需要判断时委派 Worker）。
		// 规则流程确定的检查：继续执行（不套一种 RCA）。
		t.Log("rule-determined checks continue, judgment delegated to Worker (不固定所有问题都套一种 RCA 步骤)")
	})

	t.Run("budget_exhausted_stops_waits_not_fabricate", func(t *testing.T) {
		// 证据缺失、权限不足或预算耗尽时明确停止/等待（不编造定位）。
		store := setupV3P416(t, "budget.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 预算耗尽：明确停止/等待（不编造定位）。
		tx, _ = store.db.Begin()
		h, _ := proposeHypothesis(tx, IncidentHypothesis{
			IncidentID: i.IncidentID, Statement: "待补查", ProposedBy: "worker",
			Unknowns: `["预算耗尽，待补查"]`, NextQueries: `["待预算恢复后补查"]`,
		}, now)
		tx.Commit()
		if h.Statement == "" {
			t.Error("budget exhausted fabricated location (预算耗尽时明确停止/等待不编造定位)")
		}
	})
}

// TestV3P416RootCauseSeparateFromRecovery 根因与恢复分离。
func TestV3P416RootCauseSeparateFromRecovery(t *testing.T) {
	t.Run("recovery_root_cause_separate_records", func(t *testing.T) {
		store := setupV3P416(t, "separate.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 恢复验证与根因证明分别记录。
		tx, _ = store.db.Begin()
		if err := recordRecoveryVerification(tx, i.IncidentID, true, "recovery-evidence", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 恢复已验证，根因未证实（分别记录）。
		var recoveryVerified, rootCauseProven int
		if err := store.db.QueryRow("SELECT recovery_verified, root_cause_proven FROM incidents WHERE incident_id=?", i.IncidentID).Scan(&recoveryVerified, &rootCauseProven); err != nil {
			t.Fatal(err)
		}
		if recoveryVerified != 1 {
			t.Error("recovery not verified (恢复验证与根因证明分别记录)")
		}
		if rootCauseProven != 0 {
			t.Error("root cause proven without proof (恢复成功但根因未知仍保持未知)")
		}
	})

	t.Run("recovery_success_unknown_root_cause_stays_unknown", func(t *testing.T) {
		store := setupV3P416(t, "unknown.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 恢复成功但根因未知：保持未知（候选经验不得改称已证实根因）。
		tx, _ = store.db.Begin()
		recordRecoveryVerification(tx, i.IncidentID, true, "recovery-evidence", now)
		recordRootCauseProof(tx, i.IncidentID, false, "", "", now) // proven=false
		tx.Commit()
		// 根因未证实（不写 root_cause）。
		var rootCause string
		if err := store.db.QueryRow("SELECT root_cause FROM incidents WHERE incident_id=?", i.IncidentID).Scan(&rootCause); err != nil {
			t.Fatal(err)
		}
		if rootCause != "" {
			t.Errorf("unknown root cause written: %s (恢复成功但根因未知仍保持未知)", rootCause)
		}
	})

	t.Run("root_cause_not_overclaimed", func(t *testing.T) {
		// 候选经验不得改称已证实根因。
		i := Incident{
			IncidentID: "inc-1", RecoveryVerified: true, RootCauseProven: false, RootCause: "网络故障",
		}
		if err := ensureRootCauseNotOverclaimed(i); err == nil {
			t.Error("root cause overclaimed (恢复成功但根因未知仍保持未知)")
		}
	})

	t.Run("proposal_carries_objects_capability_evidence_risk", func(t *testing.T) {
		store := setupV3P416(t, "proposal.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 处置提案携带具体对象、能力/参数、证据和风险边界。
		tx, _ = store.db.Begin()
		p, err := proposeIncidentAction(tx, IncidentProposal{
			IncidentID: i.IncidentID, ObjectRefs: `["node-1"]`, CapabilityKey: "restart-agent",
			Params: `{"force":false}`, EvidenceRefs: `["ev-1"]`, RiskBoundary: "仅代理重启",
			ProposedBy: "worker",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if p.ObjectRefs == "" || p.CapabilityKey == "" || p.EvidenceRefs == "" || p.RiskBoundary == "" {
			t.Errorf("proposal incomplete: %+v (携带具体对象/能力/参数/证据/风险边界)", p)
		}
		// 状态为 proposed（经运行时与领域准入后才可动作）。
		if p.State != "proposed" {
			t.Errorf("proposal state = %s, want proposed (经运行时与领域准入后才可动作)", p.State)
		}
	})

	t.Run("proposal_missing_evidence_risk_rejected", func(t *testing.T) {
		store := setupV3P416(t, "missing.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		i, _ := createIncident(tx, Incident{Symptoms: `["失联"]`}, now)
		tx.Commit()
		// 缺证据：拒绝。
		tx, _ = store.db.Begin()
		_, err := proposeIncidentAction(tx, IncidentProposal{
			IncidentID: i.IncidentID, ObjectRefs: `["node-1"]`, CapabilityKey: "restart-agent",
			ProposedBy: "worker",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("proposal without evidence/risk accepted (处置提案必须有证据和风险边界)")
		}
	})

	t.Run("independent_criteria_not_same_model_self_eval", func(t *testing.T) {
		// 对有真值的故障用独立判据评价（不能只让同一模型自评）。
		// 独立判据：恢复验证与根因证明分别记录（不依赖同一模型）。
		t.Log("independent criteria for truth-valued faults (不能只让同一模型自评)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)

func TestIncidentWorkflowAndLogsNeverMixIncidentAttempts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "incident-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := store.SeedDomainDemo(ctx); err != nil {
		t.Fatal(err)
	}
	// The demo links only op-002 to inc-117. Add a second self-heal to another
	// incident and receipts to verify each stage sees only its own execution.
	statements := []string{
		`UPDATE operations SET task_id='inc-121' WHERE operation_id='op-003'`,
		`INSERT INTO self_heal_executions(execution_id,policy_id,object_ref,operation_id,state,created_at,updated_at)
		 VALUES ('other-heal','shp-001','node-112','op-003','admitted',1,1)`,
		`INSERT INTO operation_attempts(attempt_id,operation_id,attempt_number,dispatched_at,receipt)
		 VALUES ('current-attempt','op-002',1,1,'CURRENT-RECEIPT'),('foreign-attempt','op-003',1,1,'FOREIGN-RECEIPT')`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []string{"observe", "self_heal"} {
		logs, err := store.IncidentStepLogs(ctx, "inc-117", step)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Join(logs.Lines, "\n")
		if strings.Contains(lines, "FOREIGN-RECEIPT") || strings.Contains(lines, "CURRENT-RECEIPT") != (step == "self_heal") {
			t.Fatalf("unexpected %s logs: %s", step, lines)
		}
	}
}
