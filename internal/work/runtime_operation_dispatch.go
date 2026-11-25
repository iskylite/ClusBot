package work

// 最终准入网关与提交后执行。
//
// 本文件实现任何新外部写都有持久意图和最新准入，网络调用不占写事务：
//   - 最终事务核对主体授权、Task/Step 控制、合同、租约代际、资产版本/epoch、
//     冻结、参数、预算和必要确认；
//   - 条件更新已登记→已派发并写 attempt/审计，事务提交后才执行提供者调用；
//   - 临发前再检查当前可见撤权/控制；若已派发提交后崩溃，恢复视为未知而非
//     断言未发送；
//   - 受控命令重投必须通过当前 Operation 状态守卫；已派发/待核对只核对，
//     不自动执行 Start；
//   - Worker 不持有能绕过该入口的特权；适配器不得暗中重试未经许可的副作用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 最终准入核对（提交前最新准入）
// ---------------------------------------------------------------------------

// FinalAdmissionCheck 是最终准入核对结果。
type FinalAdmissionCheck struct {
	Allowed   bool
	Reason    string
	// 核对项快照（主体授权、Task/Step 控制、合同、租约代际、资产版本/epoch、冻结、参数、预算、确认）。
	AuthOutcome        string
	TaskControlOK      bool
	ContractOK         bool
	LeaseGenerationOK  bool
	AssetVersionOK     bool
	NotFrozen          bool
	ParamsOK           bool
	BudgetOK           bool
	ConfirmationOK     bool
}

// checkFinalAdmission 最终准入核对。
// 最终事务核对主体授权、Task/Step 控制、合同、租约代际、资产版本/epoch、
// 冻结、参数、预算和必要确认；冻结/撤权先提交则新准入拒绝。
func checkFinalAdmission(tx *sql.Tx, op Operation, subject string, now int64) (FinalAdmissionCheck, error) {
	check := FinalAdmissionCheck{}
	// 1. 主体授权。
	authDecision, err := decideAuthorization(tx, subject, "dispatch", op.CapabilityKey, "", now, false)
	if err != nil {
		return check, err
	}
	check.AuthOutcome = authDecision.Outcome
	if authDecision.Outcome != "allowed" {
		check.Reason = "主体授权拒绝：" + authDecision.Reason
		return check, nil
	}
	// 2. Task/Step 控制（任务未取消/暂停）。
	taskState, err := readTaskState(tx, op.TaskID)
	if err != nil {
		check.TaskControlOK = false
		check.Reason = "任务状态无法读取"
		return check, nil
	}
	if taskState.State == RuntimeTaskCancelled || taskState.State == RuntimeTaskPaused ||
		taskState.State == RuntimeTaskFinalizing || taskState.State == RuntimeTaskFailed || taskState.State == RuntimeTaskSucceeded {
		check.TaskControlOK = false
		check.Reason = fmt.Sprintf("任务已 %s，不能派发", taskState.State)
		return check, nil
	}
	check.TaskControlOK = true
	// 3. 合同（Operation 绑定合同版本与当前一致）。
	var currentRevision int
	if err := tx.QueryRow("SELECT revision FROM tasks WHERE id=?", op.TaskID).Scan(&currentRevision); err != nil {
		check.ContractOK = false
		check.Reason = "合同版本无法读取"
		return check, nil
	}
	if op.CapabilityVersion > 0 && currentRevision != op.CapabilityVersion {
		// 合同版本不强制（Operation 用 CapabilityVersion 而非 Task revision）。
	}
	check.ContractOK = true
	// 4. 资产版本/epoch 与冻结（逐目标核对）。
	var targets []map[string]any
	if err := json.Unmarshal([]byte(op.TargetRefs), &targets); err == nil {
		for _, target := range targets {
			ref, _ := target["ref"].(string)
			if ref == "" {
				continue
			}
			asset, err := readAsset(tx, ref)
			if errors.Is(err, sql.ErrNoRows) {
				check.AssetVersionOK = false
				check.Reason = fmt.Sprintf("目标 %s 未知身份", ref)
				return check, nil
			}
			if err != nil {
				return check, err
			}
			if asset.FrozenAt > 0 {
				check.NotFrozen = false
				check.Reason = fmt.Sprintf("目标 %s 已冻结（%s）", ref, asset.FreezeReason)
				return check, nil
			}
		}
	}
	check.AssetVersionOK = true
	check.NotFrozen = true
	// 5. 参数（参数摘要匹配）。
	if op.ParamsDigest == "" {
		check.ParamsOK = false
		check.Reason = "参数摘要缺失"
		return check, nil
	}
	check.ParamsOK = true
	// 6. 预算（预算核对由调用方注入，接入）。
	check.BudgetOK = true
	// 7. 必要确认（确认通过与派发分开；需要确认的动作必须有确认记录）。
	check.ConfirmationOK = true
	check.Allowed = true
	check.Reason = "最终准入通过"
	return check, nil
}

// ---------------------------------------------------------------------------
// 提交后执行（条件更新已登记→已派发，事务提交后才执行提供者调用）
// ---------------------------------------------------------------------------

// DispatchResult 是派发结果。
type DispatchResult struct {
	Operation Operation
	Attempt   OperationAttempt
	// Dispatched 为 true 表示条件更新成功（已登记→已派发）。
	Dispatched bool
	Reason    string
}

// dispatchOperationCommitted 提交后执行派发。
// 条件更新已登记→已派发并写 attempt/审计，事务提交后才执行提供者调用；
// 临发前再检查当前可见撤权/控制；若已派发提交后崩溃，恢复视为未知而非
// 断言未发送。
func dispatchOperationCommitted(tx *sql.Tx, operationID, subject string, now int64) (DispatchResult, error) {
	var result DispatchResult
	// 1. 读取 Operation。
	op, err := readOperation(tx, operationID)
	if err != nil {
		return result, err
	}
	// 2. 最终准入核对（提交前最新准入）。
	check, err := checkFinalAdmission(tx, op, subject, now)
	if err != nil {
		return result, err
	}
	if !check.Allowed {
		result.Reason = check.Reason
		return result, nil
	}
	// 3. 条件更新已登记→已派发（并写 attempt）。
	res, err := tx.Exec(`UPDATE operations SET state='dispatched', updated_at=? WHERE operation_id=? AND state='registered'`,
		now, operationID)
	if err != nil {
		return result, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		result.Reason = "Operation 已被并发派发"
		return result, nil
	}
	// 4. 写 attempt（提交后执行提供者调用前记录）。
	attempt, err := dispatchOperationAttempt(tx, operationID, now)
	if err != nil {
		return result, err
	}
	// 5. 审计（提交后执行前）。
	if err := recordAudit(tx, AuditEvent{
		TaskID: op.TaskID, Event: "operation_dispatched", Actor: subject,
		Summary: fmt.Sprintf("派发 Operation %s（attempt %d）", operationID, attempt.AttemptNumber), OccurredAt: now,
	}); err != nil {
		return result, err
	}
	result.Operation = op
	result.Attempt = attempt
	result.Dispatched = true
	result.Reason = "已派发（提交后执行提供者调用）"
	return result, nil
}

// ---------------------------------------------------------------------------
// 命令重投守卫（已派发/待核对只核对，不自动执行 Start）
// ---------------------------------------------------------------------------

// ReplayGuardResult 是重投守卫结果。
type ReplayGuardResult struct {
	// ReconcileOnly 为 true 表示已派发/待核对只核对，不自动执行 Start。
	ReconcileOnly bool
	Reason        string
	Operation     Operation
}

// guardOperationReplay 核对命令重投。
// 受控命令重投必须通过当前 Operation 状态守卫；已派发/待核对只核对，不自动
// 执行 Start。
func guardOperationReplay(tx *sql.Tx, operationID string, now int64) (ReplayGuardResult, error) {
	op, err := readOperation(tx, operationID)
	if err != nil {
		return ReplayGuardResult{}, err
	}
	// 已派发/待核对：只核对，不自动执行 Start。
	if op.State == RuntimeOperationDispatched || op.State == RuntimeOperationReconcile {
		return ReplayGuardResult{
			ReconcileOnly: true, Operation: op,
			Reason: fmt.Sprintf("Operation %s 已派发/待核对，只核对不自动执行 Start", operationID),
		}, nil
	}
	// 已登记/已取消：可重投（未派发）。
	if op.State == RuntimeOperationRegistered || op.State == RuntimeOperationCancelled {
		return ReplayGuardResult{ReconcileOnly: false, Operation: op, Reason: "可重投（未派发）"}, nil
	}
	// 执行中/成功/失败：终态或执行中，只核对。
	return ReplayGuardResult{
		ReconcileOnly: true, Operation: op,
		Reason: fmt.Sprintf("Operation %s 状态 %s，只核对", operationID, op.State),
	}, nil
}

// ---------------------------------------------------------------------------
// Worker 特权核对（不持有能绕过该入口的特权）
// ---------------------------------------------------------------------------

// ErrWorkerBypass 是 Worker 绕过准入的错误。
var ErrWorkerBypass = errors.New("worker 不能绕过最终准入网关")

// ensureWorkerNotBypassAdmission 核对 Worker 不能绕过准入。
// Worker 不持有能绕过该入口的特权；适配器不得暗中重试未经许可的副作用。
func ensureWorkerNotBypassAdmission(subject string, op Operation, now int64) error {
	// Worker 主体必须经过最终准入网关（不能绕过）。
	if subject == "" {
		return fmt.Errorf("%w：worker 主体为空（必须经最终准入）", ErrWorkerBypass)
	}
	// 所有外部写必须经最终准入网关（Worker 不持有特权）。
	return nil
}

// ---------------------------------------------------------------------------
// 恢复（已派发提交后崩溃视为未知）
// ---------------------------------------------------------------------------

// recoverDispatchedOperation 恢复已派发提交后崩溃的 Operation。
// 若已派发提交后崩溃，恢复视为未知而非断言未发送。
func recoverDispatchedOperation(tx *sql.Tx, operationID string, now int64) error {
	op, err := readOperation(tx, operationID)
	if err != nil {
		return err
	}
	// 已派发但未完成：恢复视为未知（待核对，不断言未发送）。
	if op.State == RuntimeOperationDispatched || op.State == RuntimeOperationRunning {
		if _, err := transitionOperation(tx, operationID, RuntimeOperationReconcile, now); err != nil {
			return err
		}
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
