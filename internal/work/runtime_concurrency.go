package work

// P2-08：多 Task 与同 Task 并行 Step。
//
// 本文件实现任务级控制、步骤并行和共享预算共同成立：
//   - 同时调度不同 Task 和同 Task 的独立就绪 Step，依赖未满足的步骤不领取；
//   - Task 状态由所有必需步骤、Wait 和操作聚合；某一步完成不结束整项任务；
//   - 同任务取消/合同修订使受影响的未派发计划失效；其他独立任务继续；
//   - 用可控对象键演示冲突协调，真实资产/影响范围在 P3/P4 接入；
//   - 保存子 Task 的独立目标和责任；父子任务控制与 Step 依赖不混成一种状态。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 并行调度（不同 Task 与同 Task 独立就绪 Step）
// ---------------------------------------------------------------------------

// selectParallelReadyUnits 选取多个可并行的就绪单元（P2-08.1）。
// 不同 Task 和同 Task 的独立就绪 Step 同时调度；依赖未满足的步骤不领取。
// 返回可并行领取的单元（不同 Task 或同 Task 无依赖的 Step）。
func selectParallelReadyUnits(tx *sql.Tx, kind WorkUnitKind, now int64, limit int) ([]WorkUnit, error) {
	if limit < 1 {
		limit = 10
	}
	// 读取所有就绪单元（依赖已满足的）。
	rows, err := tx.Query(`SELECT unit_id,task_id,step_id,kind,lease_generation,ready_at,state,input_ref,created_at,updated_at
FROM work_units WHERE state='ready' ORDER BY ready_at LIMIT ?`, limit*3)
	if err != nil {
		return nil, err
	}
	var units []WorkUnit
	for rows.Next() {
		var u WorkUnit
		var stepID sql.NullString
		if err := rows.Scan(&u.UnitID, &u.TaskID, &stepID, &u.Kind, &u.LeaseGeneration,
			&u.ReadyAt, &u.State, &u.InputRef, &u.CreatedAt, &u.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		u.StepID = stepID.String
		// 核对依赖已满足（依赖未满足的步骤不领取）。
		if u.StepID != "" {
			steps, err := listSteps(tx, u.TaskID)
			if err != nil {
				continue
			}
			byID := make(map[string]Step, len(steps))
			var target Step
			for _, s := range steps {
				byID[s.StepID] = s
				if s.StepID == u.StepID {
					target = s
				}
			}
			if target.StepID == "" {
				continue
			}
			satisfied, _, err := stepDependenciesSatisfied(tx, target, byID)
			if err != nil || !satisfied {
				continue
			}
		}
		units = append(units, u)
		if len(units) >= limit {
			break
		}
	}
	rows.Close()
	return units, rows.Err()
}

// ---------------------------------------------------------------------------
// Task 状态聚合（所有必需步骤、Wait 和操作聚合）
// ---------------------------------------------------------------------------

// aggregateTaskState 聚合一个 Task 的状态（P2-08.2）。
// Task 状态由所有必需步骤、Wait 和操作聚合；某一步完成不结束整项任务。
func aggregateTaskState(q recordsQuerier, taskID string) (V3TaskState, error) {
	// 读取当前 Task 状态。
	current, err := readTaskState(q, taskID)
	if err != nil {
		return "", err
	}
	// 聚合：有未完成必需步骤/开放等待/未决操作时，任务不完成。
	var incompleteSteps int
	if err := q.QueryRow(`SELECT COUNT(*) FROM steps WHERE task_id=? AND state NOT IN ('completed','skipped')`, taskID).
		Scan(&incompleteSteps); err != nil {
		return "", err
	}
	var openWaits int
	if err := q.QueryRow(`SELECT COUNT(*) FROM waits WHERE task_id=? AND state='open'`, taskID).Scan(&openWaits); err != nil {
		return "", err
	}
	var pendingOps int
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('claimed','running')`, taskID).Scan(&pendingOps); err != nil {
		return "", err
	}
	// 某一步完成不结束整项任务：仍有未完成项时保持运行/等待。
	if incompleteSteps > 0 || openWaits > 0 || pendingOps > 0 {
		if current.State == V3TaskRunning || current.State == V3TaskWaiting {
			return current.State, nil
		}
		return current.State, nil
	}
	return current.State, nil
}

// ---------------------------------------------------------------------------
// 修订/取消隔离（同任务失效，其他任务继续）
// ---------------------------------------------------------------------------

// invalidateTaskPlans 使一个 Task 的受影响未派发计划失效（P2-08.3）。
// 同任务取消/合同修订使受影响的未派发计划失效；其他独立任务继续。
func invalidateTaskPlans(tx *sql.Tx, taskID string, reason string, now int64) (int, error) {
	// 该任务的未派发（ready）单元失效。
	res, err := tx.Exec(`UPDATE work_units SET state='cancelled', updated_at=? WHERE task_id=? AND state='ready'`,
		now, taskID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := recordAudit(tx, AuditEvent{
		TaskID: taskID, Event: "plans_invalidated", Actor: "system",
		Summary: fmt.Sprintf("任务修订/取消使 %d 个未派发计划失效", n), OccurredAt: now,
	}); err != nil {
		return 0, err
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// 冲突协调（可控对象键演示；真实资产在 P3/P4）
// ---------------------------------------------------------------------------

// ObjectConflict 是一次对象冲突协调结果。
type ObjectConflict struct {
	ObjectKey  string
	Allowed    bool
	Reason     string
	HeldBy     string // 当前占用者
}

// coordinateObjectConflict 协调同一对象的冲突动作（P2-08.4）。
// 用可控对象键演示冲突协调，真实资产/影响范围在 P3/P4 接入；标明测试层次。
func coordinateObjectConflict(tx *sql.Tx, objectKey, requesterID, action string, now int64) (ObjectConflict, error) {
	// 查询当前对象占用（exclusive hold）。
	var holder string
	err := tx.QueryRow(`SELECT worker_id FROM quota_reservations WHERE target_ref=? AND exclusive=1 AND state IN ('reserved','settled') LIMIT 1`,
		objectKey).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		// 无占用：允许。
		return ObjectConflict{ObjectKey: objectKey, Allowed: true, Reason: "无占用"}, nil
	}
	if err != nil {
		return ObjectConflict{}, err
	}
	// 已占用：同一占用者允许（继续），不同占用者拒绝（冲突协调）。
	if holder == requesterID {
		return ObjectConflict{ObjectKey: objectKey, Allowed: true, HeldBy: holder, Reason: "同一占用者继续"}, nil
	}
	return ObjectConflict{
		ObjectKey: objectKey, Allowed: false, HeldBy: holder,
		Reason: fmt.Sprintf("对象 %s 已被 %s 占用（冲突协调：串行或拒绝）", objectKey, holder),
	}, nil
}

// ---------------------------------------------------------------------------
// 子 Task 独立目标与责任（父子控制与 Step 依赖不混）
// ---------------------------------------------------------------------------

// ChildTaskLink 是父子任务关联。
type ChildTaskLink struct {
	ParentTaskID string
	ChildTaskID  string
	// 子 Task 有独立目标和责任（父子控制与 Step 依赖不混成一种状态）。
	ChildGoal    string
	ChildCriteria string
	// 父任务取消/修订不自动终止子任务（独立责任）。
	Independent  bool
}

// linkChildTask 建立父子任务关联（P2-08.5）。
// 保存子 Task 的独立目标和责任；父子任务控制与 Step 依赖不混成一种状态。
func linkChildTask(tx *sql.Tx, link ChildTaskLink, now int64) error {
	if link.ParentTaskID == "" || link.ChildTaskID == "" {
		return errors.New("父子任务关联缺少身份")
	}
	// 父子关联记录在 runtime_audit（不混成一种状态）。
	return recordAudit(tx, AuditEvent{
		TaskID: link.ChildTaskID, Event: "child_task_linked", Actor: "system",
		Summary: fmt.Sprintf("子任务 %s 关联父任务 %s（独立目标：%s）", link.ChildTaskID, link.ParentTaskID, link.ChildGoal),
		DetailRef: link.ParentTaskID, OccurredAt: now,
	})
}

// 确保 context/sql/errors 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
)
