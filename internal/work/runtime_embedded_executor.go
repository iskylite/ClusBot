package work

// A2+A3：本地嵌入执行单元消费路径（只读领域任务闭环）。
//
// 本文件把 work_units 的领取、执行、回报接进启动后的执行链：
//   - 注册本地嵌入 Worker（宿主可信身份），循环领取 deterministic 就绪单元；
//   - 领取 → 执行确定性处理器（只读查询）→ 结果落盘（result_receipt）→
//     幂等应用 → 原子完成 step 与 work_unit；
//   - 重启后 claimed/running 单元由 RecoverRuntime 置回 ready，可重新领取；
//     结果回执幂等：重复送达只应用一次，迟到回报不重复完成。

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"time"
)

// EmbeddedExecutor 是本地嵌入执行器（消费确定性 work_unit）。
type EmbeddedExecutor struct {
	store      *Store
	workerID   string
	credential string
	leaseTTL   int64 // 毫秒
}

// StartEmbeddedExecutor 注册本地嵌入 Worker 并返回执行器（A2+A3）。
// 本地嵌入执行器也通过同一服务协议校验，不直接访问 SQLite（经 Store 方法）。
func (s *Store) StartEmbeddedExecutor(ctx context.Context) (*EmbeddedExecutor, error) {
	identity := embeddedWorkerIdentity()
	credential := identity.Credential
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	worker, err := registerWorker(tx, identity, now())
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &EmbeddedExecutor{store: s, workerID: worker.WorkerID, credential: credential, leaseTTL: 30000}, nil
}

// RunOnce 领取并执行一个就绪的确定性单元（返回是否执行了工作）。
// 幂等：无就绪单元返回 false；结果回执重复送达只应用一次。
func (e *EmbeddedExecutor) RunOnce(ctx context.Context) (bool, error) {
	nowV := now()
	// 1. 领取一个就绪单元（短事务条件更新）。
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	claim, err := claimWorkUnit(tx, ClaimRequest{
		WorkerID: e.workerID, Credential: e.credential,
		Kind: WorkUnitDeterministic, LeaseTTL: e.leaseTTL, QuotaAvailable: true,
	}, nowV)
	if err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // 无就绪单元
		}
		return false, err
	}
	// 单元标记 running。
	if _, err := tx.Exec(`UPDATE work_units SET state='running', updated_at=? WHERE unit_id=?`, nowV, claim.Unit.UnitID); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// Give the read a deadline and keep the parent context for its completion receipt.
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(e.leaseTTL/2)*time.Millisecond)
	defer cancel()
	result, execErr := e.executeClaimed(execCtx, claim)
	// 3. 结果落盘与完成（幂等）。
	return true, e.completeClaimed(ctx, claim, result, execErr)
}

// executeClaimed 执行已领取单元的确定性处理器。
func (e *EmbeddedExecutor) executeClaimed(ctx context.Context, claim ClaimResult) (DeterministicResult, error) {
	// 读取 step 定义以取得处理器标识与版本。
	var step Step
	var state string
	err := e.store.db.QueryRowContext(ctx, `SELECT step_id,task_id,contract_revision,definition_key,definition_version,
coalesce(input_ref,''),state FROM steps WHERE step_id=?`, claim.Unit.StepID).
		Scan(&step.StepID, &step.TaskID, &step.ContractRevision, &step.DefinitionKey, &step.DefinitionVersion,
			&step.InputRef, &state)
	if err != nil {
		return DeterministicResult{}, err
	}
	step.State = RuntimeStepState(state)
	handler, err := deterministicHandler(step.DefinitionKey)
	if err != nil {
		return DeterministicResult{}, err
	}
	return handler(ctx, DeterministicInput{
		TaskID: step.TaskID, StepID: step.StepID,
		ContractRevision: step.ContractRevision, DefinitionVersion: step.DefinitionVersion,
		InputRef: step.InputRef,
	})
}

// completeClaimed 把执行结果落盘并完成单元（幂等，迟到回报不重复完成）。
func (e *EmbeddedExecutor) completeClaimed(ctx context.Context, claim ClaimResult, result DeterministicResult, execErr error) error {
	nowV := now()
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 结果键稳定：unit_id + lease_generation（重复送达只应用一次）。
	resultKey := claim.Unit.UnitID + ":" + strconv.Itoa(claim.Lease.LeaseGeneration)
	summary, evidenceRef := result.Summary, result.OutputRef
	if execErr != nil {
		summary = "执行失败：" + execErr.Error()
		evidenceRef = ""
	}
	var valid bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM work_units w JOIN work_leases l ON l.unit_id=w.unit_id
WHERE w.unit_id=? AND w.state IN ('claimed','running') AND l.lease_id=? AND l.worker_id=?
AND l.state='active' AND l.lease_until>? AND w.lease_generation=l.lease_generation AND l.lease_generation=?)`,
		claim.Unit.UnitID, claim.Lease.LeaseID, e.workerID, nowV, claim.Lease.LeaseGeneration).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		if err := recordAudit(tx, AuditEvent{TaskID: claim.Unit.TaskID, UnitID: claim.Unit.UnitID,
			ResultKey: resultKey, Actor: e.workerID, Event: "stale_unit_report", Summary: "忽略已失效租约的回报", OccurredAt: nowV}); err != nil {
			return err
		}
		return tx.Commit()
	}
	executionGen := claim.Lease.LeaseGeneration
	if _, _, err := recordReport(tx, ResultReport{
		ResultKey: resultKey, UnitID: claim.Unit.UnitID, TaskID: claim.Unit.TaskID,
		ExecutionGeneration: executionGen, LeaseGeneration: executionGen,
		Summary: summary, EvidenceRef: evidenceRef, ReceivedAt: nowV,
	}); err != nil {
		return err
	}
	applied, err := applyReport(tx, resultKey, executionGen, nil, nowV)
	if err != nil {
		return err
	}
	if !applied.Applied {
		return tx.Commit()
	}
	if execErr != nil {
		// 失败：标记 step 与 unit 失败，任务进入失败收尾。
		if _, err := tx.Exec(`UPDATE steps SET state=?, error=?, updated_at=? WHERE step_id=?`,
			string(RuntimeStepFailed), execErr.Error(), nowV, claim.Unit.StepID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE work_units SET state='failed', error=?, updated_at=? WHERE unit_id=?`,
			execErr.Error(), nowV, claim.Unit.UnitID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE work_leases SET state='returned' WHERE lease_id=? AND state='active'`, claim.Lease.LeaseID); err != nil {
			return err
		}
		state, err := readTaskState(tx, claim.Unit.TaskID)
		if err != nil {
			return err
		}
		if state.State == RuntimeTaskPending || state.State == RuntimeTaskRunning {
			guard := TaskGuard{ExpectedStorageRevision: state.StorageRevision, Actor: e.workerID}
			if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventFail, guard, nowV); err != nil {
				return err
			}
			guard.ExpectedStorageRevision++
			guard.FinalState = RuntimeTaskFailed
			if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventFinalized, guard, nowV); err != nil {
				return err
			}
			if err := projectTaskFromRuntimeState(tx, claim.Unit.TaskID, RuntimeTaskFailed, "execution_failed", summary, nowV); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	// 3. 成功：完成 step、unit，租约归还，任务进入完成判定。
	if _, err := tx.Exec(`UPDATE steps SET state=?, result_ref=?, updated_at=? WHERE step_id=?`,
		string(RuntimeStepCompleted), evidenceRef, nowV, claim.Unit.StepID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE work_units SET state='completed', result_ref=?, updated_at=? WHERE unit_id=?`,
		evidenceRef, nowV, claim.Unit.UnitID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE work_leases SET state='returned' WHERE lease_id=? AND state='active'`, claim.Lease.LeaseID); err != nil {
		return err
	}
	// 任务状态推进：填真守卫，不吞错（P1-1）。
	// 守卫证据在事务内重新读取：必需 Step 是否全完成、是否有未决 Operation/未知效果。
	state, stateErr := readTaskState(tx, claim.Unit.TaskID)
	if stateErr != nil {
		return stateErr
	}
	if state.State == RuntimeTaskPending {
		// 领取（→running）。守卫拒绝则整体回滚，结果留 received 供恢复重试。
		if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventClaim, TaskGuard{ExpectedStorageRevision: state.StorageRevision, Actor: e.workerID}, nowV); err != nil {
			return err
		}
		state.StorageRevision++
		state.State = RuntimeTaskRunning
	}
	if state.State == RuntimeTaskRunning {
		// 必需 Step 全完成判定：仍存在的非可选未完成 Step 数。
		var incompleteRequired int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM steps WHERE task_id=? AND optional=0 AND state<>'completed'`,
			claim.Unit.TaskID).Scan(&incompleteRequired); err != nil {
			return err
		}
		// 未决 Operation 数（本只读链路恒 0，但按真值填守卫）。
		var pendingOps int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE task_id=? AND state IN ('dispatched','running','reconcile')`,
			claim.Unit.TaskID).Scan(&pendingOps); err != nil {
			return err
		}
		if incompleteRequired == 0 && pendingOps == 0 {
			// 必需 Step 全完成且无未决操作 → 验收中。守卫拒绝则回滚不静默。
			if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventStepsComplete, TaskGuard{
				ExpectedStorageRevision: state.StorageRevision, PendingOperations: pendingOps, UnknownEffects: 0, Actor: e.workerID,
			}, nowV); err != nil {
				return err
			}
			// A single read is accepted on its persisted snapshot.
			var isAssetQuery bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM domain_query_tasks WHERE task_id=?)`, claim.Unit.TaskID).Scan(&isAssetQuery); err != nil {
				return err
			}
			if isAssetQuery {
				guard := TaskGuard{ExpectedStorageRevision: state.StorageRevision + 1, AcceptanceEvidence: evidenceRef != "", Actor: e.workerID}
				if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventAcceptancePass, guard, nowV); err != nil {
					return err
				}
				guard.ExpectedStorageRevision++
				if _, err := transitionTaskState(tx, claim.Unit.TaskID, EventFinalized, guard, nowV); err != nil {
					return err
				}
				if err := projectTaskFromRuntimeState(tx, claim.Unit.TaskID, RuntimeTaskSucceeded, "completed", summary, nowV); err != nil {
					return err
				}
			}

		}
		// 仍有未完成必需 Step：任务保持 running，由后续 Step 完成时再推进。
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: claim.Unit.TaskID, StepID: claim.Unit.StepID, UnitID: claim.Unit.UnitID,
		Event: "domain_query_completed", Actor: e.workerID, Summary: summary, OccurredAt: nowV,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// Run 循环消费确定性单元直到 ctx 取消（A2+A3 启动后的执行链）。
// 无就绪单元时退避；错误退避不中断循环。
func (e *EmbeddedExecutor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 连续消费直到无就绪单元。
			for {
				worked, err := e.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					slog.Error("执行本地工作单元失败", "error", err)
				}
				if err != nil || !worked {
					break
				}
			}
		}
	}
}
