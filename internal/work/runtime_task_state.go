package work

// Task 状态机、完成判定与终态约束。
//
// 本文件实现 §5.2 的事件/守卫/目标状态表：所有 Task 状态变更经一个
// 可测试入口 transitionTaskState，每次变更校验预期版本，写入原因、输入、
// 审计和需要的后续命令。完成由条件与收尾共同决定——模型返回完成、任一
// Step 完成或工具回执成功均不能直接使 Task 成功；成功/失败/已取消不复活，
// 后续工作创建有原任务关联的新 Task。
//
// 与旧执行器的关系：本状态机操作 的 task_states 权威记录，不直接
// 改写旧 tasks 五态；统一切换启动路径后由它驱动调度。个人助手的
// 成果保存与人工验收适配为当前 Task 的完成条件，不把文件
// 发布表改成通用操作表。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 状态变更事件（§5.2 的事件/守卫/目标状态表）
// ---------------------------------------------------------------------------

// TaskEvent 是触发状态变更的事件类型。
type TaskEvent string

const (
	EventCreate         TaskEvent = "create"           // 创建有效任务
	EventClaim          TaskEvent = "claim"            // 领取成功（可运行、预算、权限满足）
	EventNoRunnableStep TaskEvent = "no_runnable_step" // 没有可继续的 Step
	EventWaitResolved   TaskEvent = "wait_resolved"    // 等待条件满足
	EventPause          TaskEvent = "pause"            // 有效暂停请求
	EventResume         TaskEvent = "resume"           // 恢复已暂停任务（重评后）
	EventStepsComplete  TaskEvent = "steps_complete"   // 必需 Step 完成且操作有明确结果
	EventAcceptanceFail TaskEvent = "acceptance_fail"  // 验收未通过但允许继续
	EventAcceptancePass TaskEvent = "acceptance_pass"  // 验收通过
	EventCancel         TaskEvent = "cancel"           // 请求取消
	EventFail           TaskEvent = "fail"             // 判定最终失败
	EventFinalized      TaskEvent = "finalized"        // 收尾完成（运行时确认）
)

// ---------------------------------------------------------------------------
// 守卫证据（每次变更校验的输入）
// ---------------------------------------------------------------------------

// TaskGuard 是状态变更前必须核对的守卫证据。守卫判定在事务内重新读取
// 当前权威记录。
type TaskGuard struct {
	FinalState RuntimeTaskState // 可选的收尾终态，默认成功
	// ExpectedStorageRevision 是调用方持有的预期存储版本（乐观并发）。
	ExpectedStorageRevision int
	// HasRunnableSteps 是否仍有可运行 Step（EventNoRunnableStep 的守卫）。
	HasRunnableSteps bool
	// PendingOperations 未决 Operation 数（收尾/恢复的守卫；P3 接入实际值）。
	PendingOperations int
	// UnknownEffects 未知外部效果数（收尾不能静默消失的守卫）。
	UnknownEffects int
	// HasOpenWaits 是否仍有开放 Wait（EventWaitResolved/EventResume 的守卫）。
	HasOpenWaits bool
	// AcceptanceEvidence 验收证据是否有效（EventAcceptancePass 的守卫）。
	AcceptanceEvidence bool
	// Actor 是触发主体（审计）。
	Actor string
	// Input 是触发输入摘要（审计；不含敏感正文）。
	Input string
}

// ---------------------------------------------------------------------------
// 状态变更结果
// ---------------------------------------------------------------------------

// TaskTransition 是一次状态变更的结果。
type TaskTransition struct {
	From   RuntimeTaskState
	To     RuntimeTaskState
	Event  TaskEvent
	Reason string
	// FollowUp 是需要的后续命令（审计/核对/通知等，由调用方处理）。
	FollowUp []string
	// Rejected 为 true 表示守卫拒绝（非法跨态、旧版本、终态复活等）。
	Rejected  bool
	Rejection string
}

// ---------------------------------------------------------------------------
// 状态机入口
// ---------------------------------------------------------------------------

// transitionTaskState 是所有 Task 状态变更的唯一可测试入口。
// 在事务内：核对预期版本 → 校验事件/守卫/目标状态 → 写入新状态、原因、
// 审计和后续命令。非法跨态、旧版本和终态复活被拒绝。
func transitionTaskState(tx *sql.Tx, taskID string, event TaskEvent, guard TaskGuard, now int64) (TaskTransition, error) {
	// 读取当前权威状态（同事务重新读取，不依赖缓存）。
	current, err := readTaskState(tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		// 尚无 状态：仅允许 EventCreate 建立。
		if event != EventCreate {
			return TaskTransition{}, fmt.Errorf("任务 %s 尚无 V3 状态，事件 %s 不允许", taskID, event)
		}
		current = TaskState{TaskID: taskID, State: "", StorageRevision: 0}
	} else if err != nil {
		return TaskTransition{}, err
	}
	// 乐观并发：校验预期版本（终态后/旧版本的请求被拒）。
	if guard.ExpectedStorageRevision != current.StorageRevision {
		return TaskTransition{
			From: current.State, Rejected: true,
			Rejection: fmt.Sprintf("存储版本冲突：预期 %d，当前 %d", guard.ExpectedStorageRevision, current.StorageRevision),
		}, ErrState
	}
	// 终态不复活：终态后除审计外不允许任何状态变更。
	if current.State.IsTerminal() {
		return TaskTransition{
			From: current.State, Rejected: true,
			Rejection: fmt.Sprintf("终态 %s 不复活，后续工作应创建关联新 Task", current.State),
		}, ErrState
	}

	// 计算目标状态与守卫。
	target, reason, followUp, reject := evaluateTransition(current.State, event, guard)
	if reject != "" {
		return TaskTransition{
			From: current.State, Event: event, Rejected: true, Rejection: reject,
		}, ErrState
	}
	if !CanTransitionTask(current.State, target) && current.State != "" {
		// 双保险：守卫表与结构迁移表必须一致。
		return TaskTransition{
			From: current.State, Event: event, Rejected: true,
			Rejection: fmt.Sprintf("非法跨态：%s → %s（事件 %s）", current.State, target, event),
		}, ErrState
	}
	// EventCreate 时 current.State 为空，直接接受目标。
	if current.State == "" {
		if target != RuntimeTaskPending && target != RuntimeTaskWaiting {
			return TaskTransition{
				Event: event, Rejected: true,
				Rejection: fmt.Sprintf("创建只能进入待执行或等待中，得到 %s", target),
			}, ErrState
		}
	}

	// 写入新状态、原因、审计和后续命令（同事务）。
	newRevision := current.StorageRevision + 1
	newState := TaskState{
		TaskID:            taskID,
		State:             target,
		Reason:            reason,
		StorageRevision:   newRevision,
		ControlRevision:   current.ControlRevision,
		ReconcileRequired: current.ReconcileRequired,
		UpdatedAt:         now,
	}
	if err := upsertTaskState(tx, newState); err != nil {
		return TaskTransition{}, err
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID:          taskID,
		Event:           "task_state:" + string(event),
		Actor:           guard.Actor,
		StorageRevision: newRevision,
		ControlRevision: current.ControlRevision,
		Summary:         fmt.Sprintf("%s → %s：%s", current.State, target, reason),
		OccurredAt:      now,
	}); err != nil {
		return TaskTransition{}, err
	}
	return TaskTransition{
		From: current.State, To: target, Event: event, Reason: reason, FollowUp: followUp,
	}, nil
}

// ---------------------------------------------------------------------------
// 事件/守卫/目标状态表（§5.2）
// ---------------------------------------------------------------------------

// evaluateTransition 按 §5.2 表计算目标状态、原因与后续命令；
// 守卫不满足时返回拒绝原因。
func evaluateTransition(from RuntimeTaskState, event TaskEvent, guard TaskGuard) (to RuntimeTaskState, reason string, followUp []string, reject string) {
	switch event {
	case EventCreate:
		// 创建有效任务 → 待执行；依赖/事件/时间未满足 → 等待中。
		if guard.HasOpenWaits {
			return RuntimeTaskWaiting, "创建并登记等待", nil, ""
		}
		return RuntimeTaskPending, "创建有效任务", nil, ""

	case EventClaim:
		// 领取成功 → 执行中（守卫：可运行、预算、权限满足）。
		if from != RuntimeTaskPending {
			return "", "", nil, fmt.Sprintf("领取要求待执行，当前 %s", from)
		}
		return RuntimeTaskRunning, "领取成功", nil, ""

	case EventNoRunnableStep:
		// 没有可继续的 Step → 等待中，持久保存 Wait。
		if from != RuntimeTaskRunning && from != RuntimeTaskPending {
			return "", "", nil, fmt.Sprintf("无可运行步骤要求执行中/待执行，当前 %s", from)
		}
		if guard.HasRunnableSteps {
			return "", "", nil, "仍有可运行步骤，不能进入等待"
		}
		return RuntimeTaskWaiting, "没有可继续的步骤，登记等待", []string{"open_wait"}, ""

	case EventWaitResolved:
		// 等待条件满足 → 待执行，重新检查资格。
		if from != RuntimeTaskWaiting {
			return "", "", nil, fmt.Sprintf("唤醒要求等待中，当前 %s", from)
		}
		// 唤醒只生成需重新核对的就绪工作，不跳过当前合同/权限/未知操作检查。
		return RuntimeTaskPending, "等待条件满足，重新核对资格", []string{"recheck_eligibility"}, ""

	case EventPause:
		// 有效暂停请求 → 已暂停，停止新领取和新派发。
		if from != RuntimeTaskPending && from != RuntimeTaskRunning && from != RuntimeTaskWaiting && from != RuntimeTaskAcceptance {
			return "", "", nil, fmt.Sprintf("暂停要求非终态进行态，当前 %s", from)
		}
		// 已派发操作继续核对，回执可入账但不自动继续任务。
		return RuntimeTaskPaused, "已暂停，停止新领取和新派发", []string{"reconcile_dispatched"}, ""

	case EventResume:
		// 恢复已暂停任务 → 先核对未决在途，再按当前修订/权限/资产/Wait 重评。
		if from != RuntimeTaskPaused {
			return "", "", nil, fmt.Sprintf("恢复要求已暂停，当前 %s", from)
		}
		followUp := []string{"reconcile_pending"}
		// 恢复不会重用过期执行代际，未核对结果不能作为继续执行的依据。
		if guard.PendingOperations > 0 {
			followUp = append(followUp, "recheck_operations")
		}
		if guard.HasOpenWaits {
			return RuntimeTaskWaiting, "恢复重评：仍在等待", followUp, ""
		}
		return RuntimeTaskPending, "恢复重评：满足条件，回到待执行", followUp, ""

	case EventStepsComplete:
		// 必需 Step 完成且操作有明确结果 → 验收中。
		if from != RuntimeTaskRunning {
			return "", "", nil, fmt.Sprintf("完成判定要求执行中，当前 %s", from)
		}
		// 模型返回完成、任一 Step 完成或工具回执成功均不能直接使 Task 成功：
		// 必须有明确结果且无未决/未知操作。
		if guard.PendingOperations > 0 || guard.UnknownEffects > 0 {
			return "", "", nil, fmt.Sprintf("存在未决操作（%d）或未知效果（%d），不能进入验收",
				guard.PendingOperations, guard.UnknownEffects)
		}
		return RuntimeTaskAcceptance, "必需步骤完成，进入验收", nil, ""

	case EventAcceptanceFail:
		// 验收未通过但允许继续 → 回待执行并记录新依据。
		if from != RuntimeTaskAcceptance {
			return "", "", nil, fmt.Sprintf("验收未通过要求验收中，当前 %s", from)
		}
		return RuntimeTaskPending, "验收未通过，允许继续，记录新依据", nil, ""

	case EventAcceptancePass:
		// 验收通过 → 收尾中。
		if from != RuntimeTaskAcceptance {
			return "", "", nil, fmt.Sprintf("验收通过要求验收中，当前 %s", from)
		}
		if !guard.AcceptanceEvidence {
			return "", "", nil, "验收证据无效，不能通过"
		}
		return RuntimeTaskFinalizing, "验收通过，进入收尾", []string{"finalize_success"}, ""

	case EventCancel, EventFail:
		// 取消/最终失败 → 收尾中，核对全部在途操作、部分效果、占用与未完成事项。
		if from == "" {
			return "", "", nil, "空状态不能取消/失败"
		}
		followUp := []string{"finalize_cancel"}
		if event == EventFail {
			followUp = []string{"finalize_fail"}
		}
		// 未核对项不静默消失：收尾必须处理未知效果与占用。
		if guard.UnknownEffects > 0 {
			followUp = append(followUp, "reconcile_unknown_effects")
		}
		reason := "请求取消，进入收尾"
		if event == EventFail {
			reason = "判定最终失败，进入收尾"
		}
		return RuntimeTaskFinalizing, reason, followUp, ""

	case EventFinalized:
		// 收尾完成 → 仅运行时确认进入终态之一。
		if from != RuntimeTaskFinalizing {
			return "", "", nil, fmt.Sprintf("收尾完成要求收尾中，当前 %s", from)
		}
		// 未知效果保持待核对及收尾中，除非明确授权的人工接管已记录。
		if guard.UnknownEffects > 0 {
			return "", "", nil, fmt.Sprintf("仍有 %d 项未知效果，不能终结（需人工接管）", guard.UnknownEffects)
		}
		// 目标终态由收尾结果决定；这里要求调用方给出明确终态。
		// 成功/失败/已取消的区分由收尾记录决定，统一经 finalize 进入。
		if guard.FinalState == RuntimeTaskCancelled {
			return RuntimeTaskCancelled, "收尾完成，运行时确认取消", nil, ""
		}
		if guard.FinalState == RuntimeTaskFailed {
			return RuntimeTaskFailed, "收尾完成，运行时确认失败", nil, ""
		}
		return RuntimeTaskSucceeded, "收尾完成，运行时确认成功", nil, ""
	}
	return "", "", nil, fmt.Sprintf("未识别的事件 %q", event)
}

// ---------------------------------------------------------------------------
// 终态约束与关联新 Task
// ---------------------------------------------------------------------------

// ErrTaskTerminal 是终态后请求被拒的错误。
var ErrTaskTerminal = errors.New("任务已终结，请创建关联新任务")

// ensureTaskNotTerminal 核对任务不处于终态（终态不复活）。
func ensureTaskNotTerminal(q recordsQuerier, taskID string) error {
	state, err := readTaskState(q, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 无 V3 状态：尚未进入新状态机管理
	}
	if err != nil {
		return err
	}
	if state.State.IsTerminal() {
		return ErrTaskTerminal
	}
	return nil
}

// successorTaskDecision 判定终态后的后续请求（复用 的判定）。
// 返回"创建关联新 Task"的决定与原任务关联；不复活原任务。
func successorTaskDecision(q recordsQuerier, taskID string) (SuccessorDecision, RuntimeTaskState, error) {
	state, err := readTaskState(q, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		// 无 状态：按旧五态兼容。
		return SuccessorResumeOriginal, "", nil
	}
	if err != nil {
		return SuccessorRejected, "", err
	}
	return DecideTaskSuccessor(state.State), state.State, nil
}

// ---------------------------------------------------------------------------
// 个人助手完成条件适配
// ---------------------------------------------------------------------------

// personalCompletionGuard 把个人助手的成果保存与人工验收适配为当前 Task
// 的完成条件：成果已保存 + 人工验收通过 → 满足 EventAcceptancePass 的守卫。
// 不把文件发布表改成通用操作表——它只是验收证据的一种来源。
func personalCompletionGuard(q recordsQuerier, taskID string) (acceptanceEvidence bool, err error) {
	// 验收证据：存在有效的 task_acceptances 记录（人工验收）。
	var accepted bool
	if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_acceptances WHERE task_id=?)`, taskID).
		Scan(&accepted); err != nil {
		return false, err
	}
	return accepted, nil
}

// projectTaskFromRuntimeState derives the tasks projection row from the
// authoritative task_states state machine so both tables never drift.
func projectTaskFromRuntimeState(tx *sql.Tx, taskID string, state RuntimeTaskState, statusReason, reason string, stamp int64) error {
	if state == "" {
		row, err := readTaskState(tx, taskID)
		if err == nil {
			state = row.State
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	legacy := "active"
	var completedAt any
	switch state {
	case RuntimeTaskSucceeded:
		legacy = "completed"
		completedAt = stamp
	case RuntimeTaskCancelled:
		legacy = "cancelled"
		completedAt = stamp
	case RuntimeTaskFailed:
		legacy = "interrupted"
	case RuntimeTaskWaiting:
		legacy = "waiting_user"
	}
	if state == "" {
		_, err := tx.Exec(`UPDATE tasks SET status_reason=?,reason=?,revision=revision+1,updated_at=? WHERE id=?`,
			statusReason, reason, stamp, taskID)
		return err
	}
	_, err := tx.Exec(`UPDATE tasks SET status=?,status_reason=?,reason=?,completed_at=coalesce(?,completed_at),revision=revision+1,updated_at=? WHERE id=?`,
		legacy, statusReason, reason, completedAt, stamp, taskID)
	return err
}

// 确保 context 引用保留。
var _ = context.Background
