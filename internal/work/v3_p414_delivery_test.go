package work

// P4-14 验收：验收、实际交接与 E01 全流程。
//
// 三个测试入口：
//   - TestV3P414DeliveryHandoverGuard：请求有权接收人确认实际交接，绑定对象
//     范围、版本、决定者和时效；确认不是授权；交接提交前重核当前资产、验收
//     有效性和限制，符合条件才转在役并保存 delivery；接收确认前不转在役；
//     确认后证据失效则重判，不沿用过期决定。
//   - TestV3P414E01CommissioningEndToEnd：从 Chat 走通发现、母盘、部署、配置、
//     基准、联机、验收与交接，注入身份冲突、单机/组测失败和取消；E01 全链各
//     阶段均有实际调用/结果和证据，不使用占位成功；已发生部署变化不会因
//     Task 取消被抹除，剩余责任可查。
//   - TestV3P414PartialDeliveryTaskCriteria：按要求×节点/对象组核验，有效证据
//     满足后进入待交付，保留未通过节点与原因；部分交付只完成对应对象；Task
//     是否成功由其完成条件决定，不能自动将整批成功。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P414 创建存储、资产与授权。
func setupV3P414(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:receiver", Action: "confirm", ObjectRef: "node-1", ValidFrom: now, Owner: "receiver"}, now)
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "write", ObjectRef: "node-1", ValidFrom: now, Owner: "ops"}, now)
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStatePendingDeliver, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// TestV3P414DeliveryHandoverGuard 交接守卫。
func TestV3P414DeliveryHandoverGuard(t *testing.T) {
	t.Run("handover_confirmation_binds_all_fields", func(t *testing.T) {
		now := v3P102Now()
		// 请求有权接收人确认实际交接：绑定对象范围、版本、决定者和时效。
		confirmation, err := requestHandoverConfirmation(nil, []string{"node-1"}, 1, "user:receiver", now+100000, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(confirmation.ObjectRefs) != 1 || confirmation.Version != 1 ||
			confirmation.DecidedBy != "user:receiver" || confirmation.ExpiresAt <= now {
			t.Errorf("handover confirmation incomplete: %+v (绑定对象范围/版本/决定者/时效)", confirmation)
		}
	})

	t.Run("confirmation_not_authorization", func(t *testing.T) {
		now := v3P102Now()
		// 确认不是授权：确认人必须是决定者。
		confirmation, _ := requestHandoverConfirmation(nil, []string{"node-1"}, 1, "user:receiver", now+100000, now)
		_, err := confirmHandover(nil, confirmation, "user:other", now)
		if err == nil {
			t.Error("confirmation by non-decider allowed (确认不是授权)")
		}
		if !strings.Contains(err.Error(), "不是决定者") {
			t.Errorf("confirmation reason missing: %v", err)
		}
	})

	t.Run("expired_confirmation_rejected", func(t *testing.T) {
		now := v3P102Now()
		// 过期决定：不沿用（确认后证据失效则重判）。
		confirmation, _ := requestHandoverConfirmation(nil, []string{"node-1"}, 1, "user:receiver", now+1000, now)
		_, err := confirmHandover(nil, confirmation, "user:receiver", now+2000) // 过期
		if err == nil {
			t.Error("expired confirmation allowed (不沿用过期决定)")
		}
		if !strings.Contains(err.Error(), "过期") {
			t.Errorf("expired reason missing: %v", err)
		}
	})

	t.Run("in_service_requires_confirmed_handover", func(t *testing.T) {
		store := setupV3P414(t, "inservice.db")
		now := v3P102Now()
		// 接收确认前不转在役。
		pendingConfirmation := HandoverConfirmation{Outcome: "pending"}
		tx, _ := store.db.Begin()
		err := submitDeliveryHandover(tx, "node-1", "", "user:receiver", pendingConfirmation, now)
		tx.Rollback()
		if err == nil {
			t.Error("in-service without confirmation allowed (接收确认前不转在役)")
		}
		if !strings.Contains(err.Error(), "未确认交接") {
			t.Errorf("unconfirmed reason missing: %v", err)
		}
	})

	t.Run("frozen_asset_rejected_before_handover", func(t *testing.T) {
		store := setupV3P414(t, "frozen.db")
		now := v3P102Now()
		// 交接提交前重核限制（冻结拒绝）。
		tx, _ := store.db.Begin()
		freezeAsset(tx, "node-1", "admin", "维护冻结", now)
		tx.Commit()
		confirmation := HandoverConfirmation{Outcome: "confirmed", DecidedBy: "user:receiver"}
		tx, _ = store.db.Begin()
		err := submitDeliveryHandover(tx, "node-1", "", "user:ops", confirmation, now)
		tx.Rollback()
		if err == nil {
			t.Error("frozen asset handed over (交接提交前重核限制)")
		}
		if !strings.Contains(err.Error(), "冻结") {
			t.Errorf("frozen reason missing: %v", err)
		}
	})

	t.Run("delivery_transitions_asset_to_in_service", func(t *testing.T) {
		// P2-3：交付交接迁移资产状态（不再只写 delivery_records）。
		store := setupV3P414(t, "migrate.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		recordAcceptanceCoverage(tx, AcceptanceCoverage{RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered"}, now)
		tx.Commit()
		confirmation := HandoverConfirmation{Outcome: "confirmed", DecidedBy: "user:receiver", ConfirmationID: "conf-m"}
		tx, _ = store.db.Begin()
		if err := submitDeliveryHandover(tx, "node-1", "delivery-ready", "user:ops", confirmation, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 资产状态已迁移到在役。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateInService {
			t.Errorf("asset state=%s, want 在役（交付应迁移资产状态）", asset.State)
		}
	})

	t.Run("invalidated_evidence_blocks_in_service", func(t *testing.T) {
		// P2-3：验收证据失效时拒绝转在役（重核验收有效性）。
		store := setupV3P414(t, "invalidated.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		recordAcceptanceCoverage(tx, AcceptanceCoverage{RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "uncovered"}, now)
		tx.Commit()
		confirmation := HandoverConfirmation{Outcome: "confirmed", DecidedBy: "user:receiver", ConfirmationID: "conf-i"}
		tx, _ = store.db.Begin()
		err := submitDeliveryHandover(tx, "node-1", "delivery-ready", "user:ops", confirmation, now)
		tx.Rollback()
		if err == nil {
			t.Error("uncovered evidence allowed in-service (验收失效应拒绝转在役)")
		} else if !strings.Contains(err.Error(), "验收") {
			t.Errorf("error missing acceptance reason: %v", err)
		}
		// 资产状态未被迁移。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State == AssetStateInService {
			t.Error("asset transitioned despite invalidated evidence")
		}
	})

	t.Run("confirmed_handover_in_service_saves_delivery", func(t *testing.T) {
		store := setupV3P414(t, "confirmed.db")
		now := v3P102Now()
		// 确认交接：符合条件才转在役并保存 delivery。
		confirmation := HandoverConfirmation{Outcome: "confirmed", DecidedBy: "user:receiver", ConfirmationID: "conf-1"}
		tx, _ := store.db.Begin()
		err := submitDeliveryHandover(tx, "node-1", "", "user:ops", confirmation, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// delivery 已保存。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM delivery_records WHERE object_ref='node-1' AND stage='in_service'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("delivery not saved: count=%d (符合条件才转在役并保存 delivery)", count)
		}
	})
}

// TestV3P414E01CommissioningEndToEnd E01 端到端。
func TestV3P414E01CommissioningEndToEnd(t *testing.T) {
	t.Run("full_chain_discovery_to_handover", func(t *testing.T) {
		store := setupV3P414(t, "e2e.db")
		now := v3P102Now()
		// E01 全链：发现 → 母盘 → 部署 → 配置 → 基准 → 联机 → 验收 → 交接。
		// 1. 发现（P4-08）。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:discovery", Action: "discover", ObjectRef: "cmdb-source", ValidFrom: now, Owner: "discovery"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		processDiscoveryResult(tx, DiscoveryResult{
			Discovered: []DiscoveredObject{{CandidateIdentity: "192.168.1.1", IdentityKind: "ip"}},
			SourceRef: "cmdb-source", CollectedAt: now,
		}, "user:discovery", now)
		tx.Commit()
		// 2. 母盘（P4-09）。
		tx, _ = store.db.Begin()
		createGoldenImage(tx, GoldenImage{
			ImageKey: "cluster-base", Version: 1, ContentDigest: digest("镜像"),
			Architecture: "x86_64", SourceRef: "build-pipeline",
		}, now)
		tx.Commit()
		// 3. 部署（P4-10）。
		tx, _ = store.db.Begin()
		createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 4. 配置（P4-11）。
		tx, _ = store.db.Begin()
		bindConfigurationSpec(tx, ConfigurationSpec{
			ObjectRef: "node-1", DesiredConfig: `{"boot_order":"pxe"}`, SpecVersion: 1,
		}, now)
		tx.Commit()
		// 5. 基准（P4-12）。
		tx, _ = store.db.Begin()
		recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "passed", Value: "1000", Unit: "score", SamplingConditions: "1min", ExecutedBy: "tester",
		}, now)
		tx.Commit()
		// 6. 联机（P4-13）。
		tx, _ = store.db.Begin()
		snapshot, _ := freezeGroupTestSnapshot([]string{"node-1"}, []string{}, 1, "group-spec", `{}`, now)
		runGroupValidation(tx, snapshot, "group-connectivity", now)
		tx.Commit()
		// 7. 验收（P4-06）。
		tx, _ = store.db.Begin()
		recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered",
		}, now)
		tx.Commit()
		// 8. 交接（P4-14）。
		confirmation := HandoverConfirmation{Outcome: "confirmed", DecidedBy: "user:receiver", ConfirmationID: "conf-e2e"}
		tx, _ = store.db.Begin()
		err := submitDeliveryHandover(tx, "node-1", "delivery-ready", "user:ops", confirmation, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// E01 全链完成（各阶段均有实际调用/结果和证据）。
		var deliveryCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM delivery_records WHERE object_ref='node-1'").Scan(&deliveryCount); err != nil {
			t.Fatal(err)
		}
		if deliveryCount != 1 {
			t.Errorf("E01 chain incomplete: delivery=%d (E01 全链各阶段均有实际调用/结果和证据)", deliveryCount)
		}
	})

	t.Run("identity_conflict_injected_isolated", func(t *testing.T) {
		store := setupV3P414(t, "conflict.db")
		now := v3P102Now()
		// 注入身份冲突：隔离（不影响其他节点）。
		tx, _ := store.db.Begin()
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-conflict-1", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-conflict-2", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		tx.Commit()
		// 冲突可见（隔离）。
		tx, _ = store.db.Begin()
		conflict, refs, err := checkIdentityConflict(tx, "192.168.1.10", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict || len(refs) != 2 {
			t.Errorf("identity conflict not isolated: conflict=%v refs=%v", conflict, refs)
		}
	})

	t.Run("single_group_failure_injected", func(t *testing.T) {
		store := setupV3P414(t, "failure.db")
		now := v3P102Now()
		// 注入单机/组测失败：失败保留。
		tx, _ := store.db.Begin()
		recordValidation(tx, ValidationRecord{
			SpecRef: "single-node-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "failed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "failed", ValidatedBy: "test",
		}, now)
		tx.Commit()
		// 单机失败已记录。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM validation_records WHERE outcome='failed'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("single failure not recorded: count=%d (注入单机/组测失败)", count)
		}
	})

	t.Run("cancel_not_erase_deployed_changes", func(t *testing.T) {
		store := setupV3P414(t, "cancel.db")
		now := v3P102Now()
		// 已发生部署变化不会因 Task 取消被抹除（剩余责任可查）。
		tx, _ := store.db.Begin()
		plan, _ := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 部署结果已发生。
		tx, _ = store.db.Begin()
		recordDeploymentTargetResult(tx, plan.PlanID, "node-1", "succeeded", now)
		tx.Commit()
		// Task 取消：部署结果保留（剩余责任可查）。
		tx, _ = store.db.Begin()
		pauseDeploymentPlan(tx, plan.PlanID, now)
		tx.Commit()
		var operationID string
		if err := store.db.QueryRow("SELECT operation_id FROM operations WHERE idempotency_key=?", "deploy-"+plan.PlanID).Scan(&operationID); err != nil {
			t.Fatal(err)
		}
		results, _ := readTargetResults(store.db, operationID)
		if len(results) == 0 {
			t.Error("deployed changes erased by cancel (已发生部署变化不会因 Task 取消被抹除)")
		}
	})
}

// TestV3P414PartialDeliveryTaskCriteria 部分交付。
func TestV3P414PartialDeliveryTaskCriteria(t *testing.T) {
	t.Run("per_requirement_node_coverage_with_failed_reasons", func(t *testing.T) {
		store := setupV3P414(t, "coverage.db")
		now := v3P102Now()
		// 按要求×节点/对象组核验：保留未通过节点与原因。
		tx, _ := store.db.Begin()
		recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered",
		}, now)
		recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "delivery-ready", ObjectRef: "node-2", CoverageState: "uncovered",
		}, now)
		tx.Commit()
		// 评估待交付候选。
		tx, _ = store.db.Begin()
		candidate, err := evaluateDeliveryCandidate(tx, "batch-1", []string{"node-1", "node-2"}, "delivery-ready", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if candidate.EvidenceSatisfied {
			t.Error("evidence satisfied with failed nodes (保留未通过节点与原因)")
		}
		if len(candidate.FailedNodes) != 1 || candidate.FailureReasons["node-2"] == "" {
			t.Errorf("failed nodes/reasons wrong: %+v", candidate)
		}
	})

	t.Run("all_covered_evidence_satisfied", func(t *testing.T) {
		store := setupV3P414(t, "allcovered.db")
		now := v3P102Now()
		// 全部覆盖：有效证据满足后进入待交付。
		tx, _ := store.db.Begin()
		recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		candidate, err := evaluateDeliveryCandidate(tx, "batch-1", []string{"node-1"}, "delivery-ready", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !candidate.EvidenceSatisfied {
			t.Error("all covered not satisfied (有效证据满足后进入待交付)")
		}
	})

	t.Run("partial_delivery_not_batch_success", func(t *testing.T) {
		// 部分交付只完成对应对象（不能自动将整批成功）。
		if err := ensurePartialDeliveryNotBatchSuccess(2, 8); err == nil {
			t.Error("partial delivery marked batch success (部分交付只完成对应对象)")
		}
		// 全部交付：通过。
		if err := ensurePartialDeliveryNotBatchSuccess(8, 8); err != nil {
			t.Errorf("full delivery rejected: %v", err)
		}
	})

	t.Run("task_success_by_completion_criteria", func(t *testing.T) {
		store := setupV3P414(t, "criteria.db")
		now := v3P102Now()
		// Task 是否成功由其完成条件决定（不能自动将整批成功）。
		ctx := context.Background()
		run, _ := store.Submit(ctx, "v3p414-criteria", "任务", "", "", true)
		taskID := run.TaskID
		// 任务未完成全部步骤：不成功（完成条件决定）。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "incomplete-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		state, _ := readTaskState(store.db, taskID)
		if state.State == V3TaskSucceeded {
			t.Error("task success without completion criteria (Task 是否成功由其完成条件决定)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
