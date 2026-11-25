package work

// 暂停、取消、恢复与关联新任务。
//
// 本文件实现控制请求先持久化，状态和外部事实分开处理：
//   - 控制记录包含请求键、决定者、目标 Task/合同、控制修订及请求类型；
//     重复请求返回原决定；
//   - 暂停阻止新领取/新派发，仍允许接收已发出的结果和必要核对；不会宣告
//     外部工作停止；
//   - 取消/最终失败进入收尾，逐项记录已发生、未发生、未知、占用和后续责任；
//     未核对项不静默消失；
//   - 恢复前核对当前合同、Wait、未决操作、权限及依赖；符合条件再排队，不能
//     直接重放旧 Run 上下文；
//   - 终态后的重试/新要求建立 successor Task 并保留关联；更新旧 Resume/Retry
//     行为与请求回执。
//
// 与旧控制路径的关系：本文件在 的 control_records、的状态机、
// 的 Wait、的收尾之上提供统一控制入口；旧 Resume/Retry 的正式
// 切换在本项验收，但旧 API 仍由 的兼容入口处理。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 控制请求（先持久化）
// ---------------------------------------------------------------------------

// ControlRequest 是一次控制请求。
type ControlRequest struct {
	RequestKey     string
	TaskID         string
	DecidedBy      string
	Kind           string // pause/resume/cancel/revise/stop
	TargetContractRevision int
	Decision       string
	Detail         string
}

// recordControlRequest 先持久化控制请求。
// 重复请求键相同内容返回原决定（幂等）；不同内容冲突。
func recordControlRequest(tx *sql.Tx, req ControlRequest, controlRevision int, now int64) (ControlRecord, error) {
	if req.RequestKey == "" || req.TaskID == "" || req.DecidedBy == "" {
		return ControlRecord{}, errors.New("控制请求缺少请求键/任务/决定者")
	}
	return insertControlRecord(tx, ControlRecord{
		RecordID:               id(),
		TaskID:                 req.TaskID,
		RequestKey:             req.RequestKey,
		DecidedBy:              req.DecidedBy,
		Kind:                   req.Kind,
		ControlRevision:        controlRevision,
		TargetContractRevision: req.TargetContractRevision,
		Decision:               req.Decision,
		Detail:                 req.Detail,
		CreatedAt:              now,
	})
}

// ---------------------------------------------------------------------------
// 暂停（阻止新领取/新派发，仍允许接收已发出结果）
// ---------------------------------------------------------------------------

// PauseResult 是暂停结果。
type PauseResult struct {
	Transition TaskTransition
	// OutstandingDispatched 已派发未核对的操作数（继续核对，不宣告外部停止）。
	OutstandingDispatched int
	// FollowUp 后续命令（核对已派发）。
	FollowUp []string
}

// pauseTask 暂停任务。
// 阻止新领取/新派发，仍允许接收已发出的结果和必要核对；不会宣告外部工作停止。
func pauseTask(tx *sql.Tx, req ControlRequest, guard TaskGuard, now int64) (PauseResult, error) {
	// 先持久化控制请求。
	if _, err := recordControlRequest(tx, req, guard.ExpectedStorageRevision+1, now); err != nil {
		return PauseResult{}, err
	}
	// 状态机转换：→ 已暂停。
	transition, err := transitionTaskState(tx, req.TaskID, EventPause, guard, now)
	if err != nil {
		return PauseResult{}, err
	}
	// 统计已派发未核对的操作（继续核对，不宣告外部停止）。
	var outstanding int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('claimed','running')`, req.TaskID).
		Scan(&outstanding); err != nil {
		return PauseResult{}, err
	}
	followUp := transition.FollowUp
	if outstanding > 0 {
		followUp = append(followUp, fmt.Sprintf("reconcile_%d_dispatched", outstanding))
	}
	return PauseResult{
		Transition:            transition,
		OutstandingDispatched: outstanding,
		FollowUp:              followUp,
	}, nil
}

// ---------------------------------------------------------------------------
// 取消/最终失败（进入收尾，逐项记录责任）
// ---------------------------------------------------------------------------

// CancelResult 是取消结果。
type CancelResult struct {
	Transition TaskTransition
	// Finalization 收尾记录：已发生、未发生、未知、占用和后续责任。
	Finalization FinalizationRecord
	// FollowUp 后续命令（核对在途/占用/未知）。
	FollowUp []string
}

// cancelTask 取消任务。
// 进入收尾，逐项记录已发生、未发生、未知、占用和后续责任；未核对项不静默
// 消失；取消不能自动回滚成果、设备状态或释放未知影响。
func cancelTask(tx *sql.Tx, req ControlRequest, guard TaskGuard, now int64) (CancelResult, error) {
	// 先持久化控制请求。
	if _, err := recordControlRequest(tx, req, guard.ExpectedStorageRevision+1, now); err != nil {
		return CancelResult{}, err
	}
	// 状态机转换：→ 收尾中。
	transition, err := transitionTaskState(tx, req.TaskID, EventCancel, guard, now)
	if err != nil {
		return CancelResult{}, err
	}
	// 收尾记录：逐项记录剩余占用/待决定/未知/开放等待（未核对项不静默消失）。
	finalization, err := buildFinalizationRecord(tx, req.TaskID)
	if err != nil {
		return CancelResult{}, err
	}
	followUp := transition.FollowUp
	// 取消不能自动回滚成果、设备状态或释放未知影响：保留逐项核对命令。
	if finalization.OutstandingUnits > 0 {
		followUp = append(followUp, "reconcile_outstanding_units")
	}
	if finalization.PendingDecisions > 0 {
		followUp = append(followUp, "reconcile_pending_decisions")
	}
	return CancelResult{
		Transition:    transition,
		Finalization:  finalization,
		FollowUp:      followUp,
	}, nil
}

// ---------------------------------------------------------------------------
// 恢复（核对当前合同、Wait、未决操作、权限及依赖）
// ---------------------------------------------------------------------------

// ResumeResult 是恢复结果。
type ResumeResult struct {
	Transition TaskTransition
	// RecheckedOps 重新核对的未决操作数。
	RecheckedOps int
	// FollowUp 后续命令。
	FollowUp []string
}

// resumeRuntimeTask 恢复已暂停任务。
// 恢复前核对当前合同、Wait、未决操作、权限及依赖；符合条件再排队，不能
// 直接重放旧 Run 上下文。
func resumeRuntimeTask(tx *sql.Tx, req ControlRequest, guard TaskGuard, now int64) (ResumeResult, error) {
	// 先持久化控制请求。
	if _, err := recordControlRequest(tx, req, guard.ExpectedStorageRevision+1, now); err != nil {
		return ResumeResult{}, err
	}
	// 核对未决操作（恢复不会重用过期执行代际，未核对结果不能作为继续依据）。
	var pendingOps int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('claimed','running')`, req.TaskID).
		Scan(&pendingOps); err != nil {
		return ResumeResult{}, err
	}
	guard.PendingOperations = pendingOps
	// 核对开放 Wait（决定恢复后回到待执行还是等待中）。
	openWaits, err := readOpenWaits(tx, req.TaskID)
	if err != nil {
		return ResumeResult{}, err
	}
	guard.HasOpenWaits = len(openWaits) > 0
	// 状态机转换：→ 待执行或等待中（重评）。
	transition, err := transitionTaskState(tx, req.TaskID, EventResume, guard, now)
	if err != nil {
		return ResumeResult{}, err
	}
	followUp := transition.FollowUp
	if pendingOps > 0 {
		followUp = append(followUp, "recheck_operations")
	}
	return ResumeResult{
		Transition:   transition,
		RecheckedOps: pendingOps,
		FollowUp:     followUp,
	}, nil
}

// ---------------------------------------------------------------------------
// 关联新任务（终态后的重试/新要求）
// ---------------------------------------------------------------------------

// SuccessorTask 是终态后的关联新任务。
type SuccessorTask struct {
	// NewTaskID 是明确的新 Task ID（不复活原任务）。
	NewTaskID string
	// PredecessorID 是原（终态）Task ID（保留关联）。
	PredecessorID string
	// InheritedEvidence 是引用的先前证据（成果/验收引用）。
	InheritedEvidence []string
}

type SuccessorTemplate struct {
	Goal     string
	Scope    string
	Criteria string
}

// createSuccessorTask 在终态后建立关联新 Task。
// 不复活原任务：新 Task 有明确新 ID，保留与原任务的关联与先前证据。
// 返回明确新 ID；重复请求键幂等（旧客户端重复提交不创建多个 successor）。
func createSuccessorTask(tx *sql.Tx, predecessorID string, req ControlRequest, template SuccessorTemplate, now int64) (SuccessorTask, error) {
	// 核对原任务确实处于终态（不复活）。
	if err := ensureTaskNotTerminal(tx, predecessorID); err == nil {
		return SuccessorTask{}, errors.New("原任务未终结，不能建立 successor")
	} else if !errors.Is(err, ErrTaskTerminal) {
		return SuccessorTask{}, err
	}
	// 先持久化控制请求（重复请求键幂等）。
	if _, err := recordControlRequest(tx, req, 1, now); err != nil {
		if errors.Is(err, ErrConflict) {
			return SuccessorTask{}, err
		}
		// 幂等：同键同内容已在 control_records，查找既有 successor。
		var existing string
		lookupErr := tx.QueryRow(`SELECT detail FROM control_records WHERE request_key=?`, req.RequestKey).Scan(&existing)
		if lookupErr == nil && existing != "" {
			return SuccessorTask{NewTaskID: existing, PredecessorID: predecessorID}, nil
		}
	}
	// 建立新 Task（明确新 ID，不复活原任务）。
	newTaskID := id()
	conversationID := id()
	run := Run{ID: id(), ConversationID: conversationID, TaskID: newTaskID, Status: "queued"}
	if _, err := tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", conversationID, title(template.Goal), now); err != nil {
		return SuccessorTask{}, err
	}
	if err := insertTask(tx, run, template.Goal, template.Scope, Delivery{Criteria: &template.Criteria}); err != nil {
		return SuccessorTask{}, err
	}
	if _, err := tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,?,?)",
		run.ID, conversationID, newTaskID, "queued", now); err != nil {
		return SuccessorTask{}, err
	}
	res, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)",
		conversationID, run.ID, template.Goal)
	if err != nil {
		return SuccessorTask{}, err
	}
	message, err := res.LastInsertId()
	if err != nil {
		return SuccessorTask{}, err
	}
	if err := recordInputEvent(tx, message, "successor:"+req.RequestKey); err != nil {
		return SuccessorTask{}, err
	}
	// 保留与原任务的关联（successor 记录）。
	if _, err := tx.Exec(`INSERT INTO runtime_audit(task_id,event,actor,summary,detail_ref,occurred_at)
VALUES(?, 'successor_created', ?, ?, ?, ?)`,
		newTaskID, req.DecidedBy, fmt.Sprintf("由终态任务 %s 创建 successor", predecessorID), predecessorID, now); err != nil {
		return SuccessorTask{}, err
	}
	// 引用先前证据（原任务的成果/验收）。
	var evidence []string
	rows, err := tx.Query(`SELECT id FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 5`, predecessorID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var artifactID string
			if err := rows.Scan(&artifactID); err != nil {
				break
			}
			evidence = append(evidence, artifactID)
		}
	}
	return SuccessorTask{
		NewTaskID:         newTaskID,
		PredecessorID:     predecessorID,
		InheritedEvidence: evidence,
	}, nil
}

// 确保 context 引用保留。
var _ = context.Background
