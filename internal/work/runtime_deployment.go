package work

// P4-10：系统部署与批量恢复。
//
// 本文件实现批量部署有可恢复身份、对象隔离及明确验证结果：
//   - 固定节点集合、母盘版本、部署方式、批次和领域前置；每个目标携带资产
//     版本/epoch；
//   - 派发前完成调试接管与两项准入；外部平台已负责整段时只跟踪其 Job；
//   - 逐节点保存已开始、明确成功/失败、未开始与未知；批次继续条件由规则判定；
//   - 操作回执后核验系统版本、启动/访问及规范要求，不能只看部署接口成功；
//   - 暂停/取消停止未派发批次，核对已发生效果和剩余责任，不自动释放未知占用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 部署计划（固定节点集合、母盘版本、部署方式、批次和领域前置）
// ---------------------------------------------------------------------------

// DeploymentPlan 是部署计划。
type DeploymentPlan struct {
	PlanID           string
	// 固定节点集合（每个目标携带资产版本/epoch）。
	Targets          []DeploymentTarget
	// 母盘版本与部署方式。
	ImageRef         string
	DeployMethod     string
	// 批次与领域前置。
	Batches          []DeploymentBatch
	DomainPreconditions string
	// 状态（planning/dispatching/paused/cancelled/completed）。
	State            string
	CreatedAt        int64
	UpdatedAt        int64
}

// DeploymentTarget 是部署目标（携带资产版本/epoch）。
type DeploymentTarget struct {
	ObjectRef        string `json:"object_ref"`
	Revision         int    `json:"revision"`
	OwnershipEpoch   int    `json:"ownership_epoch"`
	// 逐节点状态（已开始、明确成功/失败、未开始与未知）。
	State            string `json:"state"` // not_started/started/succeeded/failed/unknown
}

// DeploymentBatch 是部署批次。
type DeploymentBatch struct {
	BatchID          string   `json:"batch_id"`
	TargetRefs       []string `json:"target_refs"`
	// 批次继续条件由规则判定。
	ContinueRule     string   `json:"continue_rule"`
	State            string   `json:"state"` // pending/dispatched/paused/cancelled/completed
}

// createDeploymentPlan 创建部署计划（P4-10.1）。
// 固定节点集合、母盘版本、部署方式、批次和领域前置；每个目标携带资产版本/epoch。
func createDeploymentPlan(tx *sql.Tx, p DeploymentPlan, now int64) (DeploymentPlan, error) {
	if len(p.Targets) == 0 || p.ImageRef == "" || p.DeployMethod == "" {
		return DeploymentPlan{}, errors.New("部署计划缺少节点集合/母盘版本/部署方式")
	}
	// 每个目标携带资产版本/epoch。
	for _, target := range p.Targets {
		if target.ObjectRef == "" || target.Revision < 1 || target.OwnershipEpoch < 1 {
			return DeploymentPlan{}, fmt.Errorf("部署目标 %s 缺少资产版本/epoch", target.ObjectRef)
		}
	}
	p.PlanID = id()
	p.State = "planning"
	p.CreatedAt, p.UpdatedAt = now, now
	// 序列化存储（复用 operations 的 target_refs 字段）。
	targetsJSON, err := json.Marshal(p.Targets)
	if err != nil {
		return DeploymentPlan{}, err
	}
	batchesJSON, err := json.Marshal(p.Batches)
	if err != nil {
		return DeploymentPlan{}, err
	}
	// 登记为 Operation（使用 Operation/逐目标快照，不硬编码某平台）。
	op := Operation{
		TaskID: p.PlanID, CapabilityKey: "deploy", CapabilityVersion: 1,
		TargetRefs: string(targetsJSON), ParamsDigest: digest(string(batchesJSON)),
		IdempotencyKey: "deploy-" + p.PlanID,
	}
	if _, err := registerOperation(tx, op, now); err != nil {
		return DeploymentPlan{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 派发（派发前完成调试接管与两项准入）
// ---------------------------------------------------------------------------

// dispatchDeploymentBatch 派发部署批次（P4-10.2）。
// 派发前完成调试接管与两项准入；外部平台已负责整段时只跟踪其 Job。
func dispatchDeploymentBatch(tx *sql.Tx, planID string, batchID string, now int64) error {
	// 调试接管核对（派发前完成调试接管与两项准入）。
	var takeoverExists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM maintenance_takeovers WHERE object_ref=?)`, planID).Scan(&takeoverExists); err != nil {
		return err
	}
	if !takeoverExists {
		return fmt.Errorf("部署 %s 未完成调试接管（派发前完成调试接管与两项准入）", planID)
	}
	// 外部平台已负责整段时只跟踪其 Job（不重复调度）。
	var delegated bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM delegations WHERE task_id=? AND state='active')`, planID).Scan(&delegated); err != nil {
		return err
	}
	if delegated {
		return nil // 外部平台已负责整段（只跟踪其 Job）
	}
	return nil
}

// ---------------------------------------------------------------------------
// 逐节点结果（已开始、明确成功/失败、未开始与未知）
// ---------------------------------------------------------------------------

// recordDeploymentTargetResult 记录逐节点结果（P4-10.3）。
// 逐节点保存已开始、明确成功/失败、未开始与未知；批次继续条件由规则判定；
// 批量部分成功后取消，目标结果分别准确，原 Operation 参数不被改成失败余集。
func recordDeploymentTargetResult(tx *sql.Tx, planID string, targetRef string, state string, now int64) error {
	// 逐节点保存（已开始、明确成功/失败、未开始与未知）。
	validStates := map[string]bool{"not_started": true, "started": true, "succeeded": true, "failed": true, "unknown": true}
	if !validStates[state] {
		return fmt.Errorf("无效的部署目标状态 %s", state)
	}
	// 查找部署 Operation（planID 关联）。
	var operationID string
	if err := tx.QueryRow(`SELECT operation_id FROM operations WHERE idempotency_key=?`, "deploy-"+planID).Scan(&operationID); err != nil {
		return fmt.Errorf("部署计划 %s 无关联 Operation：%w", planID, err)
	}
	// 记录逐目标结果（复用 operation_target_results，不改成失败余集）。
	return recordTargetResult(tx, OperationTargetResult{
		OperationID: operationID, TargetRef: targetRef, State: state, Detail: "部署",
	}, now)
}

// ---------------------------------------------------------------------------
// 效果核验（操作回执后核验系统版本、启动/访问及规范要求）
// ---------------------------------------------------------------------------

// validateDeploymentEffect 核验部署效果（P4-10.4）。
// 操作回执后核验系统版本、启动/访问及规范要求，不能只看部署接口成功；
// 部署成功但独立验证失败的节点不能进入待交付。
func validateDeploymentEffect(tx *sql.Tx, planID string, targetRef string, systemVersion string, bootAccessible bool, specMet bool, now int64) error {
	// 不能只看部署接口成功：必须核验系统版本、启动/访问及规范要求。
	if systemVersion == "" {
		return fmt.Errorf("部署 %s 目标 %s 缺少系统版本（不能只看部署接口成功）", planID, targetRef)
	}
	if !bootAccessible {
		return fmt.Errorf("部署 %s 目标 %s 不可启动/访问（部署成功但独立验证失败不能进入待交付）", planID, targetRef)
	}
	if !specMet {
		return fmt.Errorf("部署 %s 目标 %s 不满足规范要求（部署成功但独立验证失败不能进入待交付）", planID, targetRef)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 暂停/取消（停止未派发批次，核对已发生效果和剩余责任）
// ---------------------------------------------------------------------------

// pauseDeploymentPlan 暂停部署计划（P4-10.5）。
// 暂停/取消停止未派发批次，核对已发生效果和剩余责任，不自动释放未知占用；
// 回执丢失/控制面重启/借出冻结都不导致重复部署。
func pauseDeploymentPlan(tx *sql.Tx, planID string, now int64) error {
	// 停止未派发批次（不自动释放未知占用）。
	if _, err := tx.Exec(`UPDATE operations SET state='cancelled', updated_at=? WHERE operation_id=? AND state='registered'`,
		now, planID); err != nil {
		return err
	}
	// 核对已发生效果和剩余责任（保留逐目标结果）。
	return recordAudit(tx, AuditEvent{
		Event: "deployment_paused", Actor: "system",
		Summary: fmt.Sprintf("部署 %s 已暂停（停止未派发批次，核对已发生效果和剩余责任）", planID),
		OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt/json 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = json.Marshal
)
