package work

// 验收：Step 依赖与确定性流程基础。
//
// 三个测试入口：
//   - TestStepDependencyGraph：模型/确定性/能力步骤的类型与输入/结果
//     引用；复用 DAG 无环与缺失依赖校验；仅当必需依赖完成且证据版本有效时
//     生成就绪单元；多个独立步骤可就绪。
//   - TestDependencyEvidenceChanged：上游证据变化使依赖重新核对，不
//     删除旧结果；不可满足依赖产生明确等待/失败原因；可选步骤取消遵守固定
//     完成规则。
//   - TestDeterministicStepWithoutModel：确定性步骤无需调用模型；只写
//     计划 JSON 而没有实际推进不能验收；未知处理器类型明确拒绝。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// setupStepsStore 创建基础 Task 与存储。
func setupStepsStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p104-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// insertStepFixture 在事务内插入一个 Step。
func insertStepFixture(t *testing.T, store *Store, s Step) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertStep(tx, s); err != nil {
		t.Fatalf("insert step %s: %v", s.DefinitionKey, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestStepDependencyGraph 依赖图与就绪判定。
func TestStepDependencyGraph(t *testing.T) {
	t.Run("step_kinds_have_input_result_refs", func(t *testing.T) {
		// 三种步骤类型：输入/结果引用独立保存，绑定合同与定义版本。
		defs := []StepDefinition{
			{Key: "analyze", Kind: StepKindModel, Version: 1, InputRef: "material://input/1"},
			{Key: "normalize", Kind: StepKindDeterministic, Version: 2, InputRef: "material://input/2", DeterministicHandler: "normalize"},
			{Key: "deploy", Kind: StepKindCapability, Version: 1, InputRef: "material://input/3", CapabilityRef: "capability://deploy"},
		}
		for _, def := range defs {
			if err := def.validate(); err != nil {
				t.Errorf("definition %s invalid: %v", def.Key, err)
			}
			if def.InputRef == "" {
				t.Errorf("definition %s missing input ref", def.Key)
			}
		}
		// 反例：能力步骤缺能力引用、确定性步骤缺处理器被拒绝。
		if err := (StepDefinition{Key: "bad-cap", Kind: StepKindCapability, Version: 1}).validate(); err == nil {
			t.Error("capability step without ref not rejected")
		}
		if err := (StepDefinition{Key: "bad-det", Kind: StepKindDeterministic, Version: 1}).validate(); err == nil {
			t.Error("deterministic step without handler not rejected")
		}
	})

	t.Run("dag_validation_rejects_cycles_self_and_unknown", func(t *testing.T) {
		// 环：a→b→c→a。
		cyclic := StepPlan{
			TaskID: "t1", ContractRevision: 1,
			Steps: []StepDefinition{
				{Key: "a", Kind: StepKindModel, Version: 1},
				{Key: "b", Kind: StepKindModel, Version: 1},
				{Key: "c", Kind: StepKindModel, Version: 1},
			},
			Dependencies: map[string][]string{"a": {"c"}, "b": {"a"}, "c": {"b"}},
		}
		if err := cyclic.validate(); err == nil {
			t.Error("cyclic dependency not rejected")
		} else if !strings.Contains(err.Error(), "环") {
			t.Errorf("cycle error unclear: %v", err)
		}
		// 自依赖。
		self := StepPlan{
			TaskID: "t1", ContractRevision: 1,
			Steps:        []StepDefinition{{Key: "a", Kind: StepKindModel, Version: 1}},
			Dependencies: map[string][]string{"a": {"a"}},
		}
		if err := self.validate(); err == nil {
			t.Error("self dependency not rejected")
		}
		// 未知依赖。
		unknown := StepPlan{
			TaskID: "t1", ContractRevision: 1,
			Steps:        []StepDefinition{{Key: "a", Kind: StepKindModel, Version: 1}},
			Dependencies: map[string][]string{"a": {"ghost"}},
		}
		if err := unknown.validate(); err == nil {
			t.Error("unknown dependency not rejected")
		}
		// 反例：同键不同计划（重复 key）被拒绝。
		dup := StepPlan{
			TaskID: "t1", ContractRevision: 1,
			Steps: []StepDefinition{
				{Key: "a", Kind: StepKindModel, Version: 1},
				{Key: "a", Kind: StepKindModel, Version: 2},
			},
		}
		if err := dup.validate(); err == nil {
			t.Error("duplicate step key not rejected")
		}
	})

	t.Run("ready_steps_require_satisfied_dependencies", func(t *testing.T) {
		store, taskID := setupStepsStore(t, "ready.db")
		now := recordsTestNow()
		// a（完成）→ b（就绪，依赖 a）；c（独立，就绪）。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "a", DefinitionVersion: 1, State: RuntimeStepCompleted, EvidenceRevision: 1,
			Position: 1, CreatedAt: now, UpdatedAt: now})
		stepA, _ := listSteps(store.db, taskID)
		aID := stepA[0].StepID
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "b", DefinitionVersion: 1, State: RuntimeStepBlocked, EvidenceRevision: 1,
			Position: 2, CreatedAt: now, UpdatedAt: now})
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "c", DefinitionVersion: 1, State: RuntimeStepReady, EvidenceRevision: 0,
			Position: 3, CreatedAt: now, UpdatedAt: now})
		steps, _ := listSteps(store.db, taskID)
		var bID string
		for _, s := range steps {
			if s.DefinitionKey == "b" {
				bID = s.StepID
			}
		}
		// 建立依赖 b→a。
		tx, _ := store.db.Begin()
		if err := insertStepDependency(tx, bID, aID, 1); err != nil {
			t.Fatal(err)
		}
		tx.Commit()

		// 计算就绪：b（依赖已完成）与 c（独立）都应就绪。
		ready, err := computeReadySteps(store.db, taskID, 1)
		if err != nil {
			t.Fatal(err)
		}
		readyKeys := map[string]bool{}
		for _, r := range ready {
			readyKeys[r.Step.DefinitionKey] = true
		}
		if !readyKeys["b"] {
			t.Error("step b not ready despite satisfied dependency")
		}
		if !readyKeys["c"] {
			t.Error("independent step c not ready")
		}
		// 多个独立步骤可就绪（不只一个）。
		if len(ready) < 2 {
			t.Errorf("only %d steps ready, want >= 2 (独立步骤并行就绪)", len(ready))
		}
		// 就绪单元已生成。
		for _, r := range ready {
			if r.Unit.UnitID == "" || r.Unit.StepID != r.Step.StepID {
				t.Errorf("ready step %s missing work unit", r.Step.DefinitionKey)
			}
		}
	})

	t.Run("contract_revision_isolation_blocks_old_steps", func(t *testing.T) {
		store, taskID := setupStepsStore(t, "contract.db")
		now := recordsTestNow()
		// 合同 v1 的就绪步骤。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "old", DefinitionVersion: 1, State: RuntimeStepReady, Position: 1, CreatedAt: now, UpdatedAt: now})
		// 以合同 v2 计算就绪：v1 步骤不就绪（旧计划对新合同失效）。
		ready, err := computeReadySteps(store.db, taskID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) != 0 {
			t.Errorf("old contract steps ready under new contract: %d", len(ready))
		}
	})
}

// TestDependencyEvidenceChanged 证据失效与不可满足依赖。
func TestDependencyEvidenceChanged(t *testing.T) {
	t.Run("evidence_change_invalidates_dependents", func(t *testing.T) {
		store, taskID := setupStepsStore(t, "evidence.db")
		now := recordsTestNow()
		// a（完成，evidence=1）→ b（依赖 a，evidence=1）。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "a", DefinitionVersion: 1, State: RuntimeStepCompleted, EvidenceRevision: 1,
			Position: 1, CreatedAt: now, UpdatedAt: now})
		steps, _ := listSteps(store.db, taskID)
		aID := steps[0].StepID
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "b", DefinitionVersion: 1, State: RuntimeStepReady, EvidenceRevision: 1,
			Position: 2, CreatedAt: now, UpdatedAt: now})
		steps, _ = listSteps(store.db, taskID)
		var bID string
		for _, s := range steps {
			if s.DefinitionKey == "b" {
				bID = s.StepID
			}
		}
		tx, _ := store.db.Begin()
		insertStepDependency(tx, bID, aID, 1)
		tx.Commit()

		// 上游 a 的证据变化（evidence 1→2）：依赖它的 b 须重新核对。
		stale, err := InvalidateDependents(store.db, aID)
		if err != nil {
			t.Fatal(err)
		}
		if len(stale) != 1 || stale[0] != bID {
			t.Errorf("invalidate dependents = %v, want [%s]", stale, bID)
		}
		// 标记 b 为 blocked（不删除旧结果）。
		tx, _ = store.db.Begin()
		if err := markStepEvidenceStale(tx, bID, 2); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// b 现在是 blocked：不就绪。
		ready, err := computeReadySteps(store.db, taskID, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ready {
			if r.Step.StepID == bID {
				t.Error("stale-evidence step still ready")
			}
		}
		// 旧结果（a 的完成状态）保留，未被删除。
		var aState string
		if err := store.db.QueryRow("SELECT state FROM steps WHERE step_id=?", aID).Scan(&aState); err != nil {
			t.Fatal(err)
		}
		if aState != string(RuntimeStepCompleted) {
			t.Errorf("upstream result deleted: state=%s", aState)
		}
	})

	t.Run("unsatisfiable_dependencies_produce_reasons", func(t *testing.T) {
		store, taskID := setupStepsStore(t, "unsat.db")
		now := recordsTestNow()
		// a（失败）→ b（依赖 a）：不可满足，产生明确失败原因。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "a", DefinitionVersion: 1, State: RuntimeStepFailed, Position: 1, CreatedAt: now, UpdatedAt: now})
		steps, _ := listSteps(store.db, taskID)
		aID := steps[0].StepID
		bStep := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "b", DefinitionVersion: 1, State: RuntimeStepBlocked, Position: 2, CreatedAt: now, UpdatedAt: now}
		insertStepFixture(t, store, bStep)
		tx, _ := store.db.Begin()
		insertStepDependency(tx, bStep.StepID, aID, 1)
		tx.Commit()

		// 直接核对依赖满足性：失败依赖产生明确原因。
		steps, _ = listSteps(store.db, taskID)
		byID := map[string]Step{}
		var b Step
		for _, s := range steps {
			byID[s.StepID] = s
			if s.DefinitionKey == "b" {
				b = s
			}
		}
		ok, reason, err := stepDependenciesSatisfied(store.db, b, byID)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("failed dependency satisfied")
		}
		if reason == "" || !strings.Contains(reason, "失败") && !strings.Contains(reason, "failed") {
			t.Errorf("unsatisfiable reason missing: %q", reason)
		}
		t.Logf("unsatisfiable dependency reason: %s", reason)
	})

	t.Run("optional_step_cancel_does_not_block", func(t *testing.T) {
		store, taskID := setupStepsStore(t, "optional.db")
		now := recordsTestNow()
		// a（可选，已取消）→ b（依赖 a）：可选步骤取消不阻塞。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "a", DefinitionVersion: 1, State: RuntimeStepCancelled, Optional: true,
			Position: 1, CreatedAt: now, UpdatedAt: now})
		steps, _ := listSteps(store.db, taskID)
		aID := steps[0].StepID
		bStep := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "b", DefinitionVersion: 1, State: RuntimeStepBlocked, EvidenceRevision: 0,
			Position: 2, CreatedAt: now, UpdatedAt: now}
		insertStepFixture(t, store, bStep)
		tx, _ := store.db.Begin()
		insertStepDependency(tx, bStep.StepID, aID, 0)
		tx.Commit()

		steps, _ = listSteps(store.db, taskID)
		byID := map[string]Step{}
		var b Step
		for _, s := range steps {
			byID[s.StepID] = s
			if s.DefinitionKey == "b" {
				b = s
			}
		}
		ok, reason, err := stepDependenciesSatisfied(store.db, b, byID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("optional cancelled dependency blocked: %s", reason)
		}
	})
}

// TestDeterministicStepWithoutModel 确定性步骤无需模型。
func TestDeterministicStepWithoutModel(t *testing.T) {
	t.Run("deterministic_step_executes_without_model", func(t *testing.T) {
		// 注册一个确定性处理器：不调用任何模型。
		RegisterDeterministic("normalize", func(ctx context.Context, input DeterministicInput) (DeterministicResult, error) {
			return DeterministicResult{
				OutputRef: "material://output/normalized",
				Summary:   "已规范化",
			}, nil
		})
		step := Step{StepID: "s1", TaskID: "t1", ContractRevision: 1, State: RuntimeStepReady, InputRef: "material://input/1"}
		def := StepDefinition{Key: "normalize", Kind: StepKindDeterministic, Version: 2, DeterministicHandler: "normalize"}
		result, err := executeDeterministicStep(context.Background(), step, def)
		if err != nil {
			t.Fatalf("deterministic execution: %v", err)
		}
		if result.OutputRef == "" || result.Summary == "" {
			t.Errorf("deterministic result incomplete: %+v", result)
		}
	})

	t.Run("unknown_handler_rejected", func(t *testing.T) {
		step := Step{StepID: "s1", TaskID: "t1", ContractRevision: 1, State: RuntimeStepReady}
		def := StepDefinition{Key: "ghost", Kind: StepKindDeterministic, Version: 1, DeterministicHandler: "ghost"}
		_, err := executeDeterministicStep(context.Background(), step, def)
		if err == nil {
			t.Error("unknown deterministic handler not rejected")
		}
	})

	t.Run("non_ready_step_rejected", func(t *testing.T) {
		RegisterDeterministic("normalize2", func(ctx context.Context, input DeterministicInput) (DeterministicResult, error) {
			return DeterministicResult{OutputRef: "material://out"}, nil
		})
		step := Step{StepID: "s1", TaskID: "t1", ContractRevision: 1, State: RuntimeStepCompleted}
		def := StepDefinition{Key: "normalize2", Kind: StepKindDeterministic, Version: 1, DeterministicHandler: "normalize2"}
		if _, err := executeDeterministicStep(context.Background(), step, def); err == nil {
			t.Error("non-ready step executed")
		}
	})

	t.Run("plan_json_without_progress_not_acceptance", func(t *testing.T) {
		// 反例：依赖未满足的 blocked 步骤不就绪、不产生就绪单元——"只写计划
		// 而没有实际推进"不能验收。
		store, taskID := setupStepsStore(t, "planonly.db")
		now := recordsTestNow()
		// 上游未完成（running）→ 下游 blocked 不就绪。
		insertStepFixture(t, store, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "upstream", DefinitionVersion: 1, State: RuntimeStepRunning, Position: 1, CreatedAt: now, UpdatedAt: now})
		steps, _ := listSteps(store.db, taskID)
		upID := steps[0].StepID
		down := Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "downstream", DefinitionVersion: 1, State: RuntimeStepBlocked, Position: 2, CreatedAt: now, UpdatedAt: now}
		insertStepFixture(t, store, down)
		tx, _ := store.db.Begin()
		insertStepDependency(tx, down.StepID, upID, 1)
		tx.Commit()
		// 依赖未满足 → 不就绪 → 不产生就绪单元。
		ready, err := computeReadySteps(store.db, taskID, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ready {
			if r.Step.StepID == down.StepID {
				t.Errorf("step with unsatisfied dependency became ready (plan without progress)")
			}
		}
	})

	t.Run("workflow_steps_classified", func(t *testing.T) {
		// 有独立目标/验收的保留子 Task；普通步骤使用 Step。
		plan := WorkflowRequest{
			Goal: "发布报告", Scope: "工作", Criteria: "完成",
			Steps: []WorkflowStep{
				{Key: "collect", Goal: "收集", Criteria: "完整"},
				{Key: "draft", Goal: "起草", Criteria: "通过"},
			},
		}
		classified := classifyWorkflowSteps(plan)
		for key, ownership := range classified {
			if ownership != OwnershipChildTask {
				t.Errorf("workflow step %s = %v, want child task (有独立目标/验收)", key, ownership)
			}
		}
		// 无独立验收的应为 Step。
		if got := ClassifyWorkflowStep(true, false); got != OwnershipStep {
			t.Errorf("step without independent criteria = %v, want OwnershipStep", got)
		}
	})
}

// 确保 sql/errors 引用保留。
var (
	_ = sql.ErrNoRows
	_ = errors.Is
)
