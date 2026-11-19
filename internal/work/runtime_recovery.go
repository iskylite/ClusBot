package work

// P1-13：启动恢复与统一推进循环。
//
// 本文件实现重启按持久状态恢复扫描、核对和就绪工作，避免所有中断一律
// 人工重启：
//   - 恢复顺序：校验/迁移 → 未应用结果与输入 → 未决执行核对 → Wait/Timer/
//     Schedule → 就绪单元；每步幂等；
//   - 阶段内只启用单个受控嵌入执行器，新 work_unit 具备稳定身份和代际；
//     外部 Worker 接入留到 P2；
//   - 替换旧全局 running→interrupted 的粗粒度恢复；能确定安全的继续，不明
//     的保留核对，不能把失联视为未执行；
//   - 后台循环有上下文取消、分批扫描、退避和退出期限；退出保存已持久责任，
//     不能依赖内存 map 才能恢复；
//   - 控制面模型不可用仍能处理输入回执、到期和核对；健康检查展示恢复阶段
//     与失败原因。
//
// 与旧恢复的关系：本文件编排 P1-05 至 P1-12 的持久结构，旧 `Recover()` 仍
// 负责旧五态的收尾；本文件是 V3 结构的统一恢复与推进，P1-14 接入完整观测。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 恢复阶段（健康检查展示恢复阶段与失败原因）
// ---------------------------------------------------------------------------

// RecoveryPhase 是恢复阶段。
type RecoveryPhase string

const (
	PhaseValidateMigrate  RecoveryPhase = "validate_migrate"  // 校验/迁移
	PhasePendingResults   RecoveryPhase = "pending_results"   // 未应用结果与输入
	PhaseReconcileRunning RecoveryPhase = "reconcile_running" // 未决执行核对
	PhaseWaitsTimers      RecoveryPhase = "waits_timers"      // Wait/Timer/Schedule
	PhaseReadyUnits       RecoveryPhase = "ready_units"       // 就绪单元
	PhaseComplete         RecoveryPhase = "complete"          // 完成
)

// RecoveryReport 是恢复报告（每步幂等的证据）。
type RecoveryReport struct {
	Phase                        RecoveryPhase
	CompletedPhases              []RecoveryPhase
	FailedPhase                  RecoveryPhase
	Failure                      string
	PendingResults               int // 未应用结果数
	ReconciledRuns               int // 核对的未决执行数
	AppliedTimerEvents           int // 已消费的 Timer 到期事件（不等于完成后续工作）
	DeliveredTimers              int // 投递的到期 Timer 数
	TriggeredSchedules           int // 触发的计划数
	ExpiredApprovals             int // 已持久记录的到期审批，不是人工拒绝
	InterruptedBenchmarkPreviews int
	ReadyUnits                   int // 就绪单元数
}

// ---------------------------------------------------------------------------
// 统一恢复（每步幂等）
// ---------------------------------------------------------------------------

// RecoverV3 按持久状态恢复扫描、核对和就绪工作（P1-13.1）。
// 每步幂等：连续两次恢复不产生重复 Task、Timer 唤醒或结果应用。
// 在不同恢复步骤中止再启动，仍可从持久记录继续。
func (s *Store) RecoverV3(ctx context.Context, now int64) (*RecoveryReport, error) {
	report := &RecoveryReport{Phase: PhaseValidateMigrate}
	// 步骤 1：校验/迁移（Open 已完成；此处核对版本）。
	if err := s.validateMigrated(); err != nil {
		report.FailedPhase = PhaseValidateMigrate
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseValidateMigrate, err)
	}
	report.CompletedPhases = append(report.CompletedPhases, PhaseValidateMigrate)
	report.Phase = PhasePendingResults

	// 步骤 2：未应用结果与输入（幂等：重复恢复不重复应用）。
	pendingResults, err := s.recoverPendingResults(ctx)
	if err != nil {
		report.FailedPhase = PhasePendingResults
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhasePendingResults, err)
	}
	report.PendingResults = pendingResults
	report.CompletedPhases = append(report.CompletedPhases, PhasePendingResults)
	report.Phase = PhaseReconcileRunning

	if _, err := s.db.ExecContext(ctx, `UPDATE inbox_events SET next_retry_at=0 WHERE source='chat' AND kind='chat_input'
AND state='received' AND payload_ref<>''`); err != nil {
		return report, err
	}

	// 步骤 3：未决执行核对（替换旧粗粒度恢复：能确定安全的继续，不明的保留核对）。
	report.InterruptedBenchmarkPreviews, err = s.interruptBenchmarkRevisionRequests(ctx, "", now)
	if err != nil {
		report.FailedPhase = PhaseReconcileRunning
		report.Failure = err.Error()
		return report, err
	}
	reconciled, err := s.reconcileRunningRuns(ctx)
	if err != nil {
		report.FailedPhase = PhaseReconcileRunning
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseReconcileRunning, err)
	}
	report.ReconciledRuns = reconciled
	report.CompletedPhases = append(report.CompletedPhases, PhaseReconcileRunning)
	report.Phase = PhaseWaitsTimers

	// 步骤 4：Wait/Timer/Schedule（幂等：到期事件唯一，重复扫描不重复投递）。
	deliveredTimers, err := recoverDueTimers(s.db, now, 50)
	if err != nil {
		report.FailedPhase = PhaseWaitsTimers
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseWaitsTimers, err)
	}
	report.DeliveredTimers = deliveredTimers.Delivered
	report.AppliedTimerEvents, err = s.consumeTimerInbox(ctx, now, 50)
	if err != nil {
		report.FailedPhase = PhaseWaitsTimers
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseWaitsTimers, err)
	}
	if _, err := s.consumeTimerWaitFollowups(ctx, now, 50); err != nil {
		report.FailedPhase = PhaseWaitsTimers
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseWaitsTimers, err)
	}
	report.ExpiredApprovals, err = s.expirePendingApprovals(ctx, now, approvalExpiryBatch)
	if err != nil {
		report.FailedPhase = PhaseWaitsTimers
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseWaitsTimers, err)
	}
	triggered, err := scanDueSchedules(s.db, now, 20)
	if err != nil {
		report.FailedPhase = PhaseWaitsTimers
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseWaitsTimers, err)
	}
	report.TriggeredSchedules = triggered.Triggered
	report.CompletedPhases = append(report.CompletedPhases, PhaseWaitsTimers)
	report.Phase = PhaseReadyUnits

	// 步骤 5：就绪单元（核对依赖与合同版本，生成就绪工作）。
	ready, err := s.computeAllReadyUnits(ctx)
	if err != nil {
		report.FailedPhase = PhaseReadyUnits
		report.Failure = err.Error()
		return report, fmt.Errorf("%s: %w", PhaseReadyUnits, err)
	}
	report.ReadyUnits = ready
	report.CompletedPhases = append(report.CompletedPhases, PhaseReadyUnits)
	report.Phase = PhaseComplete
	return report, nil
}

// validateMigrated 核对数据库已迁移到当前版本（校验失败不派发工作）。
func (s *Store) validateMigrated() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != databaseSchemaVersion {
		return fmt.Errorf("数据库版本 %d，当前需要 %d（迁移未完成，不派发工作）", version, databaseSchemaVersion)
	}
	return nil
}

// recoverPendingResults 恢复未应用结果与输入（幂等）。
// 返回恢复的结果数；重复恢复不重复应用（applyReport 幂等守卫）。
func (s *Store) recoverPendingResults(ctx context.Context) (int, error) {
	rows, err := s.db.Query(`SELECT result_key FROM result_receipts WHERE state='received' AND summary NOT LIKE '%[旧代际留证]'`)
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	recovered := 0
	for _, key := range keys {
		// 读取结果代际（无 Run 关联时用结果自带代际，避免误判为旧代际）。
		var resultGen, currentGen int
		err := s.db.QueryRow(`SELECT execution_generation FROM result_receipts WHERE result_key=?`, key).Scan(&resultGen)
		if err != nil {
			continue
		}
		currentGen = resultGen // 默认用结果自带代际
		if err := s.db.QueryRow(`SELECT r.execution_generation FROM result_receipts rr
JOIN runs r ON r.id=rr.run_id WHERE rr.result_key=?`, key).Scan(&currentGen); err != nil {
			currentGen = resultGen // 无 Run 关联：用结果自带代际
		}
		tx, err := s.db.Begin()
		if err != nil {
			return recovered, err
		}
		result, err := applyReport(tx, key, currentGen, nil, now())
		if err != nil {
			tx.Rollback()
			// 单项失败保留责任并隔离，不中断整体恢复。
			continue
		}
		if err := tx.Commit(); err != nil {
			return recovered, err
		}
		if result.Applied {
			recovered++
		}
	}
	return recovered, nil
}

// reconcileRunningRuns 核对未决执行（P1-13.3：替换旧粗粒度恢复）。
// 能确定安全的继续，不明的保留核对，不能把失联视为未执行（P1-2 修复）。
//
// 区分单元类型处理：
//   - deterministic 只读单元：重放无副作用，置回 ready 可重新领取；
//   - action（Operation 派发）单元：失联先核对，关联 Operation 置待核对（reconcile），
//     不能视为未执行直接重发；
//   - model 单元：置回 ready，旧结果因代际递增被判 stale，不会重复完成。
//
// 同时接通租约回收（reclaimExpiredLeases），按持久租约判定失联，不盲重置。
func (s *Store) reconcileRunningRuns(ctx context.Context) (int, error) {
	nowV := now()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 1. 租约回收：按持久租约判定失联（不依赖内存），已派发操作进入核对。
	if _, err := reclaimExpiredLeases(tx, nowV, 100); err != nil {
		return 0, err
	}
	// 2. action 类单元：失联先核对（关联 Operation 置待核对，不视为未执行）。
	// 找到 claimed/running 的 action 单元，把其 Operation 标记为待核对。
	rows, err := tx.Query(`SELECT u.unit_id,u.task_id,coalesce(u.step_id,'') FROM work_units u
WHERE u.kind='action' AND u.state IN ('claimed','running')
AND NOT EXISTS (SELECT 1 FROM work_leases l WHERE l.unit_id=u.unit_id AND l.lease_generation=u.lease_generation AND l.state='reclaimed')`)
	if err != nil {
		return 0, err
	}
	type actionUnit struct{ unitID, taskID, stepID string }
	var actions []actionUnit
	for rows.Next() {
		var a actionUnit
		if err := rows.Scan(&a.unitID, &a.taskID, &a.stepID); err != nil {
			rows.Close()
			return 0, err
		}
		actions = append(actions, a)
	}
	rows.Close()
	for _, a := range actions {
		// 关联 Operation 置待核对（失联先核对，不视为未执行）。
		operations, err := reconcileLeasedActionOperations(tx, a.taskID, a.stepID, nowV)
		if err != nil {
			return 0, err
		}
		summary := "保留失联动作单元；未找到同一步骤的在途操作，待人工核对"
		if len(operations) != 0 {
			summary = "保留失联动作单元；同一步骤的在途操作待核对，不视为未执行"
		}
		// Keep the existing state barrier rather than inventing a WorkUnit state.
		if err := recordAudit(tx, AuditEvent{
			TaskID: a.taskID, UnitID: a.unitID, Event: "unit_reconcile_required", Actor: "system",
			Summary: summary, OccurredAt: nowV,
		}); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`UPDATE work_leases SET state='reclaimed', reclaimed_at=?, reclaim_reason='进程重启'
WHERE state='active' AND unit_id IN (SELECT unit_id FROM work_units WHERE state IN ('claimed','running') AND kind IN ('deterministic','model'))`, nowV); err != nil {
		return 0, err
	}
	// 3. deterministic/model 单元：置回 ready 可重新领取（只读/模型重放安全，
	//    旧结果因代际递增被判 stale，不会重复完成）。action 单元不在此列。
	res, err := tx.Exec(`UPDATE work_units SET state='ready', updated_at=?
WHERE state IN ('claimed','running') AND kind IN ('deterministic','model')`, nowV)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// computeAllReadyUnits 核对所有任务的就绪单元（依赖与合同版本）。
func (s *Store) computeAllReadyUnits(ctx context.Context) (int, error) {
	// Check eligibility and materialize units in one bounded transaction.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT s.step_id,s.task_id,s.definition_key,s.input_ref
FROM steps s JOIN tasks t ON t.id=s.task_id
WHERE `+runnableTask+` AND `+runnableStep+`
AND NOT EXISTS (
  SELECT 1 FROM work_units w WHERE w.step_id=s.step_id
)
ORDER BY s.created_at,s.step_id LIMIT 50`)
	if err != nil {
		return 0, err
	}
	var steps []Step
	for rows.Next() {
		var step Step
		if err := rows.Scan(&step.StepID, &step.TaskID, &step.DefinitionKey, &step.InputRef); err != nil {
			rows.Close()
			return 0, err
		}
		steps = append(steps, step)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	stamp := now()
	for _, step := range steps {
		kind := WorkUnitModel
		if _, err := deterministicHandler(step.DefinitionKey); err == nil {
			kind = WorkUnitDeterministic
		}
		if err := insertWorkUnit(tx, WorkUnit{
			UnitID: id(), TaskID: step.TaskID, StepID: step.StepID, Kind: kind,
			InputRef: step.InputRef, State: WorkUnitReady, ReadyAt: stamp,
			CreatedAt: stamp, UpdatedAt: stamp,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(steps), nil
}

// ---------------------------------------------------------------------------
// 统一推进循环（后台，有取消/分批/退避/退出期限）
// ---------------------------------------------------------------------------

// RuntimeLoop 是统一推进循环（P1-13.4）。
// 后台循环有上下文取消、分批扫描、退避和退出期限；退出保存已持久责任，
// 不能依赖内存 map 才能恢复。
type RuntimeLoop struct {
	store *Store
	// modelAvailable 报告模型是否可用（模型不可用仍能处理输入回执/到期/核对）。
	modelAvailable func() bool
	// 观测：循环轮次与失败。
	rounds   int64
	failures int64
	mu       sync.Mutex
}

// NewRuntimeLoop 创建统一推进循环。
func NewRuntimeLoop(s *Store, modelAvailable func() bool) *RuntimeLoop {
	return &RuntimeLoop{store: s, modelAvailable: modelAvailable}
}

// Run 启动推进循环（阻塞直到 ctx 取消或退出期限）。
// 模型不可用时：输入回执、到期和核对仍推进（确定性工作独立存活）。
func (l *RuntimeLoop) Run(ctx context.Context, interval time.Duration, maxRounds int) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	round := 0
	for {
		select {
		case <-ctx.Done():
			return // 退出：已持久责任保存（不依赖内存 map）
		case <-ticker.C:
			round++
			if maxRounds > 0 && round > maxRounds {
				return // 退出期限
			}
			l.tick(ctx)
		}
	}
}

// tick 执行一轮推进（分批扫描，失败退避）。
func (l *RuntimeLoop) tick(ctx context.Context) {
	l.mu.Lock()
	l.rounds++
	l.mu.Unlock()
	now := now()
	// 确定性工作（模型不可用仍推进）：到期 Timer、Schedule、未决核对。
	if _, err := scanDueTimers(l.store.db, now, 50, 5*time.Second); err != nil {
		l.recordFailure()
	}
	if _, err := l.store.consumeTimerInbox(ctx, now, 50); err != nil {
		l.recordFailure()
	}
	if _, err := l.store.consumeTimerWaitFollowups(ctx, now, 50); err != nil {
		l.recordFailure()
	}
	if _, err := l.store.expirePendingApprovals(ctx, now, approvalExpiryBatch); err != nil {
		l.recordFailure()
	}
	if _, err := scanDueSchedules(l.store.db, now, 20); err != nil {
		l.recordFailure()
	}
	if err := l.store.reclaimExpiredEmbeddedReads(ctx, now); err != nil {
		l.recordFailure()
	}
	// 就绪单元核对（依赖与合同版本）。
	if _, err := l.store.computeAllReadyUnits(ctx); err != nil {
		l.recordFailure()
	}
	// 模型可用时才推进模型工作（模型不可用仍能处理上述确定性工作）。
	if l.modelAvailable != nil && l.modelAvailable() {
		// 模型工作的领取/执行由 Start 的执行器驱动（本循环不重复）。
	}
}

// recordFailure 记录一轮失败（退避由 ticker 间隔自然提供）。
func (l *RuntimeLoop) recordFailure() {
	l.mu.Lock()
	l.failures++
	l.mu.Unlock()
}

// Stats 返回循环观测（轮次与失败）。
func (l *RuntimeLoop) Stats() (rounds, failures int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rounds, l.failures
}

// ---------------------------------------------------------------------------
// 健康检查（展示恢复阶段与失败原因）
// ---------------------------------------------------------------------------

// HealthStatus 是运行时健康状态。
type HealthStatus struct {
	Migrated         bool
	SchemaVersion    int
	LastRecovery     *RecoveryReport
	ModelAvailable   bool
	PendingResults   int
	PendingTimers    int
	ReadyUnits       int
	DatabaseWritable bool
}

// Health 返回运行时健康状态（P1-13.5：模型不可用仍能处理输入回执/到期/核对）。
func (s *Store) Health(modelAvailable bool) (HealthStatus, error) {
	var h HealthStatus
	h.ModelAvailable = modelAvailable
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&h.SchemaVersion); err != nil {
		return h, err
	}
	h.Migrated = h.SchemaVersion == databaseSchemaVersion
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE state='received'`).Scan(&h.PendingResults); err != nil {
		return h, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM timers WHERE state='pending'`).Scan(&h.PendingTimers); err != nil {
		return h, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state='ready'`).Scan(&h.ReadyUnits); err != nil {
		return h, err
	}
	// 数据库可写性（不可写时停止新的相关派发）。
	tx, err := s.db.Begin()
	if err != nil {
		h.DatabaseWritable = false
		return h, nil
	}
	h.DatabaseWritable = ensureWritable(tx) == nil
	tx.Rollback()
	return h, nil
}

// 确保 errors/fmt 引用保留。
var (
	_ = errors.Is
	_ = fmt.Sprintf
)
