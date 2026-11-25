package work

// 故障调查、假设和验证证据。
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
	"strings"
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

// createIncident 创建 incident。
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

// proposeHypothesis 提出假设。
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

// addHypothesisEvidence 添加假设证据。
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

// proposeIncidentAction 提出处置提案。
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

// recordRecoveryVerification 记录恢复验证。
// 恢复验证与根因证明分别记录；恢复成功但根因未知仍保持未知。
func recordRecoveryVerification(tx *sql.Tx, incidentID string, verified bool, evidence string, now int64) error {
	// 恢复验证与根因证明分别记录。
	_, err := tx.Exec(`UPDATE incidents SET recovery_verified=?, recovery_evidence=?, updated_at=? WHERE incident_id=?`,
		boolToInt(verified), evidence, now, incidentID)
	return err
}

// recordRootCauseProof 记录根因证明。
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

// ensureRootCauseNotOverclaimed 核对根因不过度声称。
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
	StepKey          string               `json:"step_key"`
	Label            string               `json:"label"`
	Status           string               `json:"status"` // completed, active, conditional, pending, blocked
	DataStatus       string               `json:"data_status,omitempty"`
	Summary          string               `json:"summary"`
	PolicyNote       string               `json:"policy_note,omitempty"`
	ProposalRiskNote string               `json:"proposal_risk_note,omitempty"`
	ImpactNote       string               `json:"impact_note,omitempty"`
	Actions          []IncidentStepAction `json:"actions"`
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

	var lastPropID, lastPropCap, lastPropRisk, lastPropState string
	var lastPropTime int64
	err = s.db.QueryRowContext(ctx, `SELECT proposal_id, capability_key, risk_boundary, state, created_at
		FROM incident_proposals WHERE incident_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, incidentID).
		Scan(&lastPropID, &lastPropCap, &lastPropRisk, &lastPropState, &lastPropTime)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IncidentWorkflowView{}, err
	}

	var ticketID, ticketState, ticketAssignee string
	var ticketUpdatedAt int64
	err = s.db.QueryRowContext(ctx, `SELECT ticket_id, state, assignee, updated_at FROM tickets WHERE incident_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, incidentID).
		Scan(&ticketID, &ticketState, &ticketAssignee, &ticketUpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IncidentWorkflowView{}, err
	}

	healRows, err := s.db.QueryContext(ctx, `
		SELECT execution_id, object_ref, COALESCE(operation_id,''), state, block_reason, verification_outcome, created_at
		FROM self_heal_executions
		WHERE operation_id IN (SELECT operation_id FROM operations WHERE task_id=?)
		ORDER BY created_at DESC LIMIT 10`, incidentID)
	if err != nil {
		return IncidentWorkflowView{}, err
	}
	defer healRows.Close()
	healActions := []IncidentStepAction{}
	healStatus := "pending"
	healHasProgress := false
	for healRows.Next() {
		var eid, obj, opID, st, blk, vo string
		var ts int64
		if err := healRows.Scan(&eid, &obj, &opID, &st, &blk, &vo, &ts); err != nil {
			return IncidentWorkflowView{}, err
		}
		summary := fmt.Sprintf("%s 自愈记录 (%s)", obj, st)
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
		if st != "proposed" {
			healHasProgress = true
		}
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
		Summary: "已登记故障症状",
		Actions: []IncidentStepAction{{ActionID: inc.IncidentID + ":obs", Timestamp: inc.CreatedAt, Summary: "症状登记: " + inc.Symptoms, Status: "completed"}},
	}
	rcStatus := "active"
	rcDataStatus := "live"
	rcSummary := fmt.Sprintf("待收敛（%d 条假设，%d 条获支持）", hypCount, supportedHyp)
	rootCause := ""
	if inc.RootCauseProven {
		rcStatus = "completed"
		rootCause = inc.RootCause
		rcSummary = "根因已证实"
		if rootCause != "" {
			rcSummary = "已证实: " + rootCause
		}
	} else if hypCount == 0 {
		rcDataStatus = "not_connected"
		rcSummary = "根因假设引擎未接入（待人工登记）"
	}
	rootCauseStage := IncidentWorkflowStage{
		StepKey:    "root_cause",
		Label:      "② 根因",
		Status:     rcStatus,
		DataStatus: rcDataStatus,
		Summary:    rcSummary,
		Actions:    []IncidentStepAction{{ActionID: inc.IncidentID + ":rc", Timestamp: inc.UpdatedAt, Summary: rcSummary, Status: rcStatus, HasLogs: inc.RootCauseEvidence != "", LogPreview: inc.RootCauseEvidence}},
	}
	healExecutionCount := len(healActions)
	var activeSelfHealPolicyCount int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM self_heal_policies WHERE state='active'`).Scan(&activeSelfHealPolicyCount)
	healDataStatus := "live"
	healSummary := "暂无自愈记录"
	if healExecutionCount > 0 {
		healSummary = fmt.Sprintf("最近 %d 条自愈记录", healExecutionCount)
	} else if activeSelfHealPolicyCount > 0 {
		healSummary = fmt.Sprintf("已配置 %d 条自愈策略，待触发", activeSelfHealPolicyCount)
	}
	proposalRiskNote := ""
	if lastPropID != "" {
		healSummary += "，有处置提案"
		healActions = append(healActions, IncidentStepAction{ActionID: lastPropID, Timestamp: lastPropTime, Summary: fmt.Sprintf("处置提案 %s", lastPropCap), Status: lastPropState})
		if lastPropRisk != "" {
			proposalRiskNote = lastPropCap + "：" + lastPropRisk
		}
	} else if healExecutionCount == 0 && activeSelfHealPolicyCount == 0 {
		healDataStatus = "not_connected"
		healSummary = "自动自愈执行器未接入"
	}
	selfHealStage := IncidentWorkflowStage{
		StepKey:          "self_heal",
		Label:            "③ 自愈",
		Status:           healStatus,
		DataStatus:       healDataStatus,
		Summary:          healSummary,
		ProposalRiskNote: proposalRiskNote,
		ImpactNote:       inc.ImpactScope,
		Actions:          healActions,
	}
	ticketStatus := "pending"
	ticketSummary := "暂无关联工单"
	ticketActions := []IncidentStepAction{}
	if ticketID != "" {
		ticketStatus = "active"
		if ticketState == "verified" || ticketState == "closed" {
			ticketStatus = "completed"
		} else if ticketState == "failed" {
			ticketStatus = "blocked"
		}
		ticketSummary = fmt.Sprintf("工单 %s (%s)", ticketID, ticketState)
		ticketActions = append(ticketActions, IncidentStepAction{ActionID: ticketID, Timestamp: ticketUpdatedAt, Summary: ticketSummary + " · " + ticketAssignee, Status: ticketState})
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
	case healHasProgress:
		activeStep = "self_heal"
	case hypCount > 0 || inc.RootCauseProven || inc.State == "investigating":
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
		RootCause:        rootCause,
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
			for _, act := range st.Actions {
				if act.LogPreview != "" {
					out.Lines = append(out.Lines, act.LogPreview)
				}
			}
		}
	}
	if stepKey != "self_heal" {
		return out, nil
	}
	opRows, err := s.db.QueryContext(ctx, `
		SELECT oa.attempt_id, oa.outcome, oa.receipt
		FROM self_heal_executions sh
		JOIN operations op ON op.operation_id=sh.operation_id
		JOIN operation_attempts oa ON oa.operation_id=op.operation_id
		WHERE op.task_id=? AND oa.receipt<>''
		ORDER BY oa.dispatched_at DESC, oa.attempt_id LIMIT 10`, incidentID)
	if err != nil {
		return IncidentStepLogView{}, err
	}
	defer opRows.Close()
	for opRows.Next() {
		var aid, outcome, receipt string
		if err := opRows.Scan(&aid, &outcome, &receipt); err != nil {
			return IncidentStepLogView{}, err
		}
		out.Lines = append(out.Lines, fmt.Sprintf("[attempt:%s %s] %s", aid, outcome, receipt))
	}
	if err := opRows.Err(); err != nil {
		return IncidentStepLogView{}, err
	}
	return out, nil
}

// AddIncidentHypothesis registers a root-cause hypothesis on an incident (Stage ②).
func (s *Store) AddIncidentHypothesis(ctx context.Context, subject, incidentID, statement string, unknowns, nextQueries []string) (IncidentHypothesis, error) {
	if strings.TrimSpace(subject) == "" {
		return IncidentHypothesis{}, ErrAuthorizationDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncidentHypothesis{}, err
	}
	defer tx.Rollback()

	stamp := now()
	if err := incidentExistsAndWritable(tx, incidentID); err != nil {
		return IncidentHypothesis{}, err
	}
	h, err := proposeHypothesis(tx, IncidentHypothesis{
		IncidentID:  incidentID,
		Statement:   strings.TrimSpace(statement),
		ProposedBy:  subject,
		Unknowns:    toJSON(nonNilStrings(unknowns)),
		NextQueries: toJSON(nonNilStrings(nextQueries)),
	}, stamp)
	if err != nil {
		return IncidentHypothesis{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incidents SET updated_at=? WHERE incident_id=?`, stamp, incidentID); err != nil {
		return IncidentHypothesis{}, err
	}
	if err := tx.Commit(); err != nil {
		return IncidentHypothesis{}, err
	}
	return h, nil
}

// RecordIncidentHypothesisEvidence appends supporting or counter evidence to a hypothesis.
func (s *Store) RecordIncidentHypothesisEvidence(ctx context.Context, subject, incidentID, hypothesisID, evidenceRef string, supporting bool, nextState string) error {
	if strings.TrimSpace(subject) == "" {
		return ErrAuthorizationDenied
	}
	if strings.TrimSpace(evidenceRef) == "" {
		return errors.New("evidence_ref 不能为空")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var ownerIncident string
	err = tx.QueryRowContext(ctx, `SELECT incident_id FROM incident_hypotheses WHERE hypothesis_id=?`, hypothesisID).Scan(&ownerIncident)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ownerIncident != incidentID) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	stamp := now()
	if err := addHypothesisEvidence(tx, hypothesisID, strings.TrimSpace(evidenceRef), supporting, stamp); err != nil {
		return err
	}
	if nextState != "" {
		switch nextState {
		case "open", "supported", "refuted", "inconclusive":
			if _, err := tx.ExecContext(ctx, `UPDATE incident_hypotheses SET state=?, updated_at=? WHERE hypothesis_id=?`, nextState, stamp, hypothesisID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("无效的假设状态：%s", nextState)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incidents SET updated_at=? WHERE incident_id=?`, stamp, incidentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveIncident records recovery verification and optional root-cause proof,
// enforcing ensureRootCauseNotOverclaimed before closing the incident.
func (s *Store) ResolveIncident(ctx context.Context, subject, incidentID string, recoveryVerified bool, recoveryEvidence string, rootCauseProven bool, rootCause, rootCauseEvidence string) (Incident, error) {
	if strings.TrimSpace(subject) == "" {
		return Incident{}, ErrAuthorizationDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback()

	if err := incidentExistsAndWritable(tx, incidentID); err != nil {
		return Incident{}, err
	}
	stamp := now()
	if err := recordRecoveryVerification(tx, incidentID, recoveryVerified, strings.TrimSpace(recoveryEvidence), stamp); err != nil {
		return Incident{}, err
	}
	if err := ensureRootCauseNotOverclaimed(Incident{
		IncidentID:       incidentID,
		RecoveryVerified: recoveryVerified,
		RootCauseProven:  rootCauseProven,
		RootCause:        strings.TrimSpace(rootCause),
	}); err != nil {
		return Incident{}, err
	}
	if rootCauseProven || strings.TrimSpace(rootCause) != "" || strings.TrimSpace(rootCauseEvidence) != "" {
		if err := recordRootCauseProof(tx, incidentID, rootCauseProven, strings.TrimSpace(rootCause), strings.TrimSpace(rootCauseEvidence), stamp); err != nil {
			return Incident{}, err
		}
	}
	nextState := "mitigated"
	if recoveryVerified {
		nextState = "resolved"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incidents SET state=?, updated_at=? WHERE incident_id=?`, nextState, stamp, incidentID); err != nil {
		return Incident{}, err
	}
	if err := tx.Commit(); err != nil {
		return Incident{}, err
	}
	return readIncidentByID(s.db, incidentID)
}

func readIncidentByID(db *sql.DB, incidentID string) (Incident, error) {
	var inc Incident
	var recV, rcP int
	err := db.QueryRow(`SELECT incident_id,symptoms,impact_scope,timeline,state,recovery_verified,recovery_evidence,root_cause_proven,root_cause,root_cause_evidence,created_at,updated_at
FROM incidents WHERE incident_id=?`, incidentID).Scan(
		&inc.IncidentID, &inc.Symptoms, &inc.ImpactScope, &inc.Timeline, &inc.State,
		&recV, &inc.RecoveryEvidence, &rcP, &inc.RootCause, &inc.RootCauseEvidence, &inc.CreatedAt, &inc.UpdatedAt)
	inc.RecoveryVerified = recV != 0
	inc.RootCauseProven = rcP != 0
	return inc, err
}

func incidentExistsAndWritable(tx *sql.Tx, incidentID string) error {
	var state string
	err := tx.QueryRow(`SELECT state FROM incidents WHERE incident_id=?`, incidentID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if state == "resolved" {
		return ErrState
	}
	return nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)

// RecentIncidents is a bounded list for the workflow selector. Workflows and
// raw receipts are read separately after the user chooses an incident.
type IncidentSummary struct {
	IncidentID string `json:"incident_id"`
	Symptoms   string `json:"symptoms"`
	State      string `json:"state"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (s *Store) RecentIncidents(ctx context.Context) ([]IncidentSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT incident_id,symptoms,state,updated_at
		FROM incidents ORDER BY updated_at DESC,incident_id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []IncidentSummary{}
	for rows.Next() {
		var item IncidentSummary
		if err := rows.Scan(&item.IncidentID, &item.Symptoms, &item.State, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
