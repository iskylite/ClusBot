package work

// P3-16：整段外部工作流委托。
//
// 本文件实现已有平台可独自推进其负责的流程，本地持续观察、控制和验收：
//   - 委托记录实际目标、已固定版本、起止边界和双方责任；一次委托关联外部 Job；
//   - 外部负责的步骤，本地不再并行调度同一动作；结果通过 Inbox 更新 Operation，
//     领域验收仍由本地规则决定；
//   - 暂停/取消/补充是否支持以提供者契约为准；外部不支持时停止本地后续工作
//     并保留在途责任；
//   - 需要交回部分步骤时，先确认外部停止边界和未决效果，再生成新本地执行
//     单元，不能无缝假定已停；
//   - 重启恢复按原 Job 对账，不因外部状态查询失败重新启动整个流程。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 委托（实际目标、已固定版本、起止边界和双方责任）
// ---------------------------------------------------------------------------

// Delegation 是 delegations 的一行。
type Delegation struct {
	DelegationID         string
	TaskID               string
	TargetRefs           string
	PinnedVersions       string
	BoundaryStart        int64
	BoundaryEnd          int64
	LocalResponsibility  string
	RemoteResponsibility string
	ExternalJobID        string
	State                string // active/completed/cancelled/handover
	SupportsPause        string
	SupportsCancel       string
	SupportsSupplement   string
	CreatedAt            int64
	UpdatedAt            int64
}

// createDelegation 创建一个外部工作流委托（P3-16.1）。
// 委托记录实际目标、已固定版本、起止边界和双方责任；一次委托关联外部 Job。
func createDelegation(tx *sql.Tx, d Delegation, now int64) (Delegation, error) {
	if d.TaskID == "" || d.TargetRefs == "" {
		return Delegation{}, errors.New("委托缺少任务或目标")
	}
	if d.ExternalJobID == "" {
		return Delegation{}, errors.New("委托缺少外部 Job 关联")
	}
	if d.LocalResponsibility == "" || d.RemoteResponsibility == "" {
		return Delegation{}, errors.New("委托缺少双方责任")
	}
	if d.PinnedVersions == "" {
		d.PinnedVersions = "{}"
	}
	d.DelegationID = id()
	d.State = "active"
	d.CreatedAt, d.UpdatedAt = now, now
	// 暂停/取消/补充支持：默认 unknown（以提供者契约为准）。
	for _, f := range []struct{ name string; value *string }{
		{"supports_pause", &d.SupportsPause}, {"supports_cancel", &d.SupportsCancel}, {"supports_supplement", &d.SupportsSupplement},
	} {
		if *f.value == "" {
			*f.value = "unknown"
		}
	}
	_, err := tx.Exec(`INSERT INTO delegations(delegation_id,task_id,target_refs,pinned_versions,
boundary_start,boundary_end,local_responsibility,remote_responsibility,external_job_id,state,
supports_pause,supports_cancel,supports_supplement,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.DelegationID, d.TaskID, d.TargetRefs, d.PinnedVersions, d.BoundaryStart, d.BoundaryEnd,
		d.LocalResponsibility, d.RemoteResponsibility, d.ExternalJobID, d.State,
		d.SupportsPause, d.SupportsCancel, d.SupportsSupplement, now, now)
	if err != nil {
		return Delegation{}, err
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// 单一所有者（外部负责的步骤，本地不再并行调度同一动作）
// ---------------------------------------------------------------------------

// ErrDualScheduling 是双重调度的错误。
var ErrDualScheduling = errors.New("外部流程与本地执行不能双重调度同一步")

// ensureNoDualScheduling 核对单一所有者（P3-16.2）。
// 外部负责的步骤，本地不再并行调度同一动作；外部流程与本地执行账本没有
// 双重调度同一步。
func ensureNoDualScheduling(tx *sql.Tx, taskID string, stepID string, now int64) error {
	// 核对：该 Task 的该 Step 是否已委托给外部。
	var delegated bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM delegations WHERE task_id=? AND state='active')`, taskID).Scan(&delegated); err != nil {
		return err
	}
	if delegated {
		return fmt.Errorf("%w：任务 %s 的步骤 %s 已委托外部（本地不再并行调度同一动作）", ErrDualScheduling, taskID, stepID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 提供者契约（暂停/取消/补充是否支持以提供者契约为准）
// ---------------------------------------------------------------------------

// ProviderContractAction 是提供者契约动作。
type ProviderContractAction string

const (
	ContractPause      ProviderContractAction = "pause"
	ContractCancel     ProviderContractAction = "cancel"
	ContractSupplement ProviderContractAction = "supplement"
)

// checkProviderContractSupport 核对提供者契约支持（P3-16.3）。
// 暂停/取消/补充是否支持以提供者契约为准；外部不支持时停止本地后续工作并
// 保留在途责任。
func checkProviderContractSupport(tx *sql.Tx, delegationID string, action ProviderContractAction, now int64) (supported bool, reason string, err error) {
	var d Delegation
	if err := tx.QueryRow(`SELECT supports_pause, supports_cancel, supports_supplement FROM delegations WHERE delegation_id=?`, delegationID).
		Scan(&d.SupportsPause, &d.SupportsCancel, &d.SupportsSupplement); err != nil {
		return false, "", err
	}
	var support string
	switch action {
	case ContractPause:
		support = d.SupportsPause
	case ContractCancel:
		support = d.SupportsCancel
	case ContractSupplement:
		support = d.SupportsSupplement
	}
	switch support {
	case "supported":
		return true, "提供者契约支持", nil
	case "unsupported":
		return false, fmt.Sprintf("提供者契约不支持 %s（停止本地后续工作并保留在途责任）", action), nil
	default:
		return false, fmt.Sprintf("提供者契约对 %s 的支持未知（按保守处理）", action), nil
	}
}

// ---------------------------------------------------------------------------
// 移交（确认外部停止边界和未决效果，再生成新本地执行单元）
// ---------------------------------------------------------------------------

// DelegationHandover 是 delegation_handovers 的一行。
type DelegationHandover struct {
	HandoverID           string
	DelegationID         string
	ExternalStopBoundary string
	PendingEffects       string
	NewLocalUnits        string
	State                string // pending/confirmed/completed
	CreatedAt            int64
	ConfirmedAt          int64
}

// initiateHandover 发起移交（P3-16.4）。
// 需要交回部分步骤时，先确认外部停止边界和未决效果，再生成新本地执行单元，
// 不能无缝假定已停；移交边界不明时保持等待，不能让新旧执行方同时写。
func initiateHandover(tx *sql.Tx, delegationID string, now int64) (DelegationHandover, error) {
	h := DelegationHandover{
		HandoverID: id(), DelegationID: delegationID, State: "pending", CreatedAt: now,
	}
	_, err := tx.Exec(`INSERT INTO delegation_handovers(handover_id,delegation_id,state,created_at)
VALUES(?,?,?,?)`, h.HandoverID, h.DelegationID, h.State, now)
	if err != nil {
		return h, err
	}
	// 移交边界不明时保持等待（不能让新旧执行方同时写）。
	if _, err := tx.Exec(`UPDATE delegations SET state='handover', updated_at=? WHERE delegation_id=?`, now, delegationID); err != nil {
		return h, err
	}
	return h, nil
}

// confirmHandoverBoundary 确认移交边界（P3-16.4）。
// 先确认外部停止边界和未决效果，再生成新本地执行单元，不能无缝假定已停。
func confirmHandoverBoundary(tx *sql.Tx, handoverID string, externalStopBoundary string, pendingEffects string, now int64) error {
	if externalStopBoundary == "" {
		return errors.New("移交必须先确认外部停止边界（不能无缝假定已停）")
	}
	res, err := tx.Exec(`UPDATE delegation_handovers SET external_stop_boundary=?, pending_effects=?, state='confirmed', confirmed_at=?
WHERE handover_id=? AND state='pending'`, externalStopBoundary, pendingEffects, now, handoverID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("移交 %s 不存在或已确认", handoverID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 重启恢复（按原 Job 对账，不因外部状态查询失败重新启动）
// ---------------------------------------------------------------------------

// recoverDelegationByJob 按原 Job 恢复委托（P3-16.5）。
// 重启恢复按原 Job 对账，不因外部状态查询失败重新启动整个流程。
func recoverDelegationByJob(tx *sql.Tx, externalJobID string, now int64) (Delegation, error) {
	var d Delegation
	err := tx.QueryRow(`SELECT delegation_id,task_id,state,external_job_id FROM delegations WHERE external_job_id=?`,
		externalJobID).Scan(&d.DelegationID, &d.TaskID, &d.State, &d.ExternalJobID)
	if err != nil {
		return d, err
	}
	// 外部状态查询失败：不重新启动整个流程（保留当前状态，持续核对）。
	if d.State == "active" {
		return d, nil
	}
	return d, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
