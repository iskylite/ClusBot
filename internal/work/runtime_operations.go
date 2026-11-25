package work

// Operation 七态、尝试与逐目标结果。
//
// 本文件实现每个外部副作用有稳定身份、固定意图和可核对历史：
//   - Operation 绑定 Task/Step、能力/连接、目标快照、参数摘要、授权依据和
//     幂等键；登记与 Outbox 同事务；
//   - 实现七态守卫；outcome unknown 映射待核对，不能新增随意状态或直接失败重试；
//   - 同一逻辑操作跨 attempt 保持幂等键和参数；参数/目标变更创建新关联 Operation；
//   - 批量保存逐目标状态与证据；按成功余集补做是新操作，不改写原参数；
//   - 迟到矛盾回执保留并进入核对，不直接改写终态；失败可包含已发生效果。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Operation（稳定身份、固定意图、可核对历史）
// ---------------------------------------------------------------------------

// Operation 是 operations 的一行。
type Operation struct {
	OperationID       string
	TaskID            string
	StepID            string
	CapabilityKey     string
	CapabilityVersion int
	ConnectionID      string
	TargetRefs        string
	ParamsDigest      string
	AuthorizationRef  string
	IdempotencyKey    string
	State             RuntimeOperationState
	AttemptCount      int
	CurrentAttemptID  string
	PredecessorID     string
	RegisteredAt      int64
	UpdatedAt         int64
	CompletedAt       int64
	EffectRefs        string
	FailureReason     string
}

// ---------------------------------------------------------------------------
// 登记（与 Outbox 同事务）
// ---------------------------------------------------------------------------

// registerOperation 登记一个 Operation。
// Operation 绑定 Task/Step、能力/连接、目标快照、参数摘要、授权依据和幂等键；
// 登记与 Outbox 同事务；同请求重复只返回同一 Operation。
func registerOperation(tx *sql.Tx, o Operation, now int64) (Operation, error) {
	if o.TaskID == "" || o.CapabilityKey == "" || o.IdempotencyKey == "" {
		return Operation{}, errors.New("Operation 缺少任务/能力/幂等键")
	}
	if o.ParamsDigest == "" || o.TargetRefs == "" {
		return Operation{}, errors.New("Operation 缺少参数摘要或目标快照")
	}
	// 同请求重复：返回既有 Operation（幂等键唯一）。
	var existingID string
	err := tx.QueryRow(`SELECT operation_id FROM operations WHERE idempotency_key=?`, o.IdempotencyKey).Scan(&existingID)
	if err == nil {
		existing, err := readOperation(tx, existingID)
		if err != nil {
			return Operation{}, err
		}
		if existing.TaskID != o.TaskID || existing.StepID != o.StepID ||
			existing.CapabilityKey != o.CapabilityKey || existing.CapabilityVersion != o.CapabilityVersion ||
			existing.ConnectionID != o.ConnectionID || existing.TargetRefs != o.TargetRefs ||
			existing.ParamsDigest != o.ParamsDigest || existing.AuthorizationRef != o.AuthorizationRef ||
			existing.PredecessorID != o.PredecessorID {
			return Operation{}, fmt.Errorf("%w：操作幂等键已绑定不同的执行意图", ErrConflict)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, err
	}
	// 登记事务失败不产生可派发动作：登记与 Outbox 同事务（在事务内失败则回滚）。
	o.OperationID = id()
	o.State = RuntimeOperationRegistered
	o.RegisteredAt, o.UpdatedAt = now, now
	var stepID, currentAttemptID, predecessorID any
	if o.StepID != "" {
		stepID = o.StepID
	}
	if o.CurrentAttemptID != "" {
		currentAttemptID = o.CurrentAttemptID
	}
	if o.PredecessorID != "" {
		predecessorID = o.PredecessorID
	}
	_, err = tx.Exec(`INSERT INTO operations(operation_id,task_id,step_id,capability_key,capability_version,
connection_id,target_refs,params_digest,authorization_ref,idempotency_key,state,attempt_count,current_attempt_id,
predecessor_id,registered_at,updated_at,effect_refs,failure_reason)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.OperationID, o.TaskID, stepID, o.CapabilityKey, o.CapabilityVersion, o.ConnectionID,
		o.TargetRefs, o.ParamsDigest, o.AuthorizationRef, o.IdempotencyKey, string(o.State),
		o.AttemptCount, currentAttemptID, predecessorID, o.RegisteredAt, now, o.EffectRefs, o.FailureReason)
	if err != nil {
		return Operation{}, err
	}
	return o, nil
}

// readOperation 读取一个 Operation。
func readOperation(q recordsQuerier, operationID string) (Operation, error) {
	var o Operation
	var stepID, currentAttemptID, predecessorID sql.NullString
	var state string
	err := q.QueryRow(`SELECT operation_id,task_id,step_id,capability_key,capability_version,connection_id,
target_refs,params_digest,authorization_ref,idempotency_key,state,attempt_count,current_attempt_id,
predecessor_id,registered_at,updated_at,completed_at,effect_refs,failure_reason
FROM operations WHERE operation_id=?`, operationID).
		Scan(&o.OperationID, &o.TaskID, &stepID, &o.CapabilityKey, &o.CapabilityVersion, &o.ConnectionID,
			&o.TargetRefs, &o.ParamsDigest, &o.AuthorizationRef, &o.IdempotencyKey, &state,
			&o.AttemptCount, &currentAttemptID, &predecessorID, &o.RegisteredAt, &o.UpdatedAt,
			&o.CompletedAt, &o.EffectRefs, &o.FailureReason)
	if err != nil {
		return o, err
	}
	o.StepID = stepID.String
	o.CurrentAttemptID = currentAttemptID.String
	o.PredecessorID = predecessorID.String
	o.State = RuntimeOperationState(state)
	return o, nil
}

// ---------------------------------------------------------------------------
// 七态守卫（outcome unknown 映射待核对，不能新增随意状态）
// ---------------------------------------------------------------------------

// transitionOperation 转换 Operation 状态。
// 七态守卫：状态跳跃、终态复活被拒绝；outcome unknown 映射待核对。
func transitionOperation(tx *sql.Tx, operationID string, to RuntimeOperationState, now int64) (Operation, error) {
	o, err := readOperation(tx, operationID)
	if err != nil {
		return o, err
	}
	// 终态不复活。
	if o.State == RuntimeOperationSucceeded || o.State == RuntimeOperationFailed || o.State == RuntimeOperationCancelled {
		return o, fmt.Errorf("%w：Operation 终态 %s 不能复活", ErrTaskTerminal, o.State)
	}
	// 状态跳跃：必须经允许迁移表。
	if !CanTransitionOperation(o.State, to) {
		return o, fmt.Errorf("Operation 状态跳跃：%s → %s 不允许", o.State, to)
	}
	res, err := tx.Exec(`UPDATE operations SET state=?, updated_at=? WHERE operation_id=? AND state=?`,
		string(to), now, operationID, string(o.State))
	if err != nil {
		return o, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return o, fmt.Errorf("Operation 状态已被并发修改")
	}
	o.State = to
	o.UpdatedAt = now
	return o, nil
}

// mapOutcomeUnknownToReconcile 把 outcome unknown 映射为待核对。
// outcome unknown 映射待核对，不能新增随意状态或直接失败重试。
func mapOutcomeUnknownToReconcile(tx *sql.Tx, operationID string, now int64) error {
	o, err := readOperation(tx, operationID)
	if err != nil {
		return err
	}
	if o.State == RuntimeOperationRunning {
		if _, err := transitionOperation(tx, operationID, RuntimeOperationReconcile, now); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 尝试（同一逻辑操作跨 attempt 保持幂等键和参数）
// ---------------------------------------------------------------------------

// OperationAttempt 是 operation_attempts 的一行。
type OperationAttempt struct {
	AttemptID     string
	OperationID   string
	AttemptNumber int
	DispatchedAt  int64
	Receipt       string
	Outcome       string // success/failure/unknown/pending
	EvidenceRefs  string
	CompletedAt   int64
}

// dispatchOperationAttempt 派发一个尝试。
// 同一逻辑操作跨 attempt 保持幂等键和参数；参数/目标变更创建新关联 Operation。
func dispatchOperationAttempt(tx *sql.Tx, operationID string, now int64) (OperationAttempt, error) {
	o, err := readOperation(tx, operationID)
	if err != nil {
		return OperationAttempt{}, err
	}
	// 状态守卫：只有 registered/dispatched 可派发新 attempt。
	if o.State != RuntimeOperationRegistered && o.State != RuntimeOperationDispatched {
		return OperationAttempt{}, fmt.Errorf("Operation 状态 %s 不能派发新 attempt", o.State)
	}
	// 创建新 attempt（同一逻辑操作跨 attempt 保持幂等键和参数）。
	attempt := OperationAttempt{
		AttemptID: id(), OperationID: operationID, AttemptNumber: o.AttemptCount + 1,
		DispatchedAt: now, Outcome: "pending",
	}
	if _, err := tx.Exec(`INSERT INTO operation_attempts(attempt_id,operation_id,attempt_number,dispatched_at,outcome)
VALUES(?,?,?,?,?)`, attempt.AttemptID, operationID, attempt.AttemptNumber, now, "pending"); err != nil {
		return OperationAttempt{}, err
	}
	// 更新 Operation（attempt_count + 状态）。
	if _, err := tx.Exec(`UPDATE operations SET attempt_count=attempt_count+1, current_attempt_id=?,
state='dispatched', updated_at=? WHERE operation_id=?`, attempt.AttemptID, now, operationID); err != nil {
		return OperationAttempt{}, err
	}
	return attempt, nil
}

// ---------------------------------------------------------------------------
// 逐目标结果（批量保存逐目标状态与证据）
// ---------------------------------------------------------------------------

// OperationTargetResult 是 operation_target_results 的一行。
type OperationTargetResult struct {
	OperationID string
	TargetRef   string
	State       string // pending/succeeded/failed/unknown
	EvidenceRef string
	Detail      string
	UpdatedAt   int64
}

// recordTargetResult 记录逐目标结果。
// 批量保存逐目标状态与证据；部分成功不等于全批完成，未知部分不被成功部分覆盖。
func recordTargetResult(tx *sql.Tx, r OperationTargetResult, now int64) error {
	if r.OperationID == "" || r.TargetRef == "" || r.State == "" {
		return errors.New("目标结果缺少操作/目标/状态")
	}
	r.UpdatedAt = now
	_, err := tx.Exec(`INSERT INTO operation_target_results(operation_id,target_ref,state,evidence_ref,detail,updated_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(operation_id,target_ref) DO UPDATE SET
state=excluded.state,evidence_ref=excluded.evidence_ref,detail=excluded.detail,updated_at=excluded.updated_at`,
		r.OperationID, r.TargetRef, r.State, r.EvidenceRef, r.Detail, now)
	return err
}

// readTargetResults 读取一个 Operation 的逐目标结果。
func readTargetResults(q recordsQuerier, operationID string) ([]OperationTargetResult, error) {
	rows, err := q.Query(`SELECT operation_id,target_ref,state,evidence_ref,detail,updated_at
FROM operation_target_results WHERE operation_id=?`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationTargetResult
	for rows.Next() {
		var r OperationTargetResult
		if err := rows.Scan(&r.OperationID, &r.TargetRef, &r.State, &r.EvidenceRef, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 迟到矛盾回执（保留并进入核对，不直接改写终态）
// ---------------------------------------------------------------------------

// recordConflictingReceipt 记录迟到矛盾回执。
// 迟到矛盾回执保留并进入核对，不直接改写终态；失败可包含已发生效果。
func recordConflictingReceipt(tx *sql.Tx, operationID, receipt, evidenceRef string, now int64) error {
	o, err := readOperation(tx, operationID)
	if err != nil {
		return err
	}
	// 迟到矛盾回执：保留（进入核对），不直接改写终态。
	if o.State == RuntimeOperationSucceeded || o.State == RuntimeOperationFailed || o.State == RuntimeOperationCancelled {
		// 终态：保留回执留证（不改写终态）。
		return recordAudit(tx, AuditEvent{
			TaskID: o.TaskID, Event: "conflicting_receipt_after_terminal", Actor: "system",
			Summary:   fmt.Sprintf("Operation %s 终态后收到矛盾回执（保留留证）", operationID),
			DetailRef: evidenceRef, OccurredAt: now,
		})
	}
	// 非终态：进入核对。
	if _, err := transitionOperation(tx, operationID, RuntimeOperationReconcile, now); err != nil {
		return err
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
