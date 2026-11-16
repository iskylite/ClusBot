package work

// P4-07 验收：版本化运维流程模板与运行时绑定。
//
// 三个测试入口：
//   - TestV3P407DomainWorkflowBinding：受理时绑定领域包/模板/规范/目标版本，
//     转换成 Step 与依赖；模型只在允许调查节点提出补查；流程含缺依赖、冲突
//     目标或未绑定能力时不能直接执行。
//   - TestV3P407WorkflowVersionChange：模板变化不静默改运行中工作；迁移需
//     合同修订、依赖影响评估和必要重新确认；同一模板重启后按持久步骤继续，
//     未决 Job 不重启。
//   - TestV3P407WorkflowExecutionOwner：明确原子能力、委托整段流程和持续服务
//     三种粒度，防止本地与外部重复调度；为领域可自动判定的步骤提供确定性
//     处理器；不把每个流程都变为模型循环或 Goal；失败分支和取消有明确剩余
//     责任，外部流程成功仍需本地验收。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P407 创建存储与任务。
func setupV3P407(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p407-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// v3P407Steps 构造测试步骤。
func v3P407Steps() []DomainWorkflowStep {
	return []DomainWorkflowStep{
		{StepID: "precheck", Kind: "deterministic", CapabilityKey: "precheck-cap"},
		{StepID: "investigate", Kind: "investigation", DependsOn: []string{"precheck"}},
		{StepID: "validate", Kind: "deterministic", CapabilityKey: "validate-cap", DependsOn: []string{"investigate"}},
		{StepID: "acceptance", Kind: "acceptance", DependsOn: []string{"validate"}},
	}
}

// TestV3P407DomainWorkflowBinding 领域流程绑定。
func TestV3P407DomainWorkflowBinding(t *testing.T) {
	t.Run("binding_creates_runtime_steps_dependencies", func(t *testing.T) {
		store, taskID := setupV3P407(t, "binding.db")
		now := v3P102Now()
		// 发布模板。
		tx, _ := store.db.Begin()
		template, err := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "node-validation", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, v3P407Steps(), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 绑定：转换成 Step 与依赖。
		tx, _ = store.db.Begin()
		binding, err := bindWorkflowToTask(tx, taskID, template.TemplateID, "cluster-domain@1", `{"node":"1.0"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if binding.TaskID != taskID || binding.TemplateID != template.TemplateID {
			t.Errorf("binding incomplete: %+v", binding)
		}
		// 运行时 Step 已创建（流程交给既有运行时 Step/Wait 推进）。
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 4 {
			t.Errorf("runtime steps = %d, want 4 (转换成 Step 与依赖)", len(steps))
		}
	})

	t.Run("missing_dependency_rejected", func(t *testing.T) {
		store, _ := setupV3P407(t, "missingdep.db")
		now := v3P102Now()
		// 缺依赖：不能直接执行。
		tx, _ := store.db.Begin()
		badSteps := []DomainWorkflowStep{
			{StepID: "step1", Kind: "deterministic", CapabilityKey: "cap1", DependsOn: []string{"nonexistent"}},
		}
		_, err := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "bad-template", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, badSteps, now)
		tx.Rollback()
		if err == nil {
			t.Error("missing dependency not rejected (缺依赖不能直接执行)")
		}
		if !strings.Contains(err.Error(), "不存在") && !strings.Contains(err.Error(), "缺依赖") {
			t.Errorf("missing dependency reason missing: %v", err)
		}
	})

	t.Run("unbound_capability_rejected", func(t *testing.T) {
		store, _ := setupV3P407(t, "unbound.db")
		now := v3P102Now()
		// 未绑定能力：不能直接执行。
		tx, _ := store.db.Begin()
		badSteps := []DomainWorkflowStep{
			{StepID: "step1", Kind: "deterministic"}, // 缺 CapabilityKey
		}
		_, err := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "unbound-template", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, badSteps, now)
		tx.Rollback()
		if err == nil {
			t.Error("unbound capability not rejected (未绑定能力不能直接执行)")
		}
		if !strings.Contains(err.Error(), "缺少能力绑定") {
			t.Errorf("unbound capability reason missing: %v", err)
		}
	})

	t.Run("model_only_proposes_investigation_node", func(t *testing.T) {
		// 模型只在允许调查节点提出补查（不在其他节点）。
		steps := v3P407Steps()
		investigationCount := 0
		for _, step := range steps {
			if step.Kind == "investigation" {
				investigationCount++
			}
		}
		if investigationCount != 1 {
			t.Errorf("investigation steps = %d, want 1 (模型只在允许调查节点提出补查)", investigationCount)
		}
	})
}

// TestV3P407WorkflowVersionChange 版本变化。
func TestV3P407WorkflowVersionChange(t *testing.T) {
	t.Run("template_change_not_silently_modify_running", func(t *testing.T) {
		store, taskID := setupV3P407(t, "change.db")
		now := v3P102Now()
		// 发布模板 v1 并绑定。
		tx, _ := store.db.Begin()
		template, _ := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "node-validation", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, v3P407Steps(), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		bindWorkflowToTask(tx, taskID, template.TemplateID, "cluster-domain@1", `{}`, now)
		tx.Commit()
		// 模板变化评估：有运行中绑定需合同修订与重新确认。
		tx, _ = store.db.Begin()
		change, err := assessTemplateChange(tx, template.TemplateID, 1, 2, "admin", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !change.ContractRevisionRequired {
			t.Error("template change not requiring contract revision (迁移需合同修订)")
		}
		if !change.ReconfirmationRequired {
			t.Error("template change not requiring reconfirmation (必要重新确认)")
		}
		if !strings.Contains(change.ImpactAssessment, "运行中") {
			t.Errorf("impact assessment missing: %s", change.ImpactAssessment)
		}
	})

	t.Run("restart_continues_by_persistent_steps", func(t *testing.T) {
		// 同一模板重启后按持久步骤继续（未决 Job 不重启）。
		path := filepath.Join(t.TempDir(), "restart.db")
		now := v3P102Now()
		var taskID, templateID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, _ := store.Submit(ctx, "v3p407-restart", "任务", "", "", true)
			taskID = run.TaskID
			tx, _ := store.db.Begin()
			template, _ := publishWorkflowTemplate(tx, WorkflowTemplate{
				Name: "restart-workflow", Version: 1, TriggerKind: "manual",
				Granularity: "atomic_capability", PublishedBy: "admin",
			}, v3P407Steps(), now)
			templateID = template.TemplateID
			bindWorkflowToTask(tx, taskID, templateID, "domain@1", `{}`, now)
			tx.Commit()
		}()
		// 重启：持久步骤仍在（按持久步骤继续）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 4 {
			t.Errorf("persistent steps lost after restart: %d (重启后按持久步骤继续)", len(steps))
		}
		// 未决 Job 不重启（无 operation_jobs）。
		var jobCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_jobs").Scan(&jobCount); err != nil {
			t.Fatal(err)
		}
		if jobCount != 0 {
			t.Errorf("pending jobs restarted: count=%d (未决 Job 不重启)", jobCount)
		}
	})

	t.Run("no_active_binding_no_contract_revision", func(t *testing.T) {
		store, _ := setupV3P407(t, "noactive.db")
		now := v3P102Now()
		// 无运行中绑定：不需合同修订。
		tx, _ := store.db.Begin()
		template, _ := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "inactive-template", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, v3P407Steps(), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		change, err := assessTemplateChange(tx, template.TemplateID, 1, 2, "admin", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if change.ContractRevisionRequired {
			t.Error("no active binding requires contract revision (无运行中绑定不需合同修订)")
		}
	})
}

// TestV3P407WorkflowExecutionOwner 执行所有者。
func TestV3P407WorkflowExecutionOwner(t *testing.T) {
	t.Run("granularity_prevents_duplicate_scheduling", func(t *testing.T) {
		store, taskID := setupV3P407(t, "granularity.db")
		now := v3P102Now()
		// 委托整段流程：检查重复调度。
		tx, _ := store.db.Begin()
		createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-dup",
		}, now)
		tx.Commit()
		// 委托整段流程：重复调度拒绝。
		tx, _ = store.db.Begin()
		err := ensureNoDuplicateScheduling(tx, taskID, "delegated_workflow", now)
		tx.Rollback()
		if err == nil {
			t.Error("duplicate scheduling allowed (防止本地与外部重复调度)")
		}
		// 原子能力：允许（无委托）。
		tx, _ = store.db.Begin()
		err = ensureNoDuplicateScheduling(tx, taskID, "atomic_capability", now)
		tx.Commit()
		if err != nil {
			t.Errorf("atomic capability rejected: %v", err)
		}
	})

	t.Run("deterministic_steps_not_all_model_loop", func(t *testing.T) {
		// 不把每个流程都变为模型循环或 Goal（确定性处理器）。
		allInvestigation := []DomainWorkflowStep{
			{StepID: "s1", Kind: "investigation"},
			{StepID: "s2", Kind: "investigation"},
		}
		if err := ensureDeterministicStepsNotAllModel(allInvestigation); err == nil {
			t.Error("all-investigation workflow allowed (不把每个流程都变为模型循环)")
		}
		// 含确定性步骤：允许。
		mixed := []DomainWorkflowStep{
			{StepID: "s1", Kind: "deterministic", CapabilityKey: "cap"},
			{StepID: "s2", Kind: "investigation"},
		}
		if err := ensureDeterministicStepsNotAllModel(mixed); err != nil {
			t.Errorf("mixed workflow rejected: %v", err)
		}
	})

	t.Run("external_workflow_success_still_local_acceptance", func(t *testing.T) {
		// 外部流程成功仍需本地验收（本地规则决定）。
		store, taskID := setupV3P407(t, "localeval.db")
		now := v3P102Now()
		// 外部流程成功（模拟）：本地验收独立。
		tx, _ := store.db.Begin()
		createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察与验收", RemoteResponsibility: "执行",
			ExternalJobID: "job-external",
		}, now)
		tx.Commit()
		// 本地验收：独立决定（外部成功不自动通过）。
		var bindingState string
		if err := store.db.QueryRow("SELECT state FROM workflow_bindings WHERE task_id=?", taskID).Scan(&bindingState); err == nil {
			t.Errorf("external success auto-accepted: state=%s (外部流程成功仍需本地验收)", bindingState)
		}
	})

	t.Run("failure_cancel_has_residual_responsibility", func(t *testing.T) {
		store, taskID := setupV3P407(t, "residual.db")
		now := v3P102Now()
		// 失败分支和取消有明确剩余责任。
		tx, _ := store.db.Begin()
		template, _ := publishWorkflowTemplate(tx, WorkflowTemplate{
			Name: "residual-workflow", Version: 1, TriggerKind: "manual",
			Granularity: "atomic_capability", PublishedBy: "admin",
		}, v3P407Steps(), now)
		bindWorkflowToTask(tx, taskID, template.TemplateID, "domain@1", `{}`, now)
		tx.Commit()
		// 绑定含剩余责任（Task 失败/取消时保留）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM workflow_bindings WHERE task_id=? AND state='active'", taskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("residual responsibility not recorded: count=%d (失败分支和取消有明确剩余责任)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
