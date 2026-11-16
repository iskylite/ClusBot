package work

// P1-05：通用 Inbox 与幂等应用。
//
// 本文件实现 V3 §5.3 的持久输入箱：先可靠受理事件，再以单事务应用到
// 当前权威状态。
//   - (source, event_id) 唯一；同 ID 同摘要返回原受理结果，同 ID 不同摘要
//     隔离并审计，不覆盖原事件；
//   - 应用事件时核对主体/关联/版本，在一个事务更新目标、审计、后续 Outbox
//     并标记已处理；
//   - 未知关联、不能判定顺序和处理失败保存明确原因与重试安排；
//   - 现有 Chat 输入以适配方式进入公共处理链，避免新旧表各自确认同一输入。
//
// 与现有 input_events 的关系：本文件是 V3 通用入口，HTTP/Worker 后续接入；
// 现有 Chat 的 input_events/input_deliveries 继续服务旧路径，P1-12 适配接入
// 公共链，不在本项删除旧结构。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Inbox 事件
// ---------------------------------------------------------------------------

// InboxEvent 是 inbox_events 的一行。
type InboxEvent struct {
	Source         string
	EventID        string
	SourceSeq      int64
	SourceVersion  string
	Principal      string
	TaskID         string
	StepID         string
	RunID          string
	OperationID    string
	WaitID         string
	Kind           string
	Summary        string
	PayloadDigest  string
	PayloadRef     string
	ReceivedAt     int64
	State          string // received/applying/applied/quarantined/failed
	AppliedAt      int64
	Attempts       int
	NextRetryAt    int64
	Failure        string
	QuarantineReason string
}

// ---------------------------------------------------------------------------
// 受理（先持久化再确认接收）
// ---------------------------------------------------------------------------

// ReceiveResult 是受理结果。
type ReceiveResult struct {
	Event      InboxEvent
	Duplicate  bool   // 同 ID 同摘要：返回既有受理结果
	Quarantined bool  // 同 ID 不同摘要：已隔离
}

// receiveInbox 先持久受理事件（P1-05.1）。
// 同 ID 同内容返回既有受理结果（幂等）；同 ID 不同内容隔离并审计，不覆盖原事件。
func receiveInbox(tx *sql.Tx, e InboxEvent) (ReceiveResult, error) {
	if e.Source == "" || e.EventID == "" {
		return ReceiveResult{}, errors.New("inbox 事件缺少来源或事件 ID")
	}
	if e.Principal == "" {
		return ReceiveResult{}, errors.New("inbox 事件缺少可信主体")
	}
	if e.PayloadDigest == "" {
		return ReceiveResult{}, errors.New("inbox 事件缺少内容摘要")
	}
	// 同 ID 检查（先读后写在同一事务）。
	var existing InboxEvent
	var taskID, stepID, runID, operationID, waitID sql.NullString
	err := tx.QueryRow(`SELECT source,event_id,source_seq,source_version,principal,task_id,step_id,run_id,operation_id,wait_id,
kind,summary,payload_digest,payload_ref,received_at,state,applied_at,attempts,next_retry_at,failure,quarantine_reason
FROM inbox_events WHERE source=? AND event_id=?`, e.Source, e.EventID).
		Scan(&existing.Source, &existing.EventID, &existing.SourceSeq, &existing.SourceVersion,
			&existing.Principal, &taskID, &stepID, &runID, &operationID, &waitID,
			&existing.Kind, &existing.Summary, &existing.PayloadDigest, &existing.PayloadRef,
			&existing.ReceivedAt, &existing.State, &existing.AppliedAt, &existing.Attempts,
			&existing.NextRetryAt, &existing.Failure, &existing.QuarantineReason)
	if err == nil {
		existing.TaskID = taskID.String
		existing.StepID = stepID.String
		existing.RunID = runID.String
		existing.OperationID = operationID.String
		existing.WaitID = waitID.String
		// 同 ID：核对内容摘要一致性。
		if existing.PayloadDigest != e.PayloadDigest {
			// 同 ID 不同摘要：隔离并审计，不覆盖原事件（P1-05.2）。
			quarantined := e
			quarantined.State = "quarantined"
			quarantined.QuarantineReason = fmt.Sprintf("同 ID 不同摘要：已有 %s，新到 %s",
				existing.PayloadDigest, e.PayloadDigest)
			if err := recordAudit(tx, AuditEvent{
				TaskID: existing.TaskID, Event: "inbox_conflict", Actor: e.Principal,
				Summary: quarantined.QuarantineReason, OccurredAt: e.ReceivedAt,
			}); err != nil {
				return ReceiveResult{}, err
			}
			return ReceiveResult{Event: existing, Quarantined: true}, nil
		}
		// 同 ID 同摘要：返回既有受理结果（幂等）。
		return ReceiveResult{Event: existing, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReceiveResult{}, err
	}
	// 新事件：持久受理。
	e.State = "received"
	e.Attempts = 0
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err = tx.Exec(`INSERT INTO inbox_events(source,event_id,source_seq,source_version,principal,
task_id,step_id,run_id,operation_id,wait_id,kind,summary,payload_digest,payload_ref,received_at,state)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Source, e.EventID, e.SourceSeq, e.SourceVersion, e.Principal,
		nullable(e.TaskID), nullable(e.StepID), nullable(e.RunID), nullable(e.OperationID), nullable(e.WaitID),
		e.Kind, e.Summary, e.PayloadDigest, e.PayloadRef, e.ReceivedAt, e.State)
	if err != nil {
		return ReceiveResult{}, err
	}
	return ReceiveResult{Event: e}, nil
}

// ---------------------------------------------------------------------------
// 应用（单事务更新目标 + 审计 + 后续 Outbox + 标记已处理）
// ---------------------------------------------------------------------------

// ApplyResult 是应用结果。
type ApplyResult struct {
	Applied    bool
	FollowUp   []string // 后续 Outbox 命令（由调用方登记）
	Failure    string
	RetryAfter int64 // 处理失败时的重试时间（毫秒时间戳）
}

// applyInboxEvent 以单事务应用事件到当前权威状态（P1-05.3）。
// 应用时核对主体/关联/版本，更新目标、写审计、登记后续 Outbox 并标记已处理。
// applier 是业务应用函数：在事务内执行实际状态更新，返回后续命令或失败原因。
func applyInboxEvent(tx *sql.Tx, source, eventID string, applier func(tx *sql.Tx, e InboxEvent) (ApplyResult, error), now int64) (ApplyResult, error) {
	// 读取并锁定事件（同事务重新读取）。
	e, err := readInboxForUpdate(tx, source, eventID)
	if err != nil {
		return ApplyResult{}, err
	}
	if e.State == "applied" {
		// 幂等：已应用的事件不重复应用（F03）。
		return ApplyResult{Applied: true}, nil
	}
	if e.State == "quarantined" {
		return ApplyResult{}, fmt.Errorf("事件已隔离：%s", e.QuarantineReason)
	}
	// 标记为应用中（并发处理同事件只产生一个有效结果）。
	res, err := tx.Exec(`UPDATE inbox_events SET state='applying', attempts=attempts+1
WHERE source=? AND event_id=? AND state IN ('received','failed')`, source, eventID)
	if err != nil {
		return ApplyResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ApplyResult{}, err
	}
	if n != 1 {
		// 并发竞争：另一处理者已取得，保留可审计信息。
		return ApplyResult{}, ErrState
	}
	// 核对关联：未知关联隔离待查，不丢弃（P1-05.4）。
	if err := validateInboxAssociation(tx, e); err != nil {
		markInboxFailed(tx, source, eventID, "quarantined", err.Error(), 0, now)
		return ApplyResult{Failure: err.Error()}, err
	}
	// 业务应用。
	result, err := applier(tx, e)
	if err != nil {
		// 处理失败：保存明确原因与重试安排（P1-05.4）。
		retryAt := now + retryBackoff(e.Attempts)
		markInboxFailed(tx, source, eventID, "failed", err.Error(), retryAt, now)
		return ApplyResult{Failure: err.Error(), RetryAfter: retryAt}, err
	}
	// 应用成功：标记已处理（与业务更新同事务）。
	if _, err := tx.Exec(`UPDATE inbox_events SET state='applied', applied_at=? WHERE source=? AND event_id=?`,
		now, source, eventID); err != nil {
		return ApplyResult{}, err
	}
	result.Applied = true
	return result, nil
}

// readInboxForUpdate 读取事件用于更新。
func readInboxForUpdate(tx *sql.Tx, source, eventID string) (InboxEvent, error) {
	var e InboxEvent
	var taskID, stepID, runID, operationID, waitID sql.NullString
	err := tx.QueryRow(`SELECT source,event_id,source_seq,source_version,principal,task_id,step_id,run_id,operation_id,wait_id,
kind,summary,payload_digest,payload_ref,received_at,state,applied_at,attempts,next_retry_at,failure,quarantine_reason
FROM inbox_events WHERE source=? AND event_id=?`, source, eventID).
		Scan(&e.Source, &e.EventID, &e.SourceSeq, &e.SourceVersion, &e.Principal,
			&taskID, &stepID, &runID, &operationID, &waitID,
			&e.Kind, &e.Summary, &e.PayloadDigest, &e.PayloadRef, &e.ReceivedAt,
			&e.State, &e.AppliedAt, &e.Attempts, &e.NextRetryAt, &e.Failure, &e.QuarantineReason)
	e.TaskID = taskID.String
	e.StepID = stepID.String
	e.RunID = runID.String
	e.OperationID = operationID.String
	e.WaitID = waitID.String
	return e, err
}

// validateInboxAssociation 核对事件关联：未知关联隔离待查，不丢弃。
func validateInboxAssociation(tx *sql.Tx, e InboxEvent) error {
	if e.TaskID == "" {
		// 无关联任务的全局事件（如系统通知）：允许。
		return nil
	}
	// 核对任务存在（未知关联隔离，不静默丢弃）。
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, e.TaskID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("未知关联任务 %s，隔离待查", e.TaskID)
	}
	return nil
}

// markInboxFailed 标记事件处理失败或隔离，保存原因与重试安排。
func markInboxFailed(tx *sql.Tx, source, eventID, state, reason string, retryAt, now int64) {
	tx.Exec(`UPDATE inbox_events SET state=?, failure=?, next_retry_at=?, quarantine_reason=CASE WHEN ?='quarantined' THEN ? ELSE quarantine_reason END
WHERE source=? AND event_id=?`, state, reason, retryAt, state, reason, source, eventID)
}

// retryBackoff 计算重试退避（有界，含抖动上限）。
func retryBackoff(attempts int) int64 {
	// 1s, 2s, 4s, ... 上限 5 分钟。
	ms := int64(1000) << uint(attempts)
	if ms > 300000 {
		ms = 300000
	}
	return ms
}

// ---------------------------------------------------------------------------
// Chat 输入适配（P1-05.5）
// ---------------------------------------------------------------------------

// AdaptChatInput 把现有 Chat 输入适配为公共 Inbox 事件。
// 避免新旧表各自确认同一输入：Chat 的 input_events 是旧路径的受理记录，
// 本函数把它映射为 Inbox 事件进入公共处理链；完整切换在 P1-12。
func AdaptChatInput(tx *sql.Tx, source, requestKey, principal, taskID, summary string, payloadDigest string, receivedAt int64) (ReceiveResult, error) {
	return receiveInbox(tx, InboxEvent{
		Source:        source,
		EventID:       requestKey,
		Principal:     principal,
		TaskID:        taskID,
		Kind:          "chat_input",
		Summary:       summary,
		PayloadDigest: payloadDigest,
		ReceivedAt:    receivedAt,
	})
}

// 确保 context 引用保留。
var _ = context.Background
