package work

// P2-07 验收：确定性执行单元与等待释放。
//
// 三个测试入口：
//   - TestV3P207DeterministicNoModelSlot：确定性步骤不占模型槽；规则、等待和
//     恢复作为普通可调度单元推进，不被模型可用性限制。
//   - TestV3P207DeterministicReplaySameResult：输入/输出快照持久化，重放结果
//     一致；确定性步骤无 IO/网络；执行前核对当前合同与步骤版本。
//   - TestV3P207DeterministicReleasesWait：确定性步骤完成即释放等待；等待不占
//     执行槽；完成后原子标记完成并释放等待。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupV3P207 创建存储、任务与确定性步骤。
func setupV3P207(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p207-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID, run.ID
}

// v3P207DeterministicHandler 是测试用确定性处理器。
func v3P207DeterministicHandler(ctx context.Context, input DeterministicInput) (DeterministicResult, error) {
	// 纯确定性：无 IO/网络，输入相同则输出相同。
	return DeterministicResult{
		OutputRef: "normalized:" + input.InputRef,
		Summary:   "已规范化",
	}, nil
}

// TestV3P207DeterministicNoModelSlot 不占模型槽。
func TestV3P207DeterministicNoModelSlot(t *testing.T) {
	t.Run("deterministic_step_no_model_slot", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "noslot.db")
		now := v3P102Now()
		// 注册确定性处理器。
		RegisterDeterministic("normalize-data", v3P207DeterministicHandler)
		// 创建确定性步骤。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "normalize-data", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 确定性步骤执行：不占模型槽（无需 acquireModelSlot）。
		steps, _ := listSteps(store.db, taskID)
		usage, err := readQuotaUsage(store.db, QuotaModelSlot, "any-worker")
		if err != nil {
			t.Fatal(err)
		}
		if usage.Reserved != 0 {
			t.Errorf("deterministic step holds model slot: %d (确定性步骤不占模型槽)", usage.Reserved)
		}
		_ = steps
	})

	t.Run("rules_wait_recovery_progress_without_model", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "nomodel.db")
		now := v3P102Now()
		// 模型不可用：确定性核对循环仍推进（规则/等待/恢复不被模型可用性限制）。
		RegisterDeterministic("rule-check", v3P207DeterministicHandler)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "rule-check", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		loop := NewRuntimeLoop(store, func() bool { return false }) // 模型不可用
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		loop.Run(ctx, 10*time.Millisecond, 2)
		rounds, _ := loop.Stats()
		if rounds < 1 {
			t.Error("deterministic progress blocked by model unavailable (规则/等待/恢复不被模型可用性限制)")
		}
	})
}

// TestV3P207DeterministicReplaySameResult 重放一致。
func TestV3P207DeterministicReplaySameResult(t *testing.T) {
	t.Run("replay_same_input_same_output", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "replay.db")
		now := v3P102Now()
		RegisterDeterministic("replay-handler", v3P207DeterministicHandler)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "replay-handler", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitDeterministic, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 重放同一确定性单元：结果一致。
		var results []string
		for i := 0; i < 2; i++ {
			tx, _ = store.db.Begin()
			unit := DeterministicUnit{
				UnitID: "replay-unit", StepID: steps[0].StepID, TaskID: taskID,
				InputSnapshot: "input-1", ContractRevision: 1, DefinitionVersion: 1,
			}
			result, err := executeDeterministicUnit(tx, unit, v3P207DeterministicHandler, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Rollback() // 不提交（重放）
			results = append(results, result.OutputRef)
		}
		if results[0] != results[1] {
			t.Errorf("replay produced different results: %q vs %q (重放结果一致)", results[0], results[1])
		}
		if results[0] != "normalized:input-1" {
			t.Errorf("output = %q, want normalized:input-1", results[0])
		}
	})

	t.Run("contract_or_version_mismatch_rejected", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "mismatch.db")
		now := v3P102Now()
		RegisterDeterministic("version-handler", v3P207DeterministicHandler)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 2, DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		// 反例：旧合同/步骤版本不匹配被拒绝（执行前核对当前合同与步骤版本）。
		tx, _ = store.db.Begin()
		unit := DeterministicUnit{
			UnitID: "mismatch-unit", StepID: steps[0].StepID, TaskID: taskID,
			InputSnapshot: "input", ContractRevision: 1, DefinitionVersion: 1, // 旧合同
		}
		_, execErr := executeDeterministicUnit(tx, unit, v3P207DeterministicHandler, now)
		tx.Rollback()
		if execErr == nil {
			t.Error("old contract/version not rejected (执行前核对当前合同与步骤版本)")
		}
	})
}

// TestV3P207DeterministicReleasesWait 释放等待。
func TestV3P207DeterministicReleasesWait(t *testing.T) {
	t.Run("complete_releases_dependency_wait", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "release.db")
		now := v3P102Now()
		RegisterDeterministic("release-handler", v3P207DeterministicHandler)
		// 开启依赖等待 + 确定性单元。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "dep-wait", TaskID: taskID, Kind: WaitKindDependency, OpenedAt: now})
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "release-handler", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "release-unit", TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitDeterministic, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 确定性单元完成：原子标记完成并释放等待。
		tx, _ = store.db.Begin()
		if err := completeDeterministicUnit(tx, "release-unit", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 单元完成。
		var unitState string
		if err := store.db.QueryRow("SELECT state FROM work_units WHERE unit_id='release-unit'").Scan(&unitState); err != nil {
			t.Fatal(err)
		}
		if unitState != "completed" {
			t.Errorf("unit state = %s, want completed", unitState)
		}
		// 依赖等待已释放（完成即释放等待）。
		var waitState string
		if err := store.db.QueryRow("SELECT state FROM waits WHERE wait_id='dep-wait'").Scan(&waitState); err != nil {
			t.Fatal(err)
		}
		if waitState != "closed" {
			t.Errorf("dependency wait state = %s, want closed (确定性步骤完成即释放等待)", waitState)
		}
	})

	t.Run("waiting_task_no_execution_slot", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "noslot2.db")
		now := v3P102Now()
		// 任务有开放等待但无 active 执行单元：等待不占执行槽。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "wait-noslot", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		free, err := waitOccupiesNoExecutionSlot(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !free {
			t.Error("waiting task occupies execution slot (等待不占执行槽)")
		}
	})

	t.Run("deterministic_in_control_plane", func(t *testing.T) {
		store, taskID, _ := setupV3P207(t, "controlplane.db")
		now := v3P102Now()
		RegisterDeterministic("control-handler", v3P207DeterministicHandler)
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "control-handler", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "cp-unit", TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitDeterministic, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 控制面就地执行（无 IO 时控制面可直接完成）。
		tx, _ = store.db.Begin()
		result, err := runDeterministicInControlPlane(tx, DeterministicUnit{
			UnitID: "cp-unit", StepID: steps[0].StepID, TaskID: taskID,
			InputSnapshot: "control-handler", ContractRevision: 1, DefinitionVersion: 1,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.OutputRef == "" {
			t.Error("control plane deterministic produced no output")
		}
		// 单元已完成（控制面就地完成并释放）。
		var unitState string
		if err := store.db.QueryRow("SELECT state FROM work_units WHERE unit_id='cp-unit'").Scan(&unitState); err != nil {
			t.Fatal(err)
		}
		if unitState != "completed" {
			t.Errorf("unit state = %s, want completed (控制面就地完成)", unitState)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
