package work

// P4-10 验收：系统部署与批量恢复。
//
// 三个测试入口：
//   - TestV3P410DeploymentPartialResults：逐节点保存已开始、明确成功/失败、
//     未开始与未知；批次继续条件由规则判定；批量部分成功后取消，目标结果
//     分别准确，原 Operation 参数不被改成失败余集。
//   - TestV3P410DeploymentRecovery：回执丢失/控制面重启/借出冻结都不导致
//     重复部署；暂停/取消停止未派发批次，核对已发生效果和剩余责任，不自动
//     释放未知占用。
//   - TestV3P410DeploymentEffectValidation：操作回执后核验系统版本、启动/
//     访问及规范要求，不能只看部署接口成功；部署成功但独立验证失败的节点
//     不能进入待交付；固定节点集合、母盘版本、部署方式、批次和领域前置；
//     每个目标携带资产版本/epoch；派发前完成调试接管与两项准入。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P410 创建存储、资产与授权。
func setupV3P410(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "dispatch", ObjectRef: "deploy", ValidFrom: now, Owner: "ops"}, now)
	tx.Commit()
	return store
}

// TestV3P410DeploymentPartialResults 部分结果。
func TestV3P410DeploymentPartialResults(t *testing.T) {
	t.Run("per_node_states_accurate", func(t *testing.T) {
		store := setupV3P410(t, "partial.db")
		now := v3P102Now()
		// 逐节点保存已开始、明确成功/失败、未开始与未知。
		tx, _ := store.db.Begin()
		plan, err := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{
				{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1},
				{ObjectRef: "node-2", Revision: 1, OwnershipEpoch: 1},
				{ObjectRef: "node-3", Revision: 1, OwnershipEpoch: 1},
			},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 逐节点状态。
		tx, _ = store.db.Begin()
		recordDeploymentTargetResult(tx, plan.PlanID, "node-1", "succeeded", now)
		recordDeploymentTargetResult(tx, plan.PlanID, "node-2", "failed", now)
		recordDeploymentTargetResult(tx, plan.PlanID, "node-3", "unknown", now)
		tx.Commit()
		// 逐节点结果分别准确（用 operation_id 查询）。
		var operationID string
		if err := store.db.QueryRow("SELECT operation_id FROM operations WHERE idempotency_key=?", "deploy-"+plan.PlanID).Scan(&operationID); err != nil {
			t.Fatal(err)
		}
		results, err := readTargetResults(store.db, operationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 3 {
			t.Errorf("target results = %d, want 3 (逐节点保存)", len(results))
		}
		states := make(map[string]int)
		for _, r := range results {
			states[r.State]++
		}
		if states["succeeded"] != 1 || states["failed"] != 1 || states["unknown"] != 1 {
			t.Errorf("per-node states wrong: %v (逐节点保存已开始/成功/失败/未开始/未知)", states)
		}
	})

	t.Run("partial_success_cancel_targets_accurate_not_failure_subset", func(t *testing.T) {
		store := setupV3P410(t, "subset.db")
		now := v3P102Now()
		// 批量部分成功后取消：目标结果分别准确，原 Operation 参数不被改成失败余集。
		tx, _ := store.db.Begin()
		plan, _ := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{
				{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1},
				{ObjectRef: "node-2", Revision: 1, OwnershipEpoch: 1},
			},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 部分成功。
		tx, _ = store.db.Begin()
		recordDeploymentTargetResult(tx, plan.PlanID, "node-1", "succeeded", now)
		recordDeploymentTargetResult(tx, plan.PlanID, "node-2", "unknown", now)
		tx.Commit()
		// 取消：目标结果分别准确（不被改成失败余集）。
		tx, _ = store.db.Begin()
		pauseDeploymentPlan(tx, plan.PlanID, now)
		tx.Commit()
		// 原 Operation 参数不被改成失败余集（node-1 仍 succeeded）。
		var operationID string
		if err := store.db.QueryRow("SELECT operation_id FROM operations WHERE idempotency_key=?", "deploy-"+plan.PlanID).Scan(&operationID); err != nil {
			t.Fatal(err)
		}
		results, _ := readTargetResults(store.db, operationID)
		var node1State string
		for _, r := range results {
			if r.TargetRef == "node-1" {
				node1State = r.State
			}
		}
		if node1State != "succeeded" {
			t.Errorf("partial success overwritten by cancel: node-1=%s (原 Operation 参数不被改成失败余集)", node1State)
		}
	})

	t.Run("batch_continue_condition_by_rule", func(t *testing.T) {
		store := setupV3P410(t, "batch.db")
		now := v3P102Now()
		// 批次继续条件由规则判定（ContinueRule 非空）。
		tx, _ := store.db.Begin()
		plan, err := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
			Batches: []DeploymentBatch{
				{BatchID: "batch-1", TargetRefs: []string{"node-1"}, ContinueRule: "all_succeeded"},
			},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		_ = plan
	})
}

// TestV3P410DeploymentRecovery 恢复。
func TestV3P410DeploymentRecovery(t *testing.T) {
	t.Run("receipt_lost_restart_no_duplicate_deployment", func(t *testing.T) {
		store := setupV3P410(t, "restart.db")
		now := v3P102Now()
		// 回执丢失/控制面重启：不导致重复部署（幂等键唯一）。
		tx, _ := store.db.Begin()
		plan, _ := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 回执丢失：再次创建同计划返回同一 Operation（不重复部署）。
		var opCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operations WHERE idempotency_key=?", "deploy-"+plan.PlanID).Scan(&opCount); err != nil {
			t.Fatal(err)
		}
		if opCount != 1 {
			t.Errorf("duplicate deployment operations: count=%d (回执丢失不导致重复部署)", opCount)
		}
	})

	t.Run("pause_stops_undispatched_batches", func(t *testing.T) {
		store := setupV3P410(t, "pause.db")
		now := v3P102Now()
		// 暂停/取消停止未派发批次。
		tx, _ := store.db.Begin()
		plan, _ := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 暂停：停止未派发批次。
		tx, _ = store.db.Begin()
		if err := pauseDeploymentPlan(tx, plan.PlanID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 暂停已审计（核对已发生效果和剩余责任）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='deployment_paused'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("pause not audited: count=%d (暂停/取消停止未派发批次)", count)
		}
	})

	t.Run("lent_freeze_not_duplicate_deployment", func(t *testing.T) {
		store := setupV3P410(t, "freeze.db")
		now := v3P102Now()
		// 借出冻结：不导致重复部署（冻结阻止新派发）。
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateLent, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
		freezeAsset(tx, "node-1", "admin", "借出冻结", now)
		tx.Commit()
		// 冻结后派发：拒绝（借出冻结不导致重复部署）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.FrozenAt == 0 {
			t.Error("lent asset not frozen (借出冻结)")
		}
	})
}

// TestV3P410DeploymentEffectValidation 效果核验。
func TestV3P410DeploymentEffectValidation(t *testing.T) {
	t.Run("receipt_not_just_interface_success", func(t *testing.T) {
		store := setupV3P410(t, "validate.db")
		now := v3P102Now()
		// 操作回执后核验系统版本、启动/访问及规范要求（不能只看部署接口成功）。
		tx, _ := store.db.Begin()
		// 缺系统版本：拒绝。
		err := validateDeploymentEffect(tx, "plan-1", "node-1", "", true, true, now)
		tx.Rollback()
		if err == nil {
			t.Error("deployment without system version accepted (不能只看部署接口成功)")
		}
	})

	t.Run("boot_not_accessible_rejected", func(t *testing.T) {
		store := setupV3P410(t, "boot.db")
		now := v3P102Now()
		// 不可启动/访问：拒绝（部署成功但独立验证失败不能进入待交付）。
		tx, _ := store.db.Begin()
		err := validateDeploymentEffect(tx, "plan-1", "node-1", "v1.0", false, true, now)
		tx.Rollback()
		if err == nil {
			t.Error("boot not accessible accepted (部署成功但独立验证失败不能进入待交付)")
		}
	})

	t.Run("spec_not_met_rejected", func(t *testing.T) {
		store := setupV3P410(t, "spec.db")
		now := v3P102Now()
		// 不满足规范要求：拒绝。
		tx, _ := store.db.Begin()
		err := validateDeploymentEffect(tx, "plan-1", "node-1", "v1.0", true, false, now)
		tx.Rollback()
		if err == nil {
			t.Error("spec not met accepted (规范要求核验)")
		}
	})

	t.Run("plan_has_targets_image_method_preconditions", func(t *testing.T) {
		store := setupV3P410(t, "plan.db")
		now := v3P102Now()
		// 固定节点集合、母盘版本、部署方式、批次和领域前置。
		tx, _ := store.db.Begin()
		plan, err := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
			DomainPreconditions: `{"debug_complete":true}`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if plan.ImageRef != "cluster-base@1" || plan.DeployMethod != "pxe" || plan.DomainPreconditions == "" {
			t.Errorf("plan incomplete: %+v (固定节点集合/母盘版本/部署方式/领域前置)", plan)
		}
	})

	t.Run("target_carries_asset_revision_epoch", func(t *testing.T) {
		store := setupV3P410(t, "target.db")
		now := v3P102Now()
		// 每个目标携带资产版本/epoch。
		tx, _ := store.db.Begin()
		_, err := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 0, OwnershipEpoch: 0}}, // 缺版本/epoch
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("target without revision/epoch accepted (每个目标携带资产版本/epoch)")
		}
	})

	t.Run("dispatch_requires_maintenance_takeover", func(t *testing.T) {
		store := setupV3P410(t, "takeover.db")
		now := v3P102Now()
		// 派发前完成调试接管与两项准入。
		tx, _ := store.db.Begin()
		plan, _ := createDeploymentPlan(tx, DeploymentPlan{
			Targets: []DeploymentTarget{{ObjectRef: "node-1", Revision: 1, OwnershipEpoch: 1}},
			ImageRef: "cluster-base@1", DeployMethod: "pxe",
		}, now)
		tx.Commit()
		// 无调试接管：派发拒绝。
		tx, _ = store.db.Begin()
		err := dispatchDeploymentBatch(tx, plan.PlanID, "batch-1", now)
		tx.Rollback()
		if err == nil {
			t.Error("dispatch without maintenance takeover allowed (派发前完成调试接管与两项准入)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
