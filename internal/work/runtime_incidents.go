package work

// P4-16：故障调查、假设和验证证据。
//
// 本文件实现调查能够比较假设和反证，定位与恢复结论分开：
//   - 建立 incident、症状/影响范围、时间线、假设、支持/反证、未知项和下一
//     查询；模型猜测不能写当前事实；
//   - 按对象图查共同依赖和时间相关观测，按预算读取日志片段；优先做有区
//     分度的补查；
//   - 规则流程确定检查继续执行，需要判断时委派 Worker；不固定所有问题都
//     套一种 RCA 步骤；
//   - 形成处置提案时携带具体对象、能力/参数、证据和风险边界，经运行时与
//     领域准入后才可动作；
//   - 恢复验证与根因证明分别记录；恢复成功但根因未知仍保持未知，候选经验
//     不得改称已证实根因。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Incident（症状/影响范围、时间线）
// ---------------------------------------------------------------------------

// Incident 是 incidents 的一行。
type Incident struct {
	IncidentID        string
	Symptoms          string
	ImpactScope       string
	Timeline          string
	State             string // open/investigating/mitigated/resolved/closed
	RecoveryVerified  bool
	RecoveryEvidence  string
	RootCauseProven   bool
	RootCause         string
	RootCauseEvidence string
	CreatedAt         int64
	UpdatedAt         int64
}

// createIncident 创建 incident（P4-16.1）。
// 建立 incident、症状/影响范围、时间线。
func createIncident(tx *sql.Tx, i Incident, now int64) (Incident, error) {
	if i.Symptoms == "" {
		return Incident{}, errors.New("incident 缺少症状")
	}
	i.IncidentID = id()
	i.State = "open"
	i.CreatedAt, i.UpdatedAt = now, now
	if i.ImpactScope == "" {
		i.ImpactScope = "{}"
	}
	if i.Timeline == "" {
		i.Timeline = "[]"
	}
	_, err := tx.Exec(`INSERT INTO incidents(incident_id,symptoms,impact_scope,timeline,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?)`, i.IncidentID, i.Symptoms, i.ImpactScope, i.Timeline, i.State, now, now)
	if err != nil {
		return Incident{}, err
	}
	return i, nil
}

// ---------------------------------------------------------------------------
// 假设（假设、支持/反证、未知项和下一查询；模型猜测不能写当前事实）
// ---------------------------------------------------------------------------

// IncidentHypothesis 是 incident_hypotheses 的一行。
type IncidentHypothesis struct {
	HypothesisID       string
	IncidentID         string
	Statement          string
	ProposedBy         string // model/worker/human
	SupportingEvidence string
	CounterEvidence    string
	State              string // candidate/supported/refuted/unknown
	Unknowns           string
	NextQueries        string
	CreatedAt          int64
	UpdatedAt          int64
}

// ErrModelGuessAsFact 是模型猜测写事实的错误。
var ErrModelGuessAsFact = errors.New("模型猜测不能写当前事实")

// proposeHypothesis 提出假设（P4-16.1）。
// 假设、支持/反证、未知项和下一查询；模型猜测不能写当前事实。
func proposeHypothesis(tx *sql.Tx, h IncidentHypothesis, now int64) (IncidentHypothesis, error) {
	if h.IncidentID == "" || h.Statement == "" || h.ProposedBy == "" {
		return IncidentHypothesis{}, errors.New("假设缺少 incident/内容/提出者")
	}
	// 模型猜测不能写当前事实：提出的假设为 candidate（不直接写事实）。
	h.HypothesisID = id()
	h.State = "candidate"
	h.CreatedAt, h.UpdatedAt = now, now
	if h.SupportingEvidence == "" {
		h.SupportingEvidence = "[]"
	}
	if h.CounterEvidence == "" {
		h.CounterEvidence = "[]"
	}
	if h.Unknowns == "" {
		h.Unknowns = "[]"
	}
	if h.NextQueries == "" {
		h.NextQueries = "[]"
	}
	_, err := tx.Exec(`INSERT INTO incident_hypotheses(hypothesis_id,incident_id,statement,proposed_by,
supporting_evidence,counter_evidence,state,unknowns,next_queries,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		h.HypothesisID, h.IncidentID, h.Statement, h.ProposedBy, h.SupportingEvidence,
		h.CounterEvidence, h.State, h.Unknowns, h.NextQueries, now, now)
	if err != nil {
		return IncidentHypothesis{}, err
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// 支持/反证（比较假设和反证）
// ---------------------------------------------------------------------------

// addHypothesisEvidence 添加假设证据（P4-16.1）。
// 比较假设和反证：支持/反证分别记录。
func addHypothesisEvidence(tx *sql.Tx, hypothesisID string, evidenceRef string, supporting bool, now int64) error {
	var field string
	if supporting {
		field = "supporting_evidence"
	} else {
		field = "counter_evidence"
	}
	// 支持/反证分别记录（比较假设和反证）。
	_, err := tx.Exec(fmt.Sprintf(`UPDATE incident_hypotheses SET %s=json_insert(%s, '$[#]', ?), updated_at=? WHERE hypothesis_id=?`,
		field, field), evidenceRef, now, hypothesisID)
	return err
}

// ---------------------------------------------------------------------------
// 处置提案（携带具体对象、能力/参数、证据和风险边界）
// ---------------------------------------------------------------------------

// IncidentProposal 是 incident_proposals 的一行。
type IncidentProposal struct {
	ProposalID    string
	IncidentID    string
	ObjectRefs    string
	CapabilityKey string
	Params        string
	EvidenceRefs  string
	RiskBoundary  string
	State         string // proposed/admitted/rejected/executed
	ProposedBy    string
	CreatedAt     int64
}

// proposeIncidentAction 提出处置提案（P4-16.4）。
// 形成处置提案时携带具体对象、能力/参数、证据和风险边界，经运行时与领域
// 准入后才可动作。
func proposeIncidentAction(tx *sql.Tx, p IncidentProposal, now int64) (IncidentProposal, error) {
	if p.IncidentID == "" || p.ObjectRefs == "" || p.CapabilityKey == "" || p.ProposedBy == "" {
		return IncidentProposal{}, errors.New("处置提案缺少 incident/对象/能力/提出者")
	}
	// 证据和风险边界必须有。
	if p.EvidenceRefs == "" || p.RiskBoundary == "" {
		return IncidentProposal{}, errors.New("处置提案缺少证据或风险边界")
	}
	p.ProposalID = id()
	p.State = "proposed"
	p.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO incident_proposals(proposal_id,incident_id,object_refs,capability_key,
params,evidence_refs,risk_boundary,state,proposed_by,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		p.ProposalID, p.IncidentID, p.ObjectRefs, p.CapabilityKey, p.Params, p.EvidenceRefs,
		p.RiskBoundary, p.State, p.ProposedBy, now)
	if err != nil {
		return IncidentProposal{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 恢复验证与根因证明分别记录
// ---------------------------------------------------------------------------

// recordRecoveryVerification 记录恢复验证（P4-16.5）。
// 恢复验证与根因证明分别记录；恢复成功但根因未知仍保持未知。
func recordRecoveryVerification(tx *sql.Tx, incidentID string, verified bool, evidence string, now int64) error {
	// 恢复验证与根因证明分别记录。
	_, err := tx.Exec(`UPDATE incidents SET recovery_verified=?, recovery_evidence=?, updated_at=? WHERE incident_id=?`,
		boolToInt(verified), evidence, now, incidentID)
	return err
}

// recordRootCauseProof 记录根因证明（P4-16.5）。
// 恢复成功但根因未知仍保持未知，候选经验不得改称已证实根因。
func recordRootCauseProof(tx *sql.Tx, incidentID string, proven bool, rootCause string, evidence string, now int64) error {
	// 恢复成功但根因未知仍保持未知：proven=false 时不写 root_cause。
	if !proven {
		rootCause = ""
	}
	_, err := tx.Exec(`UPDATE incidents SET root_cause_proven=?, root_cause=?, root_cause_evidence=?, updated_at=? WHERE incident_id=?`,
		boolToInt(proven), rootCause, evidence, now, incidentID)
	return err
}

// ensureRootCauseNotOverclaimed 核对根因不过度声称（P4-16.5）。
// 恢复成功但根因未知仍保持未知，候选经验不得改称已证实根因。
func ensureRootCauseNotOverclaimed(i Incident) error {
	if i.RecoveryVerified && !i.RootCauseProven && i.RootCause != "" {
		return fmt.Errorf("恢复成功但根因未知仍保持未知（候选经验不得改称已证实根因）：incident %s 有根因但未证实", i.IncidentID)
	}
	return nil
}

// IncidentStepAction 表示流程某一阶段内部可展开日志的执行动作。
type IncidentStepAction struct {
	ActionID   string `json:"action_id"`
	Timestamp  int64  `json:"timestamp"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	HasLogs    bool   `json:"has_logs"`
	LogPreview string `json:"log_preview,omitempty"`
}

// IncidentWorkflowStage 表示运维处置五步流程的一个交互节点（①观测 / ②根因 / ③自愈 / ④工单 / ⑤验收）。
type IncidentWorkflowStage struct {
	StepKey     string               `json:"step_key"`
	Label       string               `json:"label"`
	Status      string               `json:"status"` // completed, active, conditional, pending, blocked
	Summary     string               `json:"summary"`
	PolicyNote  string               `json:"policy_note,omitempty"`
	ImpactNote  string               `json:"impact_note,omitempty"`
	Actions     []IncidentStepAction `json:"actions"`
}

// IncidentWorkflowView 是故障处置五步流程及右侧详情抽屉的结构化投影。
type IncidentWorkflowView struct {
	IncidentID       string                  `json:"incident_id"`
	State            string                  `json:"state"`
	Symptoms         string                  `json:"symptoms"`
	ImpactScope      string                  `json:"impact_scope"`
	ActiveStep       string                  `json:"active_step"`
	RecoveryVerified bool                    `json:"recovery_verified"`
	RootCauseProven  bool                    `json:"root_cause_proven"`
	RootCause        string                  `json:"root_cause,omitempty"`
	Stages           []IncidentWorkflowStage `json:"stages"`
}

// IncidentStepLogView 是点击流程节点内某步骤展开的实时诊断/执行日志流响应。
type IncidentStepLogView struct {
	IncidentID string   `json:"incident_id"`
	StepKey    string   `json:"step_key"`
	ActionID   string   `json:"action_id,omitempty"`
	Source     string   `json:"source"`
	Streaming  bool     `json:"streaming"`
	Lines      []string `json:"lines"`
}

// IncidentWorkflow 返回单个故障事件的五步处置流程（观测/根因/自愈/工单/验收）及其执行动作列表。
func (s *Store) IncidentWorkflow(ctx context.Context, incidentID string) (IncidentWorkflowView, error) {
	var inc Incident
	var recVer, rcProv int
	err := s.db.QueryRowContext(ctx, `
		SELECT incident_id, symptoms, impact_scope, timeline, state,
		       recovery_verified, recovery_evidence, root_cause_proven, root_cause, root_cause_evidence, created_at, updated_at
		FROM incidents WHERE incident_id=?`, incidentID).
		Scan(&inc.IncidentID, &inc.Symptoms, &inc.ImpactScope, &inc.Timeline, &inc.State,
			&recVer, &inc.RecoveryEvidence, &rcProv, &inc.RootCause, &inc.RootCauseEvidence, &inc.CreatedAt, &inc.UpdatedAt)
	if err != nil {
		return IncidentWorkflowView{}, err
	}
	inc.RecoveryVerified = recVer == 1
	inc.RootCauseProven = rcProv == 1

	var hypCount, supportedHyp int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN state='supported' THEN 1 ELSE 0 END), 0) FROM incident_hypotheses WHERE incident_id=?`, incidentID).
		Scan(&hypCount, &supportedHyp); err != nil {
		return IncidentWorkflowView{}, err
	}

	var propCount int
	var lastPropCap, lastPropRisk string
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(capability_key),''), COALESCE(MAX(risk_boundary),'') FROM incident_proposals WHERE incident_id=?`, incidentID).
		Scan(&propCount, &lastPropCap, &lastPropRisk)
	if err != nil {
		return IncidentWorkflowView{}, err
	}

	var ticketID, ticketState, ticketObj, ticketAssignee string
	err = s.db.QueryRowContext(ctx, `SELECT ticket_id, state, object_ref, assignee FROM tickets WHERE incident_id=? ORDER BY created_at DESC LIMIT 1`, incidentID).
		Scan(&ticketID, &ticketState, &ticketObj, &ticketAssignee)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IncidentWorkflowView{}, err
	}

	healRows, err := s.db.QueryContext(ctx, `
		SELECT execution_id, object_ref, COALESCE(operation_id,''), state, block_reason, verification_outcome, created_at
		FROM self_heal_executions
		WHERE (?='' OR object_ref=? OR operation_id IN (SELECT operation_id FROM operations WHERE target_refs LIKE '%'||?||'%'))
		ORDER BY created_at DESC LIMIT 10`, ticketObj, ticketObj, incidentID)
	if err != nil {
		return IncidentWorkflowView{}, err
	}
	defer healRows.Close()
	healActions := []IncidentStepAction{}
	healStatus := "pending"
	for healRows.Next() {
		var eid, obj, opID, st, blk, vo string
		var ts int64
		if err := healRows.Scan(&eid, &obj, &opID, &st, &blk, &vo, &ts); err != nil {
			return IncidentWorkflowView{}, err
		}
		summary := fmt.Sprintf("%s 自愈执行 (%s)", obj, st)
		if vo != "" {
			summary += " · " + vo
		}
		if blk != "" {
			summary += " · " + blk
		}
		healActions = append(healActions, IncidentStepAction{
			ActionID:   eid,
			Timestamp:  ts,
			Summary:    summary,
			Status:     st,
			HasLogs:    opID != "" || vo != "" || blk != "",
			LogPreview: vo,
		})
		if st == "admitted" || st == "dispatched" {
			healStatus = "active"
		} else if st == "verified" && healStatus == "pending" {
			healStatus = "completed"
		} else if st == "failed" || st == "blocked" {
			healStatus = "blocked"
		}
	}
	if err := healRows.Err(); err != nil {
		return IncidentWorkflowView{}, err
	}

	observeStage := IncidentWorkflowStage{
		StepKey: "observe",
		Label:   "① 观测",
		Status:  "completed",
		Summary: "已采集故障症状与影响面快照",
		Actions: []IncidentStepAction{{ActionID: inc.IncidentID + ":obs", Timestamp: inc.CreatedAt, Summary: "症状登记: " + inc.Symptoms, Status: "completed"}},
	}
	rcStatus := "active"
	rcSummary := fmt.Sprintf("待收敛 (%d 条假设)", hypCount)
	if inc.RootCauseProven || supportedHyp > 0 {
		rcStatus = "completed"
		rcSummary = "已定位: " + inc.RootCause
		if inc.RootCause == "" {
			rcSummary = "已核实假设证据"
		}
	}
	rootCauseStage := IncidentWorkflowStage{
		StepKey: "root_cause",
		Label:   "② 根因",
		Status:  rcStatus,
		Summary: rcSummary,
		Actions: []IncidentStepAction{{ActionID: inc.IncidentID + ":rc", Timestamp: inc.UpdatedAt, Summary: rcSummary, Status: rcStatus, HasLogs: inc.RootCauseEvidence != "", LogPreview: inc.RootCauseEvidence}},
	}
	if len(healActions) == 0 && propCount > 0 {
		healStatus = "active"
		healActions = append(healActions, IncidentStepAction{ActionID: inc.IncidentID + ":prop", Timestamp: inc.UpdatedAt, Summary: fmt.Sprintf("候选处置 %s", lastPropCap), Status: "proposed"})
	}
	selfHealStage := IncidentWorkflowStage{
		StepKey:    "self_heal",
		Label:      "③ 自愈",
		Status:     healStatus,
		Summary:    fmt.Sprintf("%d 条自愈记录", len(healActions)),
		PolicyNote: lastPropRisk,
		ImpactNote: inc.ImpactScope,
		Actions:    healActions,
	}
	ticketStatus := "conditional"
	ticketSummary := "若自愈失败自动转单"
	ticketActions := []IncidentStepAction{}
	if ticketID != "" {
		ticketStatus = "active"
		if ticketState == "verified" || ticketState == "closed" {
			ticketStatus = "completed"
		}
		ticketSummary = fmt.Sprintf("工单 %s (%s)", ticketID, ticketState)
		ticketActions = append(ticketActions, IncidentStepAction{ActionID: ticketID, Timestamp: inc.UpdatedAt, Summary: ticketSummary + " · " + ticketAssignee, Status: ticketState})
	}
	ticketStage := IncidentWorkflowStage{
		StepKey: "ticket",
		Label:   "④ 工单",
		Status:  ticketStatus,
		Summary: ticketSummary,
		Actions: ticketActions,
	}
	accStatus := "pending"
	accSummary := "待复检"
	accActions := []IncidentStepAction{}
	if inc.RecoveryVerified {
		accStatus = "completed"
		accSummary = "复检已通过"
		accActions = append(accActions, IncidentStepAction{ActionID: inc.IncidentID + ":verify", Timestamp: inc.UpdatedAt, Summary: "恢复验证通过", Status: "verified", HasLogs: inc.RecoveryEvidence != "", LogPreview: inc.RecoveryEvidence})
	}
	accStage := IncidentWorkflowStage{
		StepKey: "acceptance",
		Label:   "⑤ 验收",
		Status:  accStatus,
		Summary: accSummary,
		Actions: accActions,
	}

	activeStep := "self_heal"
	switch {
	case inc.RecoveryVerified || inc.State == "resolved" || inc.State == "closed":
		activeStep = "acceptance"
	case ticketID != "":
		activeStep = "ticket"
	case len(healActions) > 0:
		activeStep = "self_heal"
	case hypCount > 0 || inc.State == "investigating":
		activeStep = "root_cause"
	default:
		activeStep = "observe"
	}

	return IncidentWorkflowView{
		IncidentID:       inc.IncidentID,
		State:            inc.State,
		Symptoms:         inc.Symptoms,
		ImpactScope:      inc.ImpactScope,
		ActiveStep:       activeStep,
		RecoveryVerified: inc.RecoveryVerified,
		RootCauseProven:  inc.RootCauseProven,
		RootCause:        inc.RootCause,
		Stages:           []IncidentWorkflowStage{observeStage, rootCauseStage, selfHealStage, ticketStage, accStage},
	}, nil
}

// IncidentStepLogs 返回故障处置某一步骤的关联执行回执、证据定位及可展开日志行。
func (s *Store) IncidentStepLogs(ctx context.Context, incidentID, stepKey string) (IncidentStepLogView, error) {
	wf, err := s.IncidentWorkflow(ctx, incidentID)
	if err != nil {
		return IncidentStepLogView{}, err
	}
	out := IncidentStepLogView{
		IncidentID: incidentID,
		StepKey:    stepKey,
		Source:     "incident:" + incidentID + ":" + stepKey,
		Lines:      []string{},
	}
	for _, st := range wf.Stages {
		if st.StepKey == stepKey {
			out.Streaming = st.Status == "active"
			if st.Summary != "" {
				out.Lines = append(out.Lines, "[阶段状态] "+st.Label+" · "+st.Summary)
			}
			for _, act := range st.Actions {
				out.Lines = append(out.Lines, fmt.Sprintf("[%d] %s (%s)", act.Timestamp, act.Summary, act.Status))
				if act.LogPreview != "" {
					out.Lines = append(out.Lines, act.LogPreview)
				}
			}
		}
	}
	// 附加关联 Operation Attempt 的实际回执输出
	opRows, err := s.db.QueryContext(ctx, `
		SELECT oa.attempt_id, oa.outcome, oa.receipt
		FROM self_heal_executions sh
		JOIN operation_attempts oa ON oa.operation_id=sh.operation_id
		WHERE sh.operation_id<>'' AND oa.receipt<>''
		ORDER BY oa.dispatched_at DESC LIMIT 10`)
	if err == nil {
		defer opRows.Close()
		for opRows.Next() {
			var aid, outcome, receipt string
			if scanErr := opRows.Scan(&aid, &outcome, &receipt); scanErr == nil {
				out.Lines = append(out.Lines, fmt.Sprintf("[attempt:%s %s] %s", aid, outcome, receipt))
			}
		}
		_ = opRows.Err()
	}
	return out, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
