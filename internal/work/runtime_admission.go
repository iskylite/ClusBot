package work

// P1-12：Router 持久受理与现有个人助手兼容。
//
// 本文件实现 Chat 仍是入口，但持久受理、模型路由和任务执行各有独立结果：
//   - 在模型分类前登记可信输入及请求键，返回可查询受理标识；尚未分类不声称
//     Task 已建立；
//   - 复用多意图与冲突识别，把创建/修订/控制转换成运行时命令，不让 Router
//     独立改运行中状态；
//   - 每个意图保留片段、目标和回执；一项澄清不阻断无依赖的其他项，重试不
//     会重复委派；
//   - 保持普通问答、材料整理、成果保存和人工验收；旧 API 字段用明确映射适配，
//     不混用旧/新终态；
//   - 需要等待或用户决定时返回具体原因和对象；禁止 UI 因请求已受理就显示
//     执行完成。
//
// 与现有提交链的关系：本文件在 HTTP submit 与 store.submitWithDelivery 之间
// 加入持久受理层，复用现有分类与执行，不改变个人助手交付功能。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ---------------------------------------------------------------------------
// 持久受理（分类前登记可信输入及请求键）
// ---------------------------------------------------------------------------

// AdmissionReceipt 是持久受理回执。
type AdmissionReceipt struct {
	// ReceiptID 是可查询受理标识（请求键）。
	ReceiptID string
	// RequestKey 是稳定请求键（重试同键得到同一受理）。
	RequestKey string
	// State 是受理状态：received（已受理未分类）/classified/executing/completed。
	State string
	// TaskID 是已建立的任务（分类前为空，不声称 Task 已建立）。
	TaskID         string
	RunID          string
	ConversationID string
	// Duplicate 为 true 表示重复请求键返回原受理。
	Duplicate bool
	// Reason 是需要等待或用户决定的具体原因。
	Reason string
}

// admitChatInput 在模型分类前登记可信输入及请求键（P1-12.1）。
// 返回可查询受理标识；尚未分类不声称 Task 已建立。
// 重试同键得到同一受理，不重复调用无依据的模型分类。
func admitChatInput(tx *sql.Tx, requestKey, principal, conversationID, text, scope string, materialIDs []string, now int64) (AdmissionReceipt, error) {
	fingerprint := submissionFingerprint(text, scope, conversationID, false)
	if len(materialIDs) > 0 {
		fingerprint = digest(fmt.Sprintf("%s %q", fingerprint, materialIDs))
	}
	return admitChatInputWithFingerprint(tx, requestKey, principal, text, fingerprint, "", now)
}

// admitChatInputWithFingerprint 以调用方提供的完整指纹登记可信输入。
// HTTP 层与提交链共用同一指纹算法（fullSubmissionFingerprint），保证同键同请求
// 返回原回执、同键异请求冲突，且与后续分类/提交去重一致。
func admitChatInputWithFingerprint(tx *sql.Tx, requestKey, principal, text, fingerprint, payload string, now int64) (AdmissionReceipt, error) {
	if requestKey == "" || principal == "" {
		return AdmissionReceipt{}, errors.New("受理缺少请求键或可信主体")
	}
	if text == "" {
		return AdmissionReceipt{}, errors.New("受理缺少输入内容")
	}
	// 登记可信输入到公共 Inbox（复用 P1-05 的受理链）。
	result, err := receiveInbox(tx, InboxEvent{
		Source:        "chat",
		EventID:       requestKey,
		Principal:     principal,
		Kind:          "chat_input",
		Summary:       title(text),
		PayloadDigest: fingerprint,
		PayloadRef:    payload,
		ReceivedAt:    now,
	})
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if result.Quarantined {
		return AdmissionReceipt{}, fmt.Errorf("%w：同请求键不同内容", ErrConflict)
	}
	return AdmissionReceipt{
		ReceiptID:  requestKey,
		RequestKey: requestKey,
		State:      result.Event.State,
		TaskID:     result.Event.TaskID,
		RunID:      result.Event.RunID,
		Duplicate:  result.Duplicate,
	}, nil
}

// ---------------------------------------------------------------------------
// 受理状态查询
// ---------------------------------------------------------------------------

// readAdmission 查询一个受理的当前状态（P1-12.5）。
// 需要等待或用户决定时返回具体原因和对象；禁止因请求已受理就显示执行完成。
func readAdmission(q recordsQuerier, requestKey string) (AdmissionReceipt, error) {
	var e InboxEvent
	var taskID, runID sql.NullString
	var conversationID, failure string
	err := q.QueryRow(`SELECT e.event_id,e.task_id,e.run_id,e.state,e.failure,coalesce(r.conversation_id,'')
FROM inbox_events e LEFT JOIN runs r ON r.id=e.run_id WHERE e.source='chat' AND e.event_id=?`, requestKey).
		Scan(&e.EventID, &taskID, &runID, &e.State, &failure, &conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return AdmissionReceipt{}, fmt.Errorf("受理 %s 不存在", requestKey)
	}
	if err != nil {
		return AdmissionReceipt{}, err
	}
	e.TaskID = taskID.String
	receipt := AdmissionReceipt{
		ReceiptID:      e.EventID,
		RequestKey:     e.EventID,
		State:          e.State,
		TaskID:         e.TaskID,
		RunID:          runID.String,
		ConversationID: conversationID,
	}
	// 受理不等于完成：按状态给出具体原因，不显示执行完成。
	switch e.State {
	case "received":
		receipt.Reason = "已受理，等待分类与执行"
	case "applying":
		receipt.Reason = "正在处理"
	case "applied":
		receipt.Reason = "已处理（任务状态以任务查询为准，受理不等于任务成功）"
	case "quarantined":
		receipt.Reason = "内容冲突已隔离，需要用户核对"
	case "failed":
		receipt.Reason = failure
	}
	return receipt, nil
}

// ---------------------------------------------------------------------------
// 意图转换（创建/修订/控制 → 运行时命令）
// ---------------------------------------------------------------------------

// IntentCommand 是一个意图转换的运行时命令。
type IntentCommand struct {
	// Kind 是命令类型：create_task/revise_task/control（pause/resume/cancel）。
	Kind string
	// Fragment 是意图片段（保留原文）。
	Fragment string
	// TargetTaskID 是目标 Task（创建时为空）。
	TargetTaskID string
	// Receipt 是该意图的回执（独立结果，一项澄清不阻断无依赖项）。
	Receipt string
	// NeedsClarification 为 true 表示该项需澄清（不阻断其他项）。
	NeedsClarification bool
	// ClarificationReason 是澄清原因。
	ClarificationReason string
}

// convertIntents 把 Router 的多意图转换为运行时命令（P1-12.2/P1-12.3）。
// 不让 Router 独立改运行中状态：所有变更经运行时命令（状态机/控制记录）。
// 每个意图保留片段、目标和回执；一项澄清不阻断无依赖的其他项。
func convertIntents(intents []RouterIntent, existingTasks map[string]string) []IntentCommand {
	commands := make([]IntentCommand, 0, len(intents))
	for _, intent := range intents {
		cmd := IntentCommand{
			Fragment:     intent.Fragment,
			TargetTaskID: intent.TaskID,
		}
		switch intent.Kind {
		case "new_task", "delegate":
			cmd.Kind = "create_task"
			cmd.Receipt = "待创建任务"
		case "follow_up", "revise":
			cmd.Kind = "revise_task"
			if intent.TaskID == "" {
				cmd.NeedsClarification = true
				cmd.ClarificationReason = "修订目标不明确，需要澄清"
				cmd.Receipt = "待澄清"
			} else {
				cmd.Receipt = "待修订任务 " + intent.TaskID
			}
		case "pause", "stop", "resume", "cancel":
			cmd.Kind = "control"
			if intent.TaskID == "" {
				cmd.NeedsClarification = true
				cmd.ClarificationReason = "控制目标不明确，需要澄清"
				cmd.Receipt = "待澄清"
			} else {
				cmd.Receipt = "待控制任务 " + intent.TaskID + "（" + intent.Kind + "）"
			}
		case "clarify":
			cmd.Kind = "clarify"
			cmd.NeedsClarification = true
			cmd.ClarificationReason = intent.Fragment
			cmd.Receipt = "需要澄清"
		default:
			cmd.Kind = "query"
			cmd.Receipt = "查询（不改变运行状态）"
		}
		commands = append(commands, cmd)
	}
	return commands
}

// ---------------------------------------------------------------------------
// 状态映射适配（旧 API 字段用明确映射，不混用旧/新终态）
// ---------------------------------------------------------------------------

// MapLegacyStatusForAPI 把旧五态映射为 API 展示状态（P1-12.4）。
// 旧 API 字段用明确映射适配，不混用旧/新终态；保持普通问答、材料整理、
// 成果保存和人工验收的兼容展示。
func MapLegacyStatusForAPI(legacyStatus, legacyReason string) (apiPhase, reason string) {
	// 用 P0-02 的映射结论，转为 API 展示阶段（不直接暴露 V3 内部状态名）。
	mapping := MapLegacyTask(LegacyTaskSnapshot{Status: legacyStatus, StatusReason: legacyReason})
	switch mapping.State {
	case V3TaskPending:
		return "待执行", mapping.Reason
	case V3TaskRunning:
		if mapping.ReconcileRequired {
			return "执行中（待核对）", mapping.Reason
		}
		return "执行中", mapping.Reason
	case V3TaskWaiting:
		return "等待中", mapping.Reason
	case V3TaskPaused:
		return "已暂停", mapping.Reason
	case V3TaskAcceptance:
		return "待验收", mapping.Reason
	case V3TaskFinalizing:
		return "收尾中", mapping.Reason
	case V3TaskSucceeded:
		return "成功", mapping.Reason
	case V3TaskFailed:
		return "失败", mapping.Reason
	case V3TaskCancelled:
		return "已取消", mapping.Reason
	default:
		return "待核对", mapping.Reason
	}
}

// ---------------------------------------------------------------------------
// Store 层持久受理（A1：在可信主体与请求校验之后、模型分类之前提交 Inbox）
// ---------------------------------------------------------------------------

// AdmissionResult 是持久受理的完整结果。
type AdmissionResult struct {
	Receipt AdmissionReceipt
	// ModelPending 为 true 表示模型当前不可用，输入已受理但等待分类。
	ModelPending bool
}

// AdmitChat 在可信主体与请求校验之后、模型分类之前提交 Inbox 记录（A1）。
// 复用现有提交指纹与分类去重：同键同请求返回原回执，同键异请求返回冲突。
// modelAvailable 为 false 时输入仍被受理（状态 received），可后续查询等待原因；
// 重启后待分类输入可经回执恢复。principal 来自可信认证（会话），不由请求方自证。
func (s *Store) AdmitChat(ctx context.Context, requestKey, principal, conversationID, text, scope string, task bool, delivery Delivery, materialIDs []string, modelAvailable bool) (AdmissionResult, error) {
	return s.admitChat(ctx, requestKey, principal, conversationID, text, scope, task, delivery, materialIDs, modelAvailable, false, 0)
}

func (s *Store) admitChat(ctx context.Context, requestKey, principal, conversationID, text, scope string, task bool, delivery Delivery, materialIDs []string, modelAvailable, explicitTask bool, explicitTaskLimit int) (AdmissionResult, error) {
	if requestKey == "" || principal == "" {
		return AdmissionResult{}, errors.New("受理缺少请求键或可信主体")
	}
	if text == "" {
		return AdmissionResult{}, errors.New("受理缺少输入内容")
	}
	fingerprint, err := fullSubmissionFingerprint(text, scope, conversationID, task, delivery, materialIDs)
	if err != nil {
		return AdmissionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdmissionResult{}, err
	}
	defer tx.Rollback()
	payload, err := json.Marshal(chatAdmissionInput{
		Text: text, Scope: scope, ConversationID: conversationID, Task: task,
		Delivery: delivery, MaterialIDs: materialIDs,
	})
	if err != nil {
		return AdmissionResult{}, err
	}
	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil {
		return AdmissionResult{}, err
	}
	if previous.ID == "" && delivery.TaskRef != nil {
		if task {
			return AdmissionResult{}, ErrState
		}
		if err := checkMessageTaskRef(tx, principal, conversationID, delivery.TaskRef); err != nil {
			return AdmissionResult{}, err
		}
	}
	if explicitTask && explicitTaskLimit > 0 && previous.ID == "" {
		var alreadyAdmitted bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM inbox_events WHERE source='chat' AND event_id=?)`, requestKey).Scan(&alreadyAdmitted); err != nil {
			return AdmissionResult{}, err
		}
		// Replay and conflict handling still belong to the same durable
		// receipt path and take precedence over capacity for new requests.
		if !alreadyAdmitted {
			if err := checkExplicitTaskCapacity(tx, explicitTaskLimit); err != nil {
				return AdmissionResult{}, err
			}
		}
	}
	receipt, err := admitChatInputWithFingerprint(tx, requestKey, principal, text, fingerprint, string(payload), now())
	if err != nil {
		return AdmissionResult{}, err
	}
	if explicitTask && previous.ID == "" && !receipt.Duplicate {
		if _, err := tx.Exec(`UPDATE inbox_events SET explicit_task_admission=1 WHERE source='chat' AND event_id=?`, requestKey); err != nil {
			return AdmissionResult{}, err
		}
	}
	if receipt.Duplicate && receipt.State == "received" {
		// Older receipts stored a summary only. The original client can supply the body again.
		if _, err := tx.Exec(`UPDATE inbox_events SET payload_ref=? WHERE source='chat' AND event_id=? AND payload_ref=''`, string(payload), requestKey); err != nil {
			return AdmissionResult{}, err
		}
	}
	if !modelAvailable && previous.ID == "" && receipt.State == "received" {
		if _, err := tx.Exec(`UPDATE inbox_events SET kind='chat_pending',next_retry_at=0 WHERE source='chat' AND event_id=?`, requestKey); err != nil {
			return AdmissionResult{}, err
		}
	}
	if modelAvailable && previous.ID == "" && receipt.State == "received" {
		if _, err := tx.Exec(`UPDATE inbox_events SET next_retry_at=? WHERE source='chat' AND event_id=?`, now()+15000, requestKey); err != nil {
			return AdmissionResult{}, err
		}
	}
	if receipt.State == "failed" && previous.ID == "" && !modelAvailable {
		current, err := readAdmission(tx, requestKey)
		if err != nil {
			return AdmissionResult{}, err
		}
		return AdmissionResult{}, fmt.Errorf("%w：%s", ErrClassification, current.Reason)
	}
	if previous.ID != "" {
		if err := linkAdmissionToRun(tx, requestKey, previous.ID, previous.TaskID, now()); err != nil {
			return AdmissionResult{}, err
		}
		receipt.State, receipt.RunID, receipt.TaskID, receipt.ConversationID = "applied", previous.ID, previous.TaskID, previous.ConversationID
	}
	if err := tx.Commit(); err != nil {
		return AdmissionResult{}, err
	}
	return AdmissionResult{Receipt: receipt, ModelPending: !modelAvailable && receipt.State == "received"}, nil
}

// ReadAdmission 查询一个受理回执的当前状态（A1：可查询回执）。
// 需要等待或用户决定时返回具体原因；关联了 Run/Task 后一并返回。
func (s *Store) ReadAdmission(ctx context.Context, requestKey string) (AdmissionReceipt, error) {
	return readAdmission(s.db, requestKey)
}

// linkAdmissionToRun 把受理回执关联到后续建立的 Run/Task，并标记已处理（A1）。
// 在提交事务内调用：受理不等于执行完成，但分类/任务已提交时重试只返回原结果。
func linkAdmissionToRun(tx *sql.Tx, requestKey, runID, taskID string, now int64) error {
	// 应用受理（条件更新防并发；重复提交幂等）。
	res, err := tx.Exec(`UPDATE inbox_events SET run_id=nullif(?,''), task_id=nullif(?,''), state='applied', applied_at=?, payload_ref='', failure=''
WHERE source='chat' AND event_id=? AND state IN ('received','failed')`, runID, taskID, now, requestKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	// 仅首次应用写审计（P2-2：Chat 快路径补齐应用审计，与通用链语义对齐）。
	// 重复提交 0 行命中（幂等）不重复写审计。
	if n == 1 {
		if err := recordAudit(tx, AuditEvent{
			TaskID: taskID, RunID: runID, Event: "inbox_applied", Actor: "chat",
			Summary: "Chat 受理已应用并关联任务", OccurredAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) linkExistingAdmission(ctx context.Context, key string, run Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := linkAdmissionToRun(tx, key, run.ID, run.TaskID, now()); err != nil {
		return err
	}
	return tx.Commit()
}

type chatAdmissionInput struct {
	Text, Scope, ConversationID string
	Task                        bool
	Delivery                    Delivery
	MaterialIDs                 []string
}

type PendingChatAdmission struct {
	RequestKey     string `json:"request_key"`
	Summary        string `json:"summary"`
	State          string `json:"state"`
	Failure        string `json:"reason,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
}

func (s *Store) PendingChatAdmissions(ctx context.Context, principal string) ([]PendingChatAdmission, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.event_id,e.summary,e.state,e.failure,coalesce(r.conversation_id,''),coalesce(e.task_id,'')
FROM inbox_events e LEFT JOIN runs r ON r.id=e.run_id
WHERE e.source='chat' AND e.principal=? AND e.kind='chat_pending'
AND (e.state IN ('received','failed') OR r.id IS NOT NULL)
ORDER BY e.received_at DESC,e.rowid DESC LIMIT 50`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PendingChatAdmission{}
	for rows.Next() {
		var item PendingChatAdmission
		if err := rows.Scan(&item.RequestKey, &item.Summary, &item.State, &item.Failure, &item.ConversationID, &item.TaskID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ProcessChatAdmissions(ctx context.Context, route Router) error {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,payload_ref FROM inbox_events
WHERE source='chat' AND state='received' AND payload_ref<>'' AND next_retry_at<=?
ORDER BY received_at,rowid LIMIT 8`, now())
	if err != nil {
		return err
	}
	type item struct{ key, payload string }
	var pending []item
	for rows.Next() {
		var entry item
		if err := rows.Scan(&entry.key, &entry.payload); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		var input chatAdmissionInput
		if err := json.Unmarshal([]byte(entry.payload), &input); err != nil {
			if err := s.FailChatAdmission(ctx, entry.key, "无法读取已受理的输入"); err != nil {
				return err
			}
			continue
		}
		if input.Task {
			_, err = s.SubmitWithDelivery(ctx, entry.key, input.Text, input.Scope, input.ConversationID, true, input.Delivery, input.MaterialIDs...)
		} else {
			_, err = s.SubmitRoutedChat(ctx, entry.key, input.Text, input.Scope, input.ConversationID, input.Delivery, route, input.MaterialIDs...)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrActive) {
			if _, err := s.db.ExecContext(ctx, `UPDATE inbox_events SET next_retry_at=? WHERE source='chat' AND event_id=? AND state='received'`, now()+2000, entry.key); err != nil {
				return err
			}
			continue
		}
		if err := s.FailChatAdmission(ctx, entry.key, err.Error()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) FailChatAdmission(ctx context.Context, key, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_events SET state='failed',failure=? WHERE source='chat' AND event_id=? AND state='received'`, reason, key)
	return err
}

func (s *Store) RunChatAdmissions(ctx context.Context, modelAvailable func() bool, route Router) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if modelAvailable() {
				if err := s.ProcessChatAdmissions(ctx, route); err != nil && ctx.Err() == nil {
					slog.Error("推进 Chat 受理失败", "error", err)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 终态新委托（P1-12.5：终态新委托返回新 Task ID）
// ---------------------------------------------------------------------------

// SubmitTerminalFollowup 处理终态任务的新委托：返回新 Task ID（不复活原任务）。
// 历史页仍显示原成果与控制记录。
func SubmitTerminalFollowup(tx *sql.Tx, predecessorID string, req ControlRequest, template ScheduleTemplate, now int64) (SuccessorTask, error) {
	// 复用 P1-11 的关联新任务（不复活原任务，返回明确新 ID）。
	return createSuccessorTask(tx, predecessorID, req, template, now)
}

// 确保 context 引用保留。
var _ = context.Background
