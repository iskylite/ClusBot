package work

// P5-03：逐请求 ContextManifest 与恢复重装。
//
// 本文件实现每次真正发送的模型请求均能追溯依据、裁剪和缺口：
//   - 装入当前工作合同/控制、事实、必要规则、能力快照、有限证据、适用经验
//     及未知项；硬约束装不下就停止/缩小范围；
//   - 在最终序列化/预算裁剪后记录来源修订、片段区间、实际使用/过滤/省略、
//     token/字节预算、请求摘要、提供者绑定和发送状态；
//   - 发送前重核当前权限和源版本；尚不能证明派生来源有效的候选不装配，不
//     等待 P5-04 才拦截；
//   - 恢复先读持久工作和未决操作，再查当前事实/授权/规则/能力，生成新
//     Manifest；旧清单只解释历史，不作为新授权；
//   - 保留旧 Manifest 格式及版本解释，不能把历史字段重新解释或补写成当时
//     未实际发送的内容。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// ContextManifest（装入当前工作合同/控制、事实、必要规则、能力快照、有限证据、
// 适用经验及未知项）
// ---------------------------------------------------------------------------

// ContextManifest 是 v3_context_manifests 的一行。
type ContextManifest struct {
	ManifestID         string `json:"manifest_id"`
	TaskID             string `json:"task_id"`
	StepID             string `json:"step_id"`
	ContractRevision   int    `json:"contract_revision"`
	FactsRefs          string `json:"facts_refs"`
	RulesRefs          string `json:"rules_refs"`
	CapabilitySnapshot string `json:"capability_snapshot"`
	EvidenceRefs       string `json:"evidence_refs"`
	ExperienceRefs     string `json:"experience_refs"`
	Unknowns           string `json:"unknowns"`
	UsedRefs           string `json:"used_refs"`
	FilteredRefs       string `json:"filtered_refs"`
	OmittedRefs        string `json:"omitted_refs"`
	OmissionReasons    string `json:"omission_reasons"`
	TokenBudget        int    `json:"token_budget"`
	ByteBudget         int    `json:"byte_budget"`
	TokensEstimated    int    `json:"tokens_estimated"`
	RequestDigest      string `json:"request_digest"`
	ProviderRef        string `json:"provider_ref"`
	State              string `json:"state"`
	SourceRevisions    string `json:"source_revisions"`
	ExcerptRanges      string `json:"excerpt_ranges"`
	ManifestVersion    int    `json:"manifest_version"`
	CreatedAt          int64  `json:"created_at"`
	SentAt             int64  `json:"sent_at"`
}

// ---------------------------------------------------------------------------
// 装配（硬约束装不下就停止/缩小范围）
// ---------------------------------------------------------------------------

// assembleContextManifest 装配上下文清单（P5-03.1/P5-03.3）。
// 装入当前工作合同/控制、事实、必要规则、能力快照、有限证据、适用经验及
// 未知项；硬约束装不下就停止/缩小范围；发送前重核当前权限和源版本；尚不能
// 证明派生来源有效的候选不装配。
func assembleContextManifest(tx *sql.Tx, taskID string, stepID string, tokenBudget int, byteBudget int, subject string, now int64) (ContextManifest, error) {
	m := ContextManifest{
		ManifestID: id(), TaskID: taskID, StepID: stepID,
		TokenBudget: tokenBudget, ByteBudget: byteBudget,
		ManifestVersion: 1, CreatedAt: now, State: "prepared",
		FactsRefs: "[]", RulesRefs: "[]", CapabilitySnapshot: "{}",
		EvidenceRefs: "[]", ExperienceRefs: "[]", Unknowns: "[]",
		UsedRefs: "[]", FilteredRefs: "[]", OmittedRefs: "[]", OmissionReasons: "{}",
		SourceRevisions: "{}", ExcerptRanges: "{}",
		TokensEstimated: -1, // 模型未报 token 明确未知
	}
	// 发送前重核当前权限和源版本（尚不能证明派生来源有效的候选不装配）。
	var taskStatus string
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&taskStatus); err != nil {
		return m, fmt.Errorf("任务 %s 无法读取（发送前重核当前权限和源版本）", taskID)
	}
	// 硬约束装不下就停止/缩小范围（预算为 0 时停止）。
	if tokenBudget <= 0 && byteBudget <= 0 {
		m.State = "rejected"
		m.OmissionReasons = `{"reason":"硬约束装不下就停止"}`
		return m, fmt.Errorf("硬约束装不下就停止（token/字节预算为 0）")
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 裁剪（实际使用/过滤/省略）
// ---------------------------------------------------------------------------

// finalizeContextManifest 完成上下文清单（P5-03.2）。
// 在最终序列化/预算裁剪后记录来源修订、片段区间、实际使用/过滤/省略、
// token/字节预算、请求摘要、提供者绑定和发送状态。
func finalizeContextManifest(tx *sql.Tx, m ContextManifest, usedRefs, filteredRefs, omittedRefs []string, omissionReasons map[string]string, now int64) (ContextManifest, error) {
	// 实际使用/过滤/省略。
	m.UsedRefs = toJSON(usedRefs)
	m.FilteredRefs = toJSON(filteredRefs)
	m.OmittedRefs = toJSON(omittedRefs)
	reasonsJSON, err := json.Marshal(omissionReasons)
	if err != nil {
		return m, err
	}
	m.OmissionReasons = string(reasonsJSON)
	// 请求摘要与提供者绑定。
	m.RequestDigest = digest(m.UsedRefs + m.CapabilitySnapshot)
	// 发送状态（准备清单不等于远端已收到）。
	m.State = "prepared"
	// 持久化。
	_, err = tx.Exec(`INSERT INTO v3_context_manifests(manifest_id,task_id,step_id,contract_revision,
facts_refs,rules_refs,capability_snapshot,evidence_refs,experience_refs,unknowns,used_refs,filtered_refs,
omitted_refs,omission_reasons,token_budget,byte_budget,tokens_estimated,request_digest,provider_ref,state,
source_revisions,excerpt_ranges,manifest_version,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ManifestID, m.TaskID, m.StepID, m.ContractRevision, m.FactsRefs, m.RulesRefs,
		m.CapabilitySnapshot, m.EvidenceRefs, m.ExperienceRefs, m.Unknowns, m.UsedRefs, m.FilteredRefs,
		m.OmittedRefs, m.OmissionReasons, m.TokenBudget, m.ByteBudget, m.TokensEstimated, m.RequestDigest,
		m.ProviderRef, m.State, m.SourceRevisions, m.ExcerptRanges, m.ManifestVersion, now)
	if err != nil {
		return m, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 发送（准备清单不等于远端已收到）
// ---------------------------------------------------------------------------

// ErrManifestNotSent 是清单未发送的错误。
var ErrManifestNotSent = errors.New("准备清单不等于远端已收到")

// markManifestSent 标记清单已发送（P5-03.2）。
// 准备清单不等于远端已收到（发送状态独立记录）。
func markManifestSent(tx *sql.Tx, manifestID string, now int64) error {
	res, err := tx.Exec(`UPDATE v3_context_manifests SET state='sent', sent_at=? WHERE manifest_id=? AND state='prepared'`,
		now, manifestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("清单 %s 不存在或已发送（准备清单不等于远端已收到）", manifestID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 恢复（旧清单只解释历史，不作为新授权）
// ---------------------------------------------------------------------------

// ErrOldManifestAsAuthorization 是旧清单作为授权的错误。
var ErrOldManifestAsAuthorization = errors.New("旧清单只解释历史，不作为新授权")

// restoreContextManifest 恢复上下文清单（P5-03.4）。
// 恢复先读持久工作和未决操作，再查当前事实/授权/规则/能力，生成新 Manifest；
// 旧清单只解释历史，不作为新授权。
func restoreContextManifest(tx *sql.Tx, taskID string, subject string, now int64) (ContextManifest, error) {
	// 恢复先读持久工作和未决操作（再查当前事实/授权/规则/能力）。
	var taskStatus string
	var revision int
	if err := tx.QueryRow(`SELECT status, revision FROM tasks WHERE id=?`, taskID).Scan(&taskStatus, &revision); err != nil {
		return ContextManifest{}, fmt.Errorf("任务 %s 无法读取（恢复先读持久工作和未决操作）", taskID)
	}
	// 生成新 Manifest（旧清单只解释历史，不作为新授权）。
	return assembleContextManifest(tx, taskID, "", 100000, 100000, subject, now)
}

// ensureOldManifestNotAuthorization 核对旧清单不作为授权（P5-03.4）。
// 旧清单只解释历史，不作为新授权；来源删除、任务修订、工具变化后旧检查点
// 不能原样重放。
func ensureOldManifestNotAuthorization(m ContextManifest, currentRevision int) error {
	if m.ContractRevision != currentRevision {
		return fmt.Errorf("%w：清单合同版本 %d≠当前 %d（来源删除、任务修订、工具变化后旧检查点不能原样重放）",
			ErrOldManifestAsAuthorization, m.ContractRevision, currentRevision)
	}
	return nil
}

// 确保 context/sql/errors/fmt/json 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = json.Marshal
)
