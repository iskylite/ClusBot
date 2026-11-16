package work

// P2-09：多任务 Chat 控制与详情展示兼容。
//
// 本文件实现用户从原 Chat 看清每项工作、等待、工作者与未决责任：
//   - 一次消息中的查询、补充、暂停按明确 Task ID/意图处理；模糊归属澄清，
//     不覆盖其他任务；
//   - API 返回多活跃 Run/Step、当前 Wait、控制修订和结果；保留旧历史查询的
//     兼容语义；
//   - 界面区分已受理、执行中、等待、收尾和终态；显示待核对原因及下一责任人，
//     不把 Worker 消失显示成任务成功；
//   - 合同变更/终态后新 Task 的链接和返回导航清楚；无需新建看板；
//   - 按既有 UI 基准验证刷新、断线重连、重复点击、多个任务同时更新的表现。
//
// 与现有 Detail 的关系：本文件在现有 `Store.Detail` 之上聚合 V3 结构
// （steps/waits/units/controls/receipts），供 API 返回；前端展示在 P2-09 的
// 界面部分另行实现（本文件提供数据）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 多任务详情聚合（API 返回多活跃 Run/Step、Wait、控制修订、结果）
// ---------------------------------------------------------------------------

// TaskDetailV3 是一个任务的 V3 聚合详情（供 API 返回）。
type TaskDetailV3 struct {
	TaskID string
	// 状态与阶段（区分已受理、执行中、等待、收尾和终态）。
	State    string
	Phase    string // admitted/running/waiting/finalizing/terminal
	// 多活跃 Run/Step。
	ActiveSteps []Step
	ReadyUnits  []WorkUnit
	// 当前 Wait。
	OpenWaits []Wait
	// 控制修订（暂停/恢复/取消）。
	ControlRevision int
	Controls        []ControlRecord
	// 结果与回执。
	PendingReceipts int
	// 待核对原因与下一责任人（不把 Worker 消失显示成任务成功）。
	PendingReason string
	NextOwner     string
	// 关联（合同变更/终态后新 Task 链接）。
	SuccessorID   string
	PredecessorID string
}

// readTaskDetailV3 聚合一个任务的 V3 详情（P2-09.2）。
// 保留旧历史查询的兼容语义；多活跃 Run/Step、当前 Wait、控制修订和结果聚合。
func readTaskDetailV3(q recordsQuerier, taskID string) (TaskDetailV3, error) {
	var d TaskDetailV3
	d.TaskID = taskID
	// 状态与阶段。
	state, err := readTaskState(q, taskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	if err == nil {
		d.State = string(state.State)
		d.Phase = v3PhaseForState(state.State)
		d.ControlRevision = state.ControlRevision
	}
	// 多活跃 Step（ready/running）。
	steps, err := listSteps(q, taskID)
	if err == nil {
		for _, s := range steps {
			if s.State == V3StepReady || s.State == V3StepRunning || s.State == V3StepBlocked {
				d.ActiveSteps = append(d.ActiveSteps, s)
			}
		}
	}
	// 就绪单元（Worker 领取候选）。
	rows, err := q.Query(`SELECT unit_id,task_id,step_id,kind,lease_generation,ready_at,state,input_ref,created_at,updated_at
FROM work_units WHERE task_id=? AND state IN ('ready','claimed','running')`, taskID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u WorkUnit
			var stepID sql.NullString
			if err := rows.Scan(&u.UnitID, &u.TaskID, &stepID, &u.Kind, &u.LeaseGeneration,
				&u.ReadyAt, &u.State, &u.InputRef, &u.CreatedAt, &u.UpdatedAt); err != nil {
				break
			}
			u.StepID = stepID.String
			d.ReadyUnits = append(d.ReadyUnits, u)
		}
	}
	// 当前 Wait。
	openWaits, err := readOpenWaits(q, taskID)
	if err == nil {
		d.OpenWaits = openWaits
	}
	// 控制修订（暂停/恢复/取消记录）。
	controlRows, err := q.Query(`SELECT record_id,task_id,request_key,decided_by,kind,control_revision,
target_contract_revision,decision,detail,created_at FROM control_records WHERE task_id=? ORDER BY created_at DESC LIMIT 10`, taskID)
	if err == nil {
		defer controlRows.Close()
		for controlRows.Next() {
			var r ControlRecord
			if err := controlRows.Scan(&r.RecordID, &r.TaskID, &r.RequestKey, &r.DecidedBy, &r.Kind,
				&r.ControlRevision, &r.TargetContractRevision, &r.Decision, &r.Detail, &r.CreatedAt); err != nil {
				break
			}
			d.Controls = append(d.Controls, r)
		}
	}
	// 待回执（未决）。
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE task_id=? AND state='received'`, taskID).Scan(&d.PendingReceipts); err != nil {
		return d, err
	}
	// 待核对原因与下一责任人。
	d.PendingReason, d.NextOwner = pendingReasonForDetail(d)
	// 关联（successor/predecessor 从审计查询）。
	var successor sql.NullString
	if err := q.QueryRow(`SELECT task_id FROM runtime_audit WHERE detail_ref=? AND event='successor_created' ORDER BY occurred_at DESC LIMIT 1`,
		taskID).Scan(&successor); err == nil {
		d.SuccessorID = successor.String
	}
	var predecessor sql.NullString
	if err := q.QueryRow(`SELECT detail_ref FROM runtime_audit WHERE task_id=? AND event='successor_created' LIMIT 1`,
		taskID).Scan(&predecessor); err == nil {
		d.PredecessorID = predecessor.String
	}
	return d, nil
}

// v3PhaseForState 把 V3 状态映射为展示阶段。
func v3PhaseForState(state V3TaskState) string {
	switch state {
	case V3TaskPending:
		return "admitted"
	case V3TaskRunning:
		return "running"
	case V3TaskWaiting:
		return "waiting"
	case V3TaskPaused:
		return "paused"
	case V3TaskAcceptance:
		return "acceptance"
	case V3TaskFinalizing:
		return "finalizing"
	case V3TaskSucceeded, V3TaskFailed, V3TaskCancelled:
		return "terminal"
	default:
		return "admitted"
	}
}

// pendingReasonForDetail 计算待核对原因与下一责任人。
// 不把 Worker 消失显示成任务成功：有待核对项时明确显示原因与责任人。
func pendingReasonForDetail(d TaskDetailV3) (reason, owner string) {
	if len(d.OpenWaits) > 0 {
		return fmt.Sprintf("等待 %d 项条件", len(d.OpenWaits)), "等待方"
	}
	if d.PendingReceipts > 0 {
		return fmt.Sprintf("有 %d 个待核对结果", d.PendingReceipts), "运行时核对"
	}
	for _, u := range d.ReadyUnits {
		if u.State == "claimed" || u.State == "running" {
			return "执行中（Worker 持有租约）", "执行 Worker"
		}
	}
	if len(d.ActiveSteps) > 0 {
		return "有就绪步骤待领取", "调度器"
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// 多意图控制（查询、补充、暂停按明确 Task ID/意图处理）
// ---------------------------------------------------------------------------

// ChatControlCommand 是一条 Chat 控制命令（按明确 Task ID/意图处理）。
type ChatControlCommand struct {
	Intent   string // query/supplement/pause/resume/cancel
	TaskID   string
	Fragment string
	// Ambiguous 为 true 表示归属不明确（需澄清，不覆盖其他任务）。
	Ambiguous bool
	ClarifyReason string
}

// resolveChatControl 解析一条 Chat 控制命令（P2-09.1）。
// 按明确 Task ID/意图处理；模糊归属澄清，不覆盖其他任务。
func resolveChatControl(intent RouterIntent, activeTasks []string) ChatControlCommand {
	cmd := ChatControlCommand{
		Intent:   intent.Kind,
		TaskID:   intent.TaskID,
		Fragment: intent.Fragment,
	}
	// 模糊归属：无明确 Task ID 且有多个活跃任务 → 澄清（不覆盖其他任务）。
	if intent.TaskID == "" && len(activeTasks) > 1 {
		cmd.Ambiguous = true
		cmd.ClarifyReason = fmt.Sprintf("有 %d 个活跃任务，请明确要操作哪一个", len(activeTasks))
		return cmd
	}
	// 无明确 Task ID 且只有一个活跃任务 → 默认归属该任务。
	if intent.TaskID == "" && len(activeTasks) == 1 {
		cmd.TaskID = activeTasks[0]
	}
	return cmd
}

// ---------------------------------------------------------------------------
// 重复控制幂等（不重复扣预算/创建新 Task）
// ---------------------------------------------------------------------------

// deduplicateControl 核对重复控制（P2-09 反例）。
// 重复控制不重复扣预算/创建新 Task（请求键幂等）。
func deduplicateControl(q recordsQuerier, requestKey string) (exists bool, existingDecision string, err error) {
	err = q.QueryRow(`SELECT decision FROM control_records WHERE request_key=?`, requestKey).Scan(&existingDecision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, existingDecision, nil
}

// ---------------------------------------------------------------------------
// 版本冲突提示（旧界面版本冲突有可理解提示）
// ---------------------------------------------------------------------------

// ErrRevisionConflict 是版本冲突的错误（旧界面版本冲突有可理解提示）。
var ErrRevisionConflict = errors.New("版本冲突：任务已被更新")

// checkRevisionConflict 核对控制请求的修订版本（P2-09 反例）。
// 旧界面版本冲突有可理解提示（不接受旧修订的控制）。
func checkRevisionConflict(q recordsQuerier, taskID string, expectedRevision int) error {
	state, err := readTaskState(q, taskID)
	if err != nil {
		return err
	}
	if expectedRevision > 0 && expectedRevision != state.ControlRevision {
		return fmt.Errorf("%w：期望控制修订 %d，当前 %d（任务已被更新，请刷新）",
			ErrRevisionConflict, expectedRevision, state.ControlRevision)
	}
	return nil
}

// 确保 context/sql/errors 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
)
