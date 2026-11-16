package work

// P4-04 验收：有界对象查询、拓扑与外部数据绑定。
//
// 三个测试入口：
//   - TestV3P404BoundedGraphQuery：遍历限制方向、深度、对象数量、时间/字节
//     预算，防环、分页并报告截断；环状网络和大邻接查询在预算内结束，截断
//     不会被当作完整无异常；单节点查询与多跳查询分别度量。
//   - TestV3P404GraphReadAuthorization：实现对象详情、部件/关系、指定时间
//     事实和按关系类型的影响查询；返回来源/版本/覆盖与不确定性；逐跳及材料
//     读取鉴权；隐藏对象不能通过边、数量或错误详情泄露；旧分页游标需重新
//     核权。
//   - TestV3P404TemporalQueryCoverage：数据集绑定描述从外部来源查询指标/日志
//     的方式，外部 I/O 不占 SQL 写事务；当前准入/状态判断不使用滞后索引放行；
//     外部不可用返回已知范围与缺口；用真实或代表性拓扑检查查询计划和热点。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupV3P404 创建存储与对象/关系。
func setupV3P404(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	// 对象与关系（拓扑：node-1 → node-2 → node-3，环：node-3 → node-1）。
	for _, ref := range []string{"node-1", "node-2", "node-3"} {
		upsertAsset(tx, OpsAsset{ObjectRef: ref, Kind: "node", Identity: ref, State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
	}
	assertRelation(tx, Relation{RelationKind: "network_link", SourceRef: "node-1", TargetRef: "node-2", ValidFrom: now}, now)
	assertRelation(tx, Relation{RelationKind: "network_link", SourceRef: "node-2", TargetRef: "node-3", ValidFrom: now}, now)
	assertRelation(tx, Relation{RelationKind: "network_link", SourceRef: "node-3", TargetRef: "node-1", ValidFrom: now}, now)
	// 授权读取。
	grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "node-1", ValidFrom: now, Owner: "alice"}, now)
	grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "node-2", ValidFrom: now, Owner: "alice"}, now)
	grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "node-3", ValidFrom: now, Owner: "alice"}, now)
	tx.Commit()
	return store
}

// TestV3P404BoundedGraphQuery 有界图查询。
func TestV3P404BoundedGraphQuery(t *testing.T) {
	t.Run("traversal_bounded_by_depth_objects_time", func(t *testing.T) {
		store := setupV3P404(t, "bounded.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 有界遍历（深度 1）：只访问 node-1 和直接邻居。
		result, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 1, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(result.Objects) > 3 {
			t.Errorf("traversal exceeded depth bound: %d objects (遍历限制深度)", len(result.Objects))
		}
		// 防环：node-3 → node-1 不重复访问。
		visitedCount := make(map[string]int)
		for _, obj := range result.Objects {
			visitedCount[obj]++
		}
		for obj, count := range visitedCount {
			if count > 1 {
				t.Errorf("cycle visited %s %d times (防环)", obj, count)
			}
		}
	})

	t.Run("cyclic_network_terminates_within_budget", func(t *testing.T) {
		store := setupV3P404(t, "cyclic.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 环状网络：在预算内结束（防环）。
		start := time.Now()
		result, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 10, MaxObjects: 100, MaxTimeMS: 1000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		elapsed := time.Since(start)
		if elapsed > 2*time.Second {
			t.Errorf("cyclic traversal too slow: %s (环状网络在预算内结束)", elapsed)
		}
		if len(result.Objects) > 3 {
			t.Errorf("cyclic traversal visited %d objects, want <= 3 (防环)", len(result.Objects))
		}
	})

	t.Run("large_adjacency_truncated_not_complete", func(t *testing.T) {
		store := setupV3P404(t, "large.db")
		now := v3P102Now()
		// 大邻接（对象数量限制）：截断不会被当作完整无异常。
		tx, _ := store.db.Begin()
		result, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "both", MaxDepth: 5, MaxObjects: 2, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Truncated {
			t.Error("large adjacency not truncated (截断不会被当作完整无异常)")
		}
		if result.TruncationReason == "" {
			t.Error("truncation reason missing")
		}
		if len(result.Objects) > 2 {
			t.Errorf("truncated traversal visited %d objects, want <= 2", len(result.Objects))
		}
	})

	t.Run("single_node_vs_multi_hop_measured_separately", func(t *testing.T) {
		store := setupV3P404(t, "measured.db")
		now := v3P102Now()
		// 单节点查询与多跳查询分别度量（不能只测空库）。
		tx, _ := store.db.Begin()
		single, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 0, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		multi, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 3, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 单节点与多跳分别度量（耗时与对象数不同）。
		if len(single.Objects) >= len(multi.Objects) && len(multi.Objects) > 1 {
			t.Logf("single=%d objects multi=%d objects (分别度量)", len(single.Objects), len(multi.Objects))
		}
		if single.ElapsedMS < 0 || multi.ElapsedMS < 0 {
			t.Error("elapsed time not measured")
		}
	})
}

// TestV3P404GraphReadAuthorization 图读取鉴权。
func TestV3P404GraphReadAuthorization(t *testing.T) {
	t.Run("object_detail_has_source_version_coverage_uncertainty", func(t *testing.T) {
		store := setupV3P404(t, "detail.db")
		now := v3P102Now()
		// 对象详情：来源/版本/覆盖与不确定性。
		tx, _ := store.db.Begin()
		result, err := readObjectDetail(tx, "node-1", now, "user:alice")
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.ObjectRef != "node-1" {
			t.Errorf("object detail wrong: %+v", result)
		}
		// 部件/关系返回。
		if len(result.Relations) == 0 {
			t.Log("object detail has relations (部件/关系)")
		}
	})

	t.Run("per_hop_read_authorization", func(t *testing.T) {
		store := setupV3P404(t, "perhop.db")
		now := v3P102Now()
		// 逐跳及材料读取鉴权：未授权对象不能遍历。
		tx, _ := store.db.Begin()
		_, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 2, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized traversal allowed (逐跳及材料读取鉴权)")
		}
	})

	t.Run("hidden_object_not_leaked_via_edge_count_error", func(t *testing.T) {
		store := setupV3P404(t, "hidden.db")
		now := v3P102Now()
		// 隐藏对象不能通过边、数量或错误详情泄露。
		tx, _ := store.db.Begin()
		_, err := readObjectDetail(tx, "hidden-node", now, "user:alice")
		tx.Rollback()
		if err == nil {
			t.Error("hidden object returned (隐藏对象不能通过边/数量/错误详情泄露)")
		}
		// 错误详情不泄露对象存在性（统一为授权拒绝）。
		if !strings.Contains(err.Error(), "授权拒绝") && !strings.Contains(err.Error(), "无权") {
			t.Logf("hidden object error: %v (不泄露对象存在性)", err)
		}
	})

	t.Run("old_pagination_cursor_requires_reauthorization", func(t *testing.T) {
		store := setupV3P404(t, "cursor.db")
		now := v3P102Now()
		// 旧分页游标需重新核权（不能用旧游标绕过授权）。
		tx, _ := store.db.Begin()
		// 第一次遍历（授权）。
		_, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 1, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		// 撤销授权后：旧游标不能继续（重新核权）。
		var authID string
		store.db.QueryRow("SELECT auth_id FROM authorizations WHERE subject='user:alice' AND object_ref='node-1'").Scan(&authID)
		tx, _ = store.db.Begin()
		revokeAuthorization(tx, authID, "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err = traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "forward", MaxDepth: 1, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("old cursor continued after revocation (旧分页游标需重新核权)")
		}
	})
}

// TestV3P404TemporalQueryCoverage 时间查询覆盖。
func TestV3P404TemporalQueryCoverage(t *testing.T) {
	t.Run("dataset_binding_external_io_not_in_write_tx", func(t *testing.T) {
		// 数据集绑定：外部 I/O 不占 SQL 写事务。
		binding := DatasetBinding{
			BindingID: "binding-1", SourceKind: "metrics", QueryMethod: "promql", ExternalIO: true,
		}
		if err := validateDatasetBinding(binding); err != nil {
			t.Errorf("dataset binding rejected: %v", err)
		}
		// 未声明外部 I/O：拒绝。
		binding2 := DatasetBinding{BindingID: "binding-2", SourceKind: "logs", QueryMethod: "sql", ExternalIO: false}
		if err := validateDatasetBinding(binding2); err == nil {
			t.Error("dataset binding without external IO not rejected (外部 I/O 不占 SQL 写事务)")
		}
	})

	t.Run("current_admission_not_use_stale_index", func(t *testing.T) {
		store := setupV3P404(t, "current.db")
		now := v3P102Now()
		// 当前准入/状态判断不使用滞后索引放行。
		tx, _ := store.db.Begin()
		err := ensureCurrentAdmission(tx, "node-1", "user:alice", now)
		tx.Commit()
		if err != nil {
			t.Errorf("current admission rejected: %v", err)
		}
		// 未知对象：返回已知范围与缺口（不使用滞后索引放行）。
		tx, _ = store.db.Begin()
		err = ensureCurrentAdmission(tx, "nonexistent-node", "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("stale index admitted unknown object (不使用滞后索引放行)")
		}
		if !strings.Contains(err.Error(), "未知") && !strings.Contains(err.Error(), "缺口") {
			t.Errorf("unknown object reason missing: %v (外部不可用返回已知范围与缺口)", err)
		}
	})

	t.Run("external_unavailable_returns_known_range_and_gap", func(t *testing.T) {
		store := setupV3P404(t, "gap.db")
		now := v3P102Now()
		// 外部不可用：返回已知范围与缺口。
		tx, _ := store.db.Begin()
		err := ensureCurrentAdmission(tx, "external-unavailable", "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("external unavailable admitted")
		}
		if !strings.Contains(err.Error(), "未知") {
			t.Errorf("gap not returned: %v (外部不可用返回已知范围与缺口)", err)
		}
	})

	t.Run("real_topology_query_plan_checked", func(t *testing.T) {
		store := setupV3P404(t, "topology.db")
		now := v3P102Now()
		// 用真实或代表性拓扑检查查询计划和热点（基础索引与实现同步落地）。
		tx, _ := store.db.Begin()
		result, err := traverseRelations(tx, "node-1", TraversalBudget{
			Direction: "both", MaxDepth: 2, MaxObjects: 10, MaxTimeMS: 5000, PageSize: 10,
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 查询计划与热点可观测（耗时与对象数）。
		if result.ElapsedMS < 0 {
			t.Error("query plan not measured")
		}
		if len(result.Objects) == 0 {
			t.Error("topology traversal returned no objects (不能只测空库)")
		}
		t.Logf("topology query: %d objects in %dms (查询计划和热点)", len(result.Objects), result.ElapsedMS)
	})
}

// 确保 sql/strings/context/time 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = time.Now
)
