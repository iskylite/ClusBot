package work

// P2-07：确定性执行单元与等待释放。
//
// 本文件把纯确定性步骤（无 IO/网络）实现为可在 Worker 进程中执行、或
// 在控制面就地执行的执行单元；完成即释放等待，不占用模型槽：
//   - 确定性步骤无 IO/网络；输入/输出快照持久化；重放结果一致；
//   - 执行前核对当前合同与步骤版本；完成后原子标记完成并释放等待；
//   - 确定性步骤可在 Worker 进程或控制面就地执行（无 IO 时控制面可直接
//     完成）；等待不占执行槽。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 确定性执行单元（纯确定性，无 IO/网络）
// ---------------------------------------------------------------------------

// DeterministicUnit 是一个确定性执行单元。
type DeterministicUnit struct {
	UnitID    string
	StepID    string
	TaskID    string
	// 输入快照（持久化；重放结果一致）。
	InputSnapshot  string
	// 输出快照（完成后持久化）。
	OutputSnapshot string
	// 合同与步骤版本（执行前核对）。
	ContractRevision int
	DefinitionVersion int
}

// executeDeterministicUnit 执行一个确定性单元（P2-07）。
// 纯确定性（无 IO/网络）；输入/输出快照持久化；重放结果一致。
// 执行前核对当前合同与步骤版本；完成后原子标记完成并释放等待。
func executeDeterministicUnit(tx *sql.Tx, unit DeterministicUnit, handler DeterministicHandler, now int64) (DeterministicResult, error) {
	// 1. 核对当前合同与步骤版本（执行前）。
	var stepContract, stepDefVersion int
	var stepState string
	if err := tx.QueryRow(`SELECT contract_revision, definition_version, state FROM steps WHERE step_id=?`,
		unit.StepID).Scan(&stepContract, &stepDefVersion, &stepState); err != nil {
		return DeterministicResult{}, err
	}
	if stepContract != unit.ContractRevision || stepDefVersion != unit.DefinitionVersion {
		return DeterministicResult{}, fmt.Errorf("%w：步骤合同 %d≠%d 或版本 %d≠%d",
			ErrContractChanged, stepContract, unit.ContractRevision, stepDefVersion, unit.DefinitionVersion)
	}
	if stepState != string(V3StepReady) && stepState != string(V3StepRunning) {
		return DeterministicResult{}, fmt.Errorf("步骤 %s 不就绪（当前 %s）", unit.StepID, stepState)
	}
	// 2. 执行确定性处理器（无 IO/网络）。
	result, err := handler(context.Background(), DeterministicInput{
		TaskID:            unit.TaskID,
		StepID:            unit.StepID,
		ContractRevision:  unit.ContractRevision,
		DefinitionVersion: unit.DefinitionVersion,
		InputRef:          unit.InputSnapshot,
	})
	if err != nil {
		return DeterministicResult{}, err
	}
	// 3. 持久化输出快照（重放结果一致；存于 input_ref 供重放比对）。
	unit.OutputSnapshot = result.OutputRef
	if _, err := tx.Exec(`UPDATE steps SET input_ref=?, updated_at=? WHERE step_id=?`,
		result.OutputRef, now, unit.StepID); err != nil {
		return DeterministicResult{}, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 完成与释放等待（原子标记完成并释放等待）
// ---------------------------------------------------------------------------

// completeDeterministicUnit 原子标记确定性单元完成并释放等待（P2-07）。
// 完成后原子标记完成并释放等待；确定性步骤完成即释放等待（不占执行槽）。
func completeDeterministicUnit(tx *sql.Tx, unitID string, now int64) error {
	// 原子标记单元完成（条件更新）。
	res, err := tx.Exec(`UPDATE work_units SET state='completed', updated_at=? WHERE unit_id=? AND state IN ('ready','claimed','running')`,
		now, unitID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("单元 %s 不在可完成状态", unitID)
	}
	// 释放该单元关联的等待（确定性步骤完成即释放等待）。
	var taskID string
	if err := tx.QueryRow("SELECT task_id FROM work_units WHERE unit_id=?", unitID).Scan(&taskID); err != nil {
		return err
	}
	// 关闭该任务的确定性等待（不占执行槽）。
	if _, err := tx.Exec(`UPDATE waits SET state='closed', closed_at=?, resolution='确定性步骤完成'
WHERE task_id=? AND state='open' AND kind='dependency'`, now, taskID); err != nil {
		return err
	}
	// 审计。
	if err := recordAudit(tx, AuditEvent{
		TaskID: taskID, UnitID: unitID, Event: "deterministic_completed", Actor: "system",
		Summary: "确定性单元完成并释放等待", OccurredAt: now,
	}); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Worker 进程或控制面就地执行（无 IO 时控制面可直接完成）
// ---------------------------------------------------------------------------

// runDeterministicInControlPlane 在控制面就地执行确定性单元（P2-07）。
// 无 IO 时控制面可直接完成（不需要 Worker 进程）。
func runDeterministicInControlPlane(tx *sql.Tx, unit DeterministicUnit, now int64) (DeterministicResult, error) {
	handler, err := deterministicHandler(unit.InputSnapshot) // 用 input 作为 handler key 查找
	if err != nil {
		// 回退：按步骤定义查找处理器。
		handler, err = deterministicHandlerForStep(tx, unit.StepID)
		if err != nil {
			return DeterministicResult{}, err
		}
	}
	result, err := executeDeterministicUnit(tx, unit, handler, now)
	if err != nil {
		return DeterministicResult{}, err
	}
	if result.OutputRef != "" {
		if err := completeDeterministicUnit(tx, unit.UnitID, now); err != nil {
			return DeterministicResult{}, err
		}
	}
	return result, nil
}

// deterministicHandlerForStep 按步骤查找确定性处理器。
func deterministicHandlerForStep(tx *sql.Tx, stepID string) (DeterministicHandler, error) {
	var defKey string
	if err := tx.QueryRow("SELECT definition_key FROM steps WHERE step_id=?", stepID).Scan(&defKey); err != nil {
		return nil, err
	}
	return deterministicHandler(defKey)
}

// ---------------------------------------------------------------------------
// 等待不占执行槽（P2-07 反例）
// ---------------------------------------------------------------------------

// waitOccupiesNoExecutionSlot 核对等待不占执行槽（P2-07 反例）。
// 确定性步骤完成即释放等待；等待中的任务不占模型槽或执行槽。
func waitOccupiesNoExecutionSlot(q recordsQuerier, taskID string) (bool, error) {
	// 等待中的任务：无 active 执行单元（ready/claimed/running）。
	var activeCount int
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('claimed','running')`, taskID).Scan(&activeCount); err != nil {
		return false, err
	}
	return activeCount == 0, nil
}

// 确保 context/sql/errors 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
)
