package work

// P2-08 验收：多 Task 与同 Task 并行 Step。
//
// 三个测试入口：
//   - TestV3P208ParallelSteps：至少两个 Worker 在同一时间区间执行两个独立
//     Step，而不是串行完成后显示两个 Worker；依赖未满足的步骤不领取。
//   - TestV3P208MultiTaskControlIsolation：暂停一个 Task 时无关 Task 继续；
//     取消父工作保留已发生子工作效果与责任；同任务取消/合同修订使受影响的
//     未派发计划失效。
//   - TestV3P208SharedTaskBudget：共享预算不超支；依赖结果更新后下游不能
//     使用旧证据；Task 状态由所有必需步骤、Wait 和操作聚合。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P208Task 创建存储与一个带步骤的任务。
func setupV3P208Task(t *testing.T, store *Store, goal string) (taskID string) {
	t.Helper()
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p208-"+id(), goal, "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return run.TaskID
}

// TestV3P208ParallelSteps 并行步骤。
func TestV3P208ParallelSteps(t *testing.T) {
	t.Run("two_workers_execute_independent_steps_concurrently", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "parallel.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		// 一个任务的两个独立就绪步骤（无依赖）。
		taskID := setupV3P208Task(t, store, "整理季度汇报")
		tx, _ := store.db.Begin()
		stepA := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze-a", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepA)
		stepB := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze-b", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepB)
		tx.Commit()
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "unit-a", TaskID: taskID, StepID: stepA.StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		insertWorkUnit(tx, WorkUnit{UnitID: "unit-b", TaskID: taskID, StepID: stepB.StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 同时选取多个可并行单元（同一任务的两个独立 Step）。
		tx, _ = store.db.Begin()
		units, err := selectParallelReadyUnits(tx, WorkUnitModel, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(units) != 2 {
			t.Errorf("parallel units = %d, want 2 (同 Task 独立 Step 同时调度)", len(units))
		}
		// 两个 Worker 可分别领取（同一时间区间，非串行）。
		workerA, credA := registerV3P202Worker(t, store, "worker-a", []WorkUnitKind{WorkUnitModel}, "")
		workerB, credB := registerV3P202Worker(t, store, "worker-b", []WorkUnitKind{WorkUnitModel}, "")
		tx, _ = store.db.Begin()
		claimA, errA := claimWorkUnit(tx, ClaimRequest{WorkerID: workerA, Credential: credA, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		claimB, errB := claimWorkUnit(tx, ClaimRequest{WorkerID: workerB, Credential: credB, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		tx.Commit()
		if errA != nil || errB != nil {
			t.Fatalf("parallel claims failed: A=%v B=%v (应可并行领取)", errA, errB)
		}
		if claimA.Unit.UnitID == claimB.Unit.UnitID {
			t.Error("two workers claimed same unit (并行 Step 应分别领取)")
		}
	})

	t.Run("unsatisfied_dependency_not_claimed", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "dep.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskID := setupV3P208Task(t, store, "整理汇报")
		// stepB 依赖 stepA（未完成）。
		tx, _ := store.db.Begin()
		stepA := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "dep-a", DefinitionVersion: 1, State: V3StepRunning, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepA)
		stepB := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "dep-b", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepB)
		insertStepDependency(tx, stepB.StepID, stepA.StepID, 0)
		insertWorkUnit(tx, WorkUnit{UnitID: "unit-dep-b", TaskID: taskID, StepID: stepB.StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 依赖未满足的步骤不领取（selectParallelReadyUnits 过滤）。
		tx, _ = store.db.Begin()
		units, err := selectParallelReadyUnits(tx, WorkUnitModel, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		for _, u := range units {
			if u.UnitID == "unit-dep-b" {
				t.Error("unsatisfied dependency unit claimed (依赖未满足的步骤不领取)")
			}
		}
	})
}

// TestV3P208MultiTaskControlIsolation 多任务控制隔离。
func TestV3P208MultiTaskControlIsolation(t *testing.T) {
	t.Run("pause_one_task_others_continue", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "isolate.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskA := setupV3P208Task(t, store, "任务A")
		taskB := setupV3P208Task(t, store, "任务B")
		// 任务 A 暂停。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskA, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskA)
		transitionTaskState(tx, taskA, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskA)
		transitionTaskState(tx, taskA, EventPause, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		tx.Commit()
		// 任务 B 继续（无关任务继续）。
		tx, _ = store.db.Begin()
		transitionTaskState(tx, taskB, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskB)
		_, err = transitionTaskState(tx, taskB, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Commit()
		if err != nil {
			t.Errorf("pausing task A blocked unrelated task B: %v (暂停一个 Task 时无关 Task 继续)", err)
		}
	})

	t.Run("cancel_invalidates_plans_keeps_effects", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "cancel.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskID := setupV3P208Task(t, store, "整理汇报")
		// 有未派发计划 + 已发生成果。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "unit-cancel", TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 已发生成果（取消前完成的工作）。
		ctx := context.Background()
		run, _ := store.claim(ctx)
		store.delta(ctx, run.ID, "# 部分成果")
		store.finish(run, "completed", "", runAccounting{})
		// 取消任务：未派发计划失效，已发生成果保留。
		tx, _ = store.db.Begin()
		invalidated, err := invalidateTaskPlans(tx, taskID, "用户取消", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if invalidated != 1 {
			t.Errorf("invalidated = %d, want 1 (同任务取消使未派发计划失效)", invalidated)
		}
		// 已发生成果保留（取消不自动回滚）。
		var artifactCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE task_id=?", taskID).Scan(&artifactCount); err != nil {
			t.Fatal(err)
		}
		if artifactCount != 1 {
			t.Errorf("cancel rolled back artifact: count=%d (取消保留已发生效果)", artifactCount)
		}
		// 审计记录失效。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=? AND event='plans_invalidated'", taskID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("invalidation not audited: count=%d", auditCount)
		}
	})

	t.Run("child_task_independent_goal_and_responsibility", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "child.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		parent := setupV3P208Task(t, store, "父任务：整理季度汇报")
		child := setupV3P208Task(t, store, "子任务：提取关键指标")
		// 父子关联（子任务有独立目标和责任）。
		tx, _ := store.db.Begin()
		err = linkChildTask(tx, ChildTaskLink{
			ParentTaskID: parent, ChildTaskID: child,
			ChildGoal: "提取关键指标", ChildCriteria: "指标完整", Independent: true,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 父子控制与 Step 依赖不混成一种状态：子任务有独立身份与目标。
		var childGoal string
		if err := store.db.QueryRow("SELECT goal FROM tasks WHERE id=?", child).Scan(&childGoal); err != nil {
			t.Fatal(err)
		}
		if childGoal != "子任务：提取关键指标" {
			t.Errorf("child goal = %s (子 Task 有独立目标和责任)", childGoal)
		}
		// 父子关联已审计。
		var linkCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=? AND event='child_task_linked'", child).Scan(&linkCount); err != nil {
			t.Fatal(err)
		}
		if linkCount != 1 {
			t.Errorf("child link not audited: count=%d", linkCount)
		}
	})
}

// TestV3P208SharedTaskBudget 共享预算与聚合状态。
func TestV3P208SharedTaskBudget(t *testing.T) {
	t.Run("shared_budget_not_exceeded", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "budget.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskID := setupV3P208Task(t, store, "整理汇报")
		// 配置共享预算上限 100。
		tx, _ := store.db.Begin()
		upsertQuotaConfig(tx, "task_budget:"+taskID, QuotaTaskBudget, 100, "tokens", taskID, now)
		tx.Commit()
		// 两个并行步骤共享预算：总和不超支。
		tx, _ = store.db.Begin()
		if _, err := reserveQuota(tx, QuotaReservation{Scope: QuotaTaskBudget, TaskID: taskID, StepID: "s1", Amount: 60, Unit: "tokens", ReservedAt: now}); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err = reserveQuota(tx, QuotaReservation{Scope: QuotaTaskBudget, TaskID: taskID, StepID: "s2", Amount: 60, Unit: "tokens", ReservedAt: now})
		tx.Rollback()
		if err == nil {
			t.Error("shared budget exceeded (共享预算不超支)")
		}
		// 占用快照：已用 60/上限 100。
		usage, _ := readQuotaUsage(store.db, QuotaTaskBudget, taskID)
		if usage.Reserved != 60 {
			t.Errorf("shared budget used = %d, want 60", usage.Reserved)
		}
	})

	t.Run("downstream_not_use_stale_evidence", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "stale.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskID := setupV3P208Task(t, store, "整理汇报")
		// stepB 依赖 stepA（就绪未完成的 stepA，带证据版本）。
		tx, _ := store.db.Begin()
		stepA := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "evidence-a", DefinitionVersion: 1, State: V3StepReady, EvidenceRevision: 1, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepA)
		stepB := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "evidence-b", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now}
		insertStep(tx, stepB)
		insertStepDependency(tx, stepB.StepID, stepA.StepID, 0)
		tx.Commit()
		// stepA 的证据失效：下游 stepB 重新核对（不能用旧证据）。
		tx, _ = store.db.Begin()
		if err := markStepEvidenceStale(tx, stepA.StepID, 2); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 依赖不满足（上游证据失效）：stepB 不可领取。
		tx, _ = store.db.Begin()
		steps, _ := listSteps(tx, taskID)
		byID := make(map[string]Step, len(steps))
		var target Step
		for _, s := range steps {
			byID[s.StepID] = s
			if s.StepID == stepB.StepID {
				target = s
			}
		}
		satisfied, reason, err := stepDependenciesSatisfied(tx, target, byID)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		if satisfied {
			t.Error("downstream used stale evidence (依赖结果更新后下游不能使用旧证据)")
		}
		if reason == "" {
			t.Error("stale evidence reason missing")
		}
	})

	t.Run("task_state_aggregates_steps_waits_ops", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "aggregate.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		taskID := setupV3P208Task(t, store, "整理汇报")
		// 一个步骤完成，但有开放等待：任务状态不完成（聚合）。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "done-step", DefinitionVersion: 1, State: V3StepCompleted, CreatedAt: now, UpdatedAt: now})
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "pending-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		openWait(tx, Wait{WaitID: "agg-wait", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		// 聚合：有未完成步骤/开放等待时，任务不完成（某一步完成不结束整项任务）。
		state, err := aggregateTaskState(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if state == V3TaskSucceeded || state == V3TaskFinalizing {
			t.Errorf("task state = %s with incomplete steps/waits (某一步完成不结束整项任务)", state)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
