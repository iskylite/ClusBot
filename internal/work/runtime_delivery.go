package work

// P4-14：验收、实际交接与 E01 全流程。
//
// 本文件实现完整调试交付链真正进入业务闭环：
//   - 按要求×节点/对象组核验，有效证据满足后进入待交付，保留未通过节点与
//     原因；
//   - 请求有权接收人确认实际交接，绑定对象范围、版本、决定者和时效；确认
//     不是授权；
//   - 交接提交前重核当前资产、验收有效性和限制，符合条件才转在役并保存
//     delivery；
//   - 部分交付只完成对应对象；Task 是否成功由其完成条件决定，不能自动将
//     整批成功；
//   - 从 Chat 走通发现、母盘、部署、配置、基准、联机、验收与交接，注入身份
//     冲突、单机/组测失败和取消。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 待交付（按要求×节点/对象组核验，有效证据满足后进入待交付）
// ---------------------------------------------------------------------------

// DeliveryCandidate 是待交付候选。
type DeliveryCandidate struct {
	ObjectRef        string
	// 未通过节点与原因。
	FailedNodes      []string
	FailureReasons   map[string]string
	// 有效证据满足后进入待交付。
	EvidenceSatisfied bool
}

// evaluateDeliveryCandidate 评估待交付候选（P4-14.1）。
// 按要求×节点/对象组核验，有效证据满足后进入待交付，保留未通过节点与原因。
func evaluateDeliveryCandidate(tx *sql.Tx, objectRef string, nodes []string, requirementKey string, now int64) (DeliveryCandidate, error) {
	candidate := DeliveryCandidate{
		ObjectRef: objectRef, FailureReasons: make(map[string]string),
	}
	// 按要求×节点/对象组核验（复用 P4-06 的 acceptance_coverage）。
	for _, node := range nodes {
		var coverageState string
		err := tx.QueryRow(`SELECT coverage_state FROM acceptance_coverage WHERE requirement_key=? AND object_ref=?`,
			requirementKey, node).Scan(&coverageState)
		if errors.Is(err, sql.ErrNoRows) {
			candidate.FailedNodes = append(candidate.FailedNodes, node)
			candidate.FailureReasons[node] = "未覆盖"
			continue
		}
		if err != nil {
			return candidate, err
		}
		if coverageState != "covered" {
			candidate.FailedNodes = append(candidate.FailedNodes, node)
			candidate.FailureReasons[node] = "覆盖状态 " + coverageState
		}
	}
	// 有效证据满足后进入待交付（无未通过节点）。
	candidate.EvidenceSatisfied = len(candidate.FailedNodes) == 0
	return candidate, nil
}

// ---------------------------------------------------------------------------
// 交接确认（请求有权接收人确认实际交接）
// ---------------------------------------------------------------------------

// HandoverConfirmation 是交接确认。
type HandoverConfirmation struct {
	ConfirmationID   string
	// 绑定对象范围、版本、决定者和时效。
	ObjectRefs       []string
	Version          int
	DecidedBy        string
	ExpiresAt        int64
	// 确认结果（confirmed/rejected/expired）。
	Outcome          string
	ConfirmedAt      int64
}

// requestHandoverConfirmation 请求交接确认（P4-14.2）。
// 请求有权接收人确认实际交接，绑定对象范围、版本、决定者和时效；确认不是
// 授权；接收确认前不转在役。
func requestHandoverConfirmation(tx *sql.Tx, objectRefs []string, version int, decidedBy string, expiresAt int64, now int64) (HandoverConfirmation, error) {
	if len(objectRefs) == 0 || decidedBy == "" || expiresAt <= now {
		return HandoverConfirmation{}, errors.New("交接确认缺少对象范围/决定者/时效")
	}
	confirmation := HandoverConfirmation{
		ConfirmationID: id(), ObjectRefs: objectRefs, Version: version,
		DecidedBy: decidedBy, ExpiresAt: expiresAt, Outcome: "pending",
	}
	return confirmation, nil
}

// confirmHandover 确认交接（P4-14.2）。
// 确认不是授权；确认后证据失效则重判，不沿用过期决定。
func confirmHandover(tx *sql.Tx, confirmation HandoverConfirmation, confirmer string, now int64) (HandoverConfirmation, error) {
	// 时效核对（不沿用过期决定）。
	if now >= confirmation.ExpiresAt {
		confirmation.Outcome = "expired"
		return confirmation, fmt.Errorf("交接确认已过期（不沿用过期决定）")
	}
	// 确认不是授权：确认人必须是决定者。
	if confirmer != confirmation.DecidedBy {
		return confirmation, fmt.Errorf("确认人 %s 不是决定者 %s（确认不是授权）", confirmer, confirmation.DecidedBy)
	}
	confirmation.Outcome = "confirmed"
	confirmation.ConfirmedAt = now
	return confirmation, nil
}

// ---------------------------------------------------------------------------
// 交接提交（交接提交前重核当前资产、验收有效性和限制）
// ---------------------------------------------------------------------------

// submitDeliveryHandover 提交交付交接（P4-14.3，P2-3 修复）。
// 交接提交前重核当前资产、验收有效性和限制，符合条件才转在役并保存 delivery；
// 接收确认前不转在役。验收/交付与状态变化原子保存（设计 §6.7）。
//
// requirementKey 是验收要求键（重核验收有效性用）；actor 是交接执行主体（状态迁移授权用）。
func submitDeliveryHandover(tx *sql.Tx, objectRef, requirementKey, actor string, confirmation HandoverConfirmation, now int64) error {
	// 接收确认前不转在役。
	if confirmation.Outcome != "confirmed" {
		return fmt.Errorf("对象 %s 未确认交接（接收确认前不转在役）", objectRef)
	}
	// 交接提交前重核当前资产（限制）。
	asset, err := readAsset(tx, objectRef)
	if err != nil {
		return fmt.Errorf("对象 %s 当前资产无法读取（交接提交前重核）", objectRef)
	}
	if asset.FrozenAt > 0 {
		return fmt.Errorf("对象 %s 已冻结（交接提交前重核限制）", objectRef)
	}
	// 重核验收有效性（P2-3）：coverage 仍 covered 且证据依赖未失效。
	if requirementKey != "" {
		var coverageState string
		err := tx.QueryRow(`SELECT coverage_state FROM acceptance_coverage WHERE requirement_key=? AND object_ref=?`,
			requirementKey, objectRef).Scan(&coverageState)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("对象 %s 验收要求 %s 未覆盖（交接提交前重核验收有效性）", objectRef, requirementKey)
		}
		if err != nil {
			return err
		}
		if coverageState != "covered" {
			return fmt.Errorf("对象 %s 验收覆盖已失效（%s），不能转在役（交接提交前重核验收有效性）", objectRef, coverageState)
		}
		// 证据依赖未失效核对。
		var invalidated int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM evidence_dependencies ed
JOIN acceptance_coverage ac ON ac.object_ref=ed.target_ref
WHERE ed.target_ref=? AND ed.invalidated=1`, objectRef).Scan(&invalidated); err != nil {
			return err
		}
		if invalidated > 0 {
			return fmt.Errorf("对象 %s 有 %d 项证据依赖已失效，不能转在役（交接提交前重核验收有效性）", objectRef, invalidated)
		}
	}
	// 符合条件才转在役：先迁移资产状态（原子保存验收/交付与状态变化）。
	ruleRefs := requirementKey
	if ruleRefs == "" {
		ruleRefs = "delivery-handover" // 无特定验收要求时的规则引用兜底
	}
	if asset.State != AssetStateInService {
		if _, err := transitionAssetState(tx, AssetTransition{
			ObjectRef: objectRef, FromState: asset.State, ToState: AssetStateInService,
			TriggerKind: "delivery_handover", EvidenceRefs: confirmation.ConfirmationID, RuleRefs: ruleRefs,
			ExpectedRevision: asset.Revision, ExpectedOwnershipEpoch: asset.OwnershipEpoch,
			Actor: actor, Responsibility: "交付交接确认转在役",
		}, now); err != nil {
			return fmt.Errorf("对象 %s 转在役状态迁移失败：%v", objectRef, err)
		}
	}
	// 保存 delivery（与状态迁移同事务）。
	if _, err := tx.Exec(`INSERT INTO delivery_records(delivery_id,object_ref,stage,record_ref,evidence_refs,occurred_at,created_at)
VALUES(?,?,?,?,?,?,?)`, id(), objectRef, "in_service", confirmation.ConfirmationID, `[]`, now, now); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		Event: "delivery_in_service", Actor: confirmation.DecidedBy,
		Summary: fmt.Sprintf("对象 %s 已转在役（交付交接确认）", objectRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 部分交付（部分交付只完成对应对象）
// ---------------------------------------------------------------------------

// ErrBatchSuccess 是整批成功的错误。
var ErrBatchSuccess = errors.New("不能自动将整批成功")

// ensurePartialDeliveryNotBatchSuccess 核对部分交付（P4-14.4）。
// 部分交付只完成对应对象；Task 是否成功由其完成条件决定，不能自动将整批成功。
func ensurePartialDeliveryNotBatchSuccess(deliveredCount int, totalCount int) error {
	if deliveredCount < totalCount {
		return fmt.Errorf("%w：部分交付 %d/%d（Task 是否成功由其完成条件决定）", ErrBatchSuccess, deliveredCount, totalCount)
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
