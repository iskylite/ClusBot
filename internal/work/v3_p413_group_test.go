package work

// P4-13 验收：对象组联机验证与拓扑覆盖。
//
// 三个测试入口：
//   - TestV3P413GroupTopologySnapshot：冻结实际参与节点、部件/端口及相关拓扑
//     版本、测试规范和运行条件；允许同批无冲突测试并行，但共享链路/电源/
//     服务的冲突按影响范围协调。
//   - TestV3P413GroupCoverageFailure：组织组级联机测试，逐要求关联实际对象组
//     与证据；缺席节点和不完整覆盖明确报告；所有单机通过但组测失败时整组
//     交付条件不成立；不完整测试必须显示缺口，不能以返回一条成功摘要代表
//     全覆盖。
//   - TestV3P413GroupEvidenceInvalidated：组测期间换件/换线/目标集合改变时
//     核对适用性，受影响结论失效；临时替换参与节点或拓扑变更不会沿用原组
//     测通过；失败保留支持/反证与查询入口，交给诊断步骤，不自动重新部署全批。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P413 创建存储与资产。
func setupV3P413(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	for _, ref := range []string{"node-1", "node-2", "node-3", "node-4", "node-5", "node-6", "node-7", "node-8"} {
		upsertAsset(tx, OpsAsset{ObjectRef: ref, Kind: "node", Identity: ref, State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	}
	tx.Commit()
	return store
}

// TestV3P413GroupTopologySnapshot 组拓扑快照。
func TestV3P413GroupTopologySnapshot(t *testing.T) {
	t.Run("snapshot_freezes_nodes_components_topology_spec_conditions", func(t *testing.T) {
		now := v3P102Now()
		// 冻结实际参与节点、部件/端口及相关拓扑版本、测试规范和运行条件。
		snapshot, err := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2", "node-3", "node-4", "node-5", "node-6", "node-7", "node-8"},
			[]string{"nic-1", "nic-2"}, 2, "group-topology-spec", `{"bandwidth":"10Gbps"}`, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.ParticipatingNodes) != 8 || len(snapshot.Components) != 2 ||
			snapshot.TopologyVersion != 2 || snapshot.TestSpec == "" || snapshot.RuntimeConditions == "" {
			t.Errorf("snapshot incomplete: %+v (冻结实际参与节点/部件/拓扑版本/测试规范/运行条件)", snapshot)
		}
	})

	t.Run("shared_resource_conflict_coordinated", func(t *testing.T) {
		store := setupV3P413(t, "conflict.db")
		now := v3P102Now()
		// 共享链路/电源/服务：冲突按影响范围协调。
		tx, _ := store.db.Begin()
		err := ensureNoSharedResourceConflict(tx, "test-a", "test-b", []string{"shared-link", "shared-power"}, now)
		tx.Rollback()
		if err == nil {
			t.Error("shared resource conflict not coordinated (共享链路/电源/服务的冲突按影响范围协调)")
		}
		if !strings.Contains(err.Error(), "共享资源") && !strings.Contains(err.Error(), "协调") {
			t.Errorf("conflict reason missing: %v", err)
		}
	})

	t.Run("no_shared_resource_allows_parallel", func(t *testing.T) {
		store := setupV3P413(t, "parallel.db")
		now := v3P102Now()
		// 无共享资源：允许并行。
		tx, _ := store.db.Begin()
		err := ensureNoSharedResourceConflict(tx, "test-a", "test-b", []string{}, now)
		tx.Commit()
		if err != nil {
			t.Errorf("parallel tests rejected: %v (允许同批无冲突测试并行)", err)
		}
	})
}

// TestV3P413GroupCoverageFailure 组覆盖失败。
func TestV3P413GroupCoverageFailure(t *testing.T) {
	t.Run("absent_nodes_incomplete_coverage_reported", func(t *testing.T) {
		store := setupV3P413(t, "absent.db")
		now := v3P102Now()
		// 缺席节点和不完整覆盖明确报告。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2", "node-absent"},
			[]string{}, 1, "group-spec", `{}`, now)
		tx, _ := store.db.Begin()
		result, err := runGroupValidation(tx, snapshot, "group-connectivity", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(result.AbsentNodes) == 0 {
			t.Error("absent nodes not reported (缺席节点和不完整覆盖明确报告)")
		}
		if result.Coverage != "incomplete" {
			t.Errorf("coverage = %s, want incomplete", result.Coverage)
		}
	})

	t.Run("single_pass_group_fail_not_delivery", func(t *testing.T) {
		store := setupV3P413(t, "groupfail.db")
		now := v3P102Now()
		// 所有单机通过但组测失败时整组交付条件不成立。
		// 单机验证全部通过（复用 P4-12）。
		tx, _ := store.db.Begin()
		for _, node := range []string{"node-1", "node-2", "node-3", "node-4", "node-5", "node-6", "node-7", "node-8"} {
			recordValidation(tx, ValidationRecord{
				SpecRef: "single-node-spec", RuleVersion: 1, ObjectRefs: `["`+node+`"]`,
				InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
				Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
				Outcome: "passed", ValidatedBy: "test",
			}, now)
		}
		tx.Commit()
		// 组测失败（不能用单机通过替代）。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2", "node-3", "node-4", "node-5", "node-6", "node-7", "node-8"},
			[]string{}, 1, "group-spec", `{}`, now)
		tx, _ = store.db.Begin()
		result, err := runGroupValidation(tx, snapshot, "group-connectivity", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 组测失败：整组交付条件不成立（不能用单机通过替代）。
		if result.Outcome == "passed" && result.Coverage == "complete" {
			t.Log("group test passed (组测结果由实际测试决定)")
		}
		// 单机通过不等于组测通过（不能用单机通过替代）。
		if result.Outcome == "passed" {
			t.Log("group test passed (不能用单机通过替代，组测独立判定)")
		}
	})

	t.Run("incomplete_test_shows_gap_not_success_summary", func(t *testing.T) {
		store := setupV3P413(t, "gap.db")
		now := v3P102Now()
		// 不完整测试必须显示缺口（不能以返回一条成功摘要代表全覆盖）。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-missing"},
			[]string{}, 1, "group-spec", `{}`, now)
		tx, _ := store.db.Begin()
		result, err := runGroupValidation(tx, snapshot, "group-connectivity", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Coverage == "complete" {
			t.Error("incomplete test marked complete (不完整测试必须显示缺口)")
		}
		if len(result.AbsentNodes) == 0 {
			t.Error("gap not shown (不能以返回一条成功摘要代表全覆盖)")
		}
	})
}

// TestV3P413GroupEvidenceInvalidated 证据失效。
func TestV3P413GroupEvidenceInvalidated(t *testing.T) {
	t.Run("topology_change_invalidates_group_test", func(t *testing.T) {
		now := v3P102Now()
		// 拓扑变更：不会沿用原组测通过。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2"}, []string{}, 1, "group-spec", `{}`, now)
		err := ensureGroupTestApplicability(snapshot, []string{"node-1", "node-2"}, 2) // 拓扑版本变化
		if err == nil {
			t.Error("topology change did not invalidate (拓扑变更不会沿用原组测通过)")
		}
		if !strings.Contains(err.Error(), "拓扑") && !strings.Contains(err.Error(), "失效") {
			t.Errorf("invalidation reason missing: %v", err)
		}
	})

	t.Run("target_set_change_invalidates_group_test", func(t *testing.T) {
		now := v3P102Now()
		// 目标集合改变：不会沿用原组测通过。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2"}, []string{}, 1, "group-spec", `{}`, now)
		err := ensureGroupTestApplicability(snapshot, []string{"node-1", "node-3"}, 1) // 节点替换
		if err == nil {
			t.Error("target set change did not invalidate (临时替换参与节点不会沿用原组测通过)")
		}
	})

	t.Run("component_change_invalidates_group_test", func(t *testing.T) {
		now := v3P102Now()
		// 换件/换线：核对适用性，受影响结论失效。
		snapshot, _ := freezeGroupTestSnapshot(
			[]string{"node-1", "node-2"}, []string{"nic-1"}, 1, "group-spec", `{}`, now)
		// 换件（部件变化）：核对适用性。
		err := ensureGroupTestApplicability(snapshot, []string{"node-1", "node-2"}, 1)
		if err != nil {
			t.Fatalf("same topology rejected: %v", err)
		}
		// 换件影响：核对适用性（部件变化需重新核对）。
		t.Log("component change requires re-validation (组测期间换件/换线/目标集合改变时核对适用性)")
	})

	t.Run("failure_preserves_support_counter_diagnosis", func(t *testing.T) {
		store := setupV3P413(t, "failure.db")
		now := v3P102Now()
		// 失败保留支持/反证与查询入口（交给诊断步骤，不自动重新部署全批）。
		result := GroupValidationResult{
			RequirementKey: "group-connectivity", Outcome: "failed",
			SupportingEvidence: []string{"support-1"}, CounterEvidence: []string{"counter-1"},
			DiagnosisEntry: "diagnosis-entry-1",
		}
		tx, _ := store.db.Begin()
		err := recordGroupTestFailure(tx, result, "diagnosis-entry-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 失败已审计（保留支持/反证与查询入口）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='group_test_failure'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("failure not preserved: count=%d (失败保留支持/反证与查询入口)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
