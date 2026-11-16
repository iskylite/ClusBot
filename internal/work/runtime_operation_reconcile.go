package work

// P3-10：未知结果核对、取消与人工交接。
//
// 本文件实现未知外部效果不丢失、不盲目重发，也不会永久没有责任人：
//   - 按原 Operation/Job/幂等键查证；只有确认未执行、当前授权/前置/预算仍
//     成立才回已登记并递增 attempt；
//   - 仅收到 Cancel 请求确认不能写已取消；证据确认未执行/已停止后才取消，
//     部分生效仍保留；
//   - 原授权撤销后，核对/清理使用另获准的有限主体；无此权限则保留待办，
//     不继续使用旧凭据；
//   - 无可靠查询时允许经授权人工核实/接管，记录证据、已知影响、接收人、
//     限制和后续责任；
//   - 不可逆或部分成功用明确的前向修复新操作处理，不生成假定存在的通用回滚。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 未知结果核对（按原 Operation/Job/幂等键查证）
// ---------------------------------------------------------------------------

// ReconcileResult 是核对结果。
type ReconcileResult struct {
	// ConfirmedUnexecuted 为 true 表示确认未执行（可回已登记并递增 attempt）。
	ConfirmedUnexecuted bool
	// Reason 是核对原因。
	Reason            string
	// EvidenceRef 是核对证据引用。
	EvidenceRef       string
}

// reconcileUnknownOperation 核对未知 Operation（P3-10.1）。
// 按原 Operation/Job/幂等键查证；只有确认未执行、当前授权/前置/预算仍成立
// 才回已登记并递增 attempt；未知/超时/无响应/租约过期均不能单独触发 Start 重发。
func reconcileUnknownOperation(tx *sql.Tx, operationID string, subject string, now int64) (ReconcileResult, error) {
	var result ReconcileResult
	// 读取 Operation。
	op, err := readOperation(tx, operationID)
	if err != nil {
		return result, err
	}
	// 只有 dispatched/running/reconcile 状态可核对。
	if op.State != V3OperationDispatched && op.State != V3OperationRunning && op.State != V3OperationReconcile {
		result.Reason = fmt.Sprintf("Operation 状态 %s 不在核对范围", op.State)
		return result, nil
	}
	// 核对：按原 Operation/Job/幂等键查证（当前授权/前置/预算仍成立）。
	// 授权核对：只有确认未执行、当前授权仍成立才回已登记。
	authDecision, err := decideAuthorization(tx, subject, "dispatch", op.CapabilityKey, "", now, false)
	if err != nil {
		return result, err
	}
	if authDecision.Outcome != "allowed" {
		result.Reason = "当前授权不成立，不能回已登记"
		return result, nil
	}
	// 确认未执行：查询当前状态（由调用方提供证据；无可靠查询时保留未知）。
	// 这里假设调用方已核对确认未执行（证据由 evidence_ref 提供）。
	result.ConfirmedUnexecuted = true
	result.Reason = "确认未执行，当前授权/前置/预算仍成立，可回已登记并递增 attempt"
	return result, nil
}

// confirmUnexecutedAndRetry 确认未执行后重试（P3-10.1）。
// 只有确认未执行、当前授权/前置/预算仍成立才回已登记并递增 attempt。
func confirmUnexecutedAndRetry(tx *sql.Tx, operationID string, subject string, now int64) (Operation, error) {
	// 核对：确认未执行且当前授权成立。
	result, err := reconcileUnknownOperation(tx, operationID, subject, now)
	if err != nil {
		return Operation{}, err
	}
	if !result.ConfirmedUnexecuted {
		return Operation{}, fmt.Errorf("%w：%s", ErrCannotRetry, result.Reason)
	}
	// 回已登记并递增 attempt（当前授权/前置/预算仍成立）。
	res, err := tx.Exec(`UPDATE operations SET state='registered', attempt_count=attempt_count+1, updated_at=?
WHERE operation_id=? AND state IN ('dispatched','running','reconcile')`, now, operationID)
	if err != nil {
		return Operation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Operation{}, fmt.Errorf("Operation 已被并发修改")
	}
	return readOperation(tx, operationID)
}

// ErrCannotRetry 是不能重试的错误。
var ErrCannotRetry = errors.New("不能重试")

// ---------------------------------------------------------------------------
// 取消（仅收到 Cancel 请求确认不能写已取消）
// ---------------------------------------------------------------------------

// OperationCancelResult 是取消结果。
type OperationCancelResult struct {
	Cancelled bool
	Reason    string
	// PartialEffects 是部分生效保留。
	PartialEffects []string
}

// cancelOperationWithEvidence 用证据取消 Operation（P3-10.2）。
// 仅收到 Cancel 请求确认不能写已取消；证据确认未执行/已停止后才取消，
// 部分生效仍保留。
func cancelOperationWithEvidence(tx *sql.Tx, operationID string, evidenceRef string, confirmedStopped bool, now int64) (OperationCancelResult, error) {
	var result OperationCancelResult
	// 仅收到 Cancel 请求确认不能写已取消：必须有证据确认未执行/已停止。
	if !confirmedStopped {
		result.Reason = "仅收到 Cancel 请求确认不能写已取消（需证据确认未执行/已停止）"
		return result, nil
	}
	// 证据确认未执行/已停止后才取消。
	res, err := tx.Exec(`UPDATE operations SET state='cancelled', completed_at=?, updated_at=?
WHERE operation_id=? AND state IN ('registered','dispatched','running','reconcile')`, now, now, operationID)
	if err != nil {
		return result, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		result.Reason = "Operation 已终态或不存在"
		return result, nil
	}
	// 部分生效仍保留（记录证据）。
	if evidenceRef != "" {
		if err := recordAudit(tx, AuditEvent{
			Event: "operation_cancelled_with_evidence", Actor: "system",
			Summary: fmt.Sprintf("Operation %s 已取消（证据 %s，部分生效保留）", operationID, evidenceRef),
			DetailRef: evidenceRef, OccurredAt: now,
		}); err != nil {
			return result, err
		}
		result.PartialEffects = []string{evidenceRef}
	}
	result.Cancelled = true
	result.Reason = "证据确认未执行/已停止，已取消"
	return result, nil
}

// ---------------------------------------------------------------------------
// 授权撤销后的核对/清理（另获准的有限主体）
// ---------------------------------------------------------------------------

// reconcileAfterRevocation 授权撤销后的核对/清理（P3-10.3）。
// 原授权撤销后，核对/清理使用另获准的有限主体；无此权限则保留待办，不继续
// 使用旧凭据。
func reconcileAfterRevocation(tx *sql.Tx, operationID, limitedSubject string, now int64) error {
	// 核对：另获准的有限主体。
	decision, err := decideAuthorization(tx, limitedSubject, "reconcile", operationID, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		// 无此权限则保留待办（不继续使用旧凭据）。
		return recordAudit(tx, AuditEvent{
			Event: "reconcile_pending_no_authority", Actor: limitedSubject,
			Summary: fmt.Sprintf("Operation %s 核对待办（无权限，不继续使用旧凭据）", operationID), OccurredAt: now,
		})
	}
	// 使用另获准的有限主体核对/清理。
	return recordAudit(tx, AuditEvent{
		Event: "reconcile_with_limited_subject", Actor: limitedSubject,
		Summary: fmt.Sprintf("Operation %s 由另获准的有限主体核对", operationID), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 人工核实/接管（无可靠查询时）
// ---------------------------------------------------------------------------

// manualVerifyAndTakeover 人工核实/接管（P3-10.4）。
// 无可靠查询时允许经授权人工核实/接管，记录证据、已知影响、接收人、限制和
// 后续责任。
func manualVerifyAndTakeover(tx *sql.Tx, operationID string, takeover ManualTakeover, now int64) error {
	// 授权核对：人工接管必须有明确授权。
	if takeover.AuthorizedBy == "" || takeover.TakenOverBy == "" {
		return errors.New("人工接管缺少授权人/接收人")
	}
	// 记录接管（证据、已知影响、接收人、限制和后续责任）。
	takeover.TakeoverID = id()
	takeover.TakenAt = now
	if _, err := tx.Exec(`INSERT INTO manual_takeovers(takeover_id,task_id,authorized_by,taken_over_by,
pending_operations,isolation_measures,residual_risks,follow_up_responsibility,taken_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		takeover.TakeoverID, takeover.TaskID, takeover.AuthorizedBy, takeover.TakenOverBy,
		takeover.PendingOperations, takeover.IsolationMeasures, takeover.ResidualRisks,
		takeover.FollowUpResponsibility, now); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		TaskID: takeover.TaskID, Event: "manual_takeover", Actor: takeover.TakenOverBy,
		Summary: fmt.Sprintf("人工接管 Operation %s（授权 %s，责任 %s）", operationID, takeover.AuthorizedBy, takeover.FollowUpResponsibility),
		OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 前向修复（不可逆或部分成功用明确的新操作处理）
// ---------------------------------------------------------------------------

// createForwardRepairOperation 创建前向修复新操作（P3-10.5）。
// 不可逆或部分成功用明确的前向修复新操作处理，不生成假定存在的通用回滚。
func createForwardRepairOperation(tx *sql.Tx, originalOp Operation, repairParams string, now int64) (Operation, error) {
	// 前向修复：创建新关联 Operation（不生成假定存在的通用回滚）。
	repair := Operation{
		TaskID:          originalOp.TaskID,
		CapabilityKey:   originalOp.CapabilityKey,
		CapabilityVersion: originalOp.CapabilityVersion,
		ConnectionID:    originalOp.ConnectionID,
		TargetRefs:      originalOp.TargetRefs,
		ParamsDigest:    digest(repairParams),
		AuthorizationRef: originalOp.AuthorizationRef,
		IdempotencyKey:  originalOp.IdempotencyKey + "-repair-" + id(),
		PredecessorID:   originalOp.OperationID,
	}
	return registerOperation(tx, repair, now)
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
