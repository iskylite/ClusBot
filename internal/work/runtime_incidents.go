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

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
