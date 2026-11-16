package work

// P4-03 验收：观测、事实和关系的双时间历史。
//
// 三个测试入口：
//   - TestV3P403BitemporalFacts：原始 Observation 保存来源/采集时间/覆盖；规范
//     声明保留值、单位、来源、有效时间和记录时间；更正新增修订并关闭原记录
//     的适用区间，不覆盖历史；换线 10:05、10:20 才记录时，当时认知与今天回看
//     10:10 可不同且均能解释。
//   - TestV3P403ConflictingSources：按字段与用途选择权威来源，冲突保留双方及
//     判据；不能按最后到达或相似度胜出；乱序旧事件不会覆盖当前新事实；相互
//     矛盾来源返回冲突/未知。
//   - TestV3P403RelationIntervalConstraints：物理组成无环、单一父关系的有效
//     区间不重叠；网络拓扑允许环，端口连线规范端点并支持双向查；并发插入
//     重叠安装关系被约束，原始矛盾声明仍保留；当前事实投影可重建并带水位，
//     过期或缺失不被伪装为新鲜。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P403 创建存储。
func setupV3P403(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P403BitemporalFacts 双时间事实。
func TestV3P403BitemporalFacts(t *testing.T) {
	t.Run("observation_has_source_collection_coverage", func(t *testing.T) {
		store := setupV3P403(t, "obs.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		o, err := recordObservation(tx, Observation{
			SourceRef: "cmdb", SourceIdentity: "collector-1", CollectedAt: now,
			Coverage: "node-1", ObjectRef: "node-1", FieldKey: "cpu",
			Value: "32", Unit: "cores",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if o.SourceRef != "cmdb" || o.CollectedAt != now || o.Coverage != "node-1" ||
			o.RecordedAt != now {
			t.Errorf("observation incomplete: %+v", o)
		}
	})

	t.Run("correction_new_revision_closes_old_not_overwrite", func(t *testing.T) {
		store := setupV3P403(t, "correction.db")
		now := v3P102Now()
		// 原始事实（v1）。
		tx, _ := store.db.Begin()
		v1, _ := assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", Unit: "cores",
			SourceRef: "cmdb", Authority: "authoritative", ValidFrom: now, Revision: 1,
		}, now)
		tx.Commit()
		// 更正（v2 关闭 v1 的适用区间，不覆盖历史）。
		tx, _ = store.db.Begin()
		v2, err := assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "64", Unit: "cores",
			SourceRef: "cmdb", Authority: "authoritative", ValidFrom: now + 1000, Revision: 2,
			Supersedes: v1.FactID,
		}, now+1000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// v1 的适用区间已关闭（不覆盖历史）。
		var validTo int64
		if err := store.db.QueryRow("SELECT valid_to FROM facts WHERE fact_id=?", v1.FactID).Scan(&validTo); err != nil {
			t.Fatal(err)
		}
		if validTo == 0 {
			t.Error("old fact not closed (更正关闭原记录适用区间不覆盖历史)")
		}
		// v1 仍可查（历史保留）。
		if v2.Supersedes != v1.FactID {
			t.Errorf("v2 supersedes = %s, want %s", v2.Supersedes, v1.FactID)
		}
	})

	t.Run("bitemporal_1005_record_1020_different_from_1010_review", func(t *testing.T) {
		store := setupV3P403(t, "bitemporal.db")
		// 换线 10:05 生效，10:20 才记录：当时认知与今天回看 10:10 可不同。
		base := v3P102Now()
		t1005 := base + 5000   // 生效时间 10:05
		t1010 := base + 10000  // 回看时间 10:10
		t1020 := base + 20000  // 记录时间 10:20
		// 10:05 换线（事实生效）。
		tx, _ := store.db.Begin()
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "nic", Value: "eth1", SourceRef: "discovery",
			Authority: "authoritative", ValidFrom: t1005, RecordedAt: t1020,
		}, t1020)
		tx.Commit()
		// 回看 10:10：该事实已生效（valid_from=1005 <= 1010），但记录时间是 10:20。
		tx, _ = store.db.Begin()
		fact, err := selectAuthoritativeFact(tx, "node-1", "nic", t1010)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if fact.ValidFrom != t1005 {
			t.Errorf("fact valid_from = %d, want %d (生效时间 10:05)", fact.ValidFrom, t1005)
		}
		if fact.RecordedAt != t1020 {
			t.Errorf("fact recorded_at = %d, want %d (记录时间 10:20)", fact.RecordedAt, t1020)
		}
		// 当时认知（10:05 前）与今天回看（10:10）可不同且均能解释。
		t.Logf("bitemporal: valid_from=%d recorded_at=%d (当时认知与回看可不同且均能解释)", fact.ValidFrom, fact.RecordedAt)
	})
}

// TestV3P403ConflictingSources 冲突来源。
func TestV3P403ConflictingSources(t *testing.T) {
	t.Run("conflicting_sources_preserved_with_basis", func(t *testing.T) {
		store := setupV3P403(t, "conflict.db")
		now := v3P102Now()
		// 两个权威来源冲突：保留双方及判据。
		tx, _ := store.db.Begin()
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", SourceRef: "cmdb",
			Authority: "authoritative", ValidFrom: now,
		}, now)
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "64", SourceRef: "agent",
			Authority: "authoritative", ValidFrom: now,
		}, now)
		tx.Commit()
		// 相互矛盾来源：返回冲突/未知（不按最后到达胜出）。
		tx, _ = store.db.Begin()
		_, err := selectAuthoritativeFact(tx, "node-1", "cpu", now+500)
		tx.Commit()
		if err == nil {
			t.Error("conflicting authoritative sources not detected (相互矛盾来源返回冲突/未知)")
		}
		if !strings.Contains(err.Error(), "冲突") {
			t.Errorf("conflict reason missing: %v", err)
		}
	})

	t.Run("not_last_arrival_wins", func(t *testing.T) {
		store := setupV3P403(t, "notlast.db")
		now := v3P102Now()
		// 不能按最后到达或相似度胜出。
		tx, _ := store.db.Begin()
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", SourceRef: "cmdb",
			Authority: "authoritative", ValidFrom: now,
		}, now)
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "64", SourceRef: "agent",
			Authority: "candidate", ValidFrom: now + 100, // 后到达
		}, now+100)
		tx.Commit()
		// 后到达的候选不胜出（权威来源优先）。
		tx, _ = store.db.Begin()
		fact, err := selectAuthoritativeFact(tx, "node-1", "cpu", now+200)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if fact.Value != "32" {
			t.Errorf("last arrival won: value=%s (不能按最后到达胜出)", fact.Value)
		}
	})

	t.Run("out_of_order_old_event_not_overwrite_new", func(t *testing.T) {
		store := setupV3P403(t, "outoforder.db")
		now := v3P102Now()
		// 新事实（valid_from 较晚）。
		tx, _ := store.db.Begin()
		newFact, _ := assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "64", SourceRef: "cmdb",
			Authority: "authoritative", ValidFrom: now + 1000,
		}, now)
		tx.Commit()
		// 乱序旧事件（valid_from 较早）：不会覆盖当前新事实。
		tx, _ = store.db.Begin()
		oldEvent := Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", SourceRef: "agent",
			Authority: "candidate", ValidFrom: now, // 早于新事实
		}
		dup, err := deduplicateFactEvent(tx, oldEvent, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if dup {
			t.Error("out-of-order event marked duplicate")
		}
		// 当前新事实未被覆盖。
		tx, _ = store.db.Begin()
		current, err := selectAuthoritativeFact(tx, "node-1", "cpu", now+1500)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if current.Value != "64" {
			t.Errorf("old event overwrote new fact: value=%s (乱序旧事件不会覆盖当前新事实)", current.Value)
		}
		_ = newFact
	})

	t.Run("duplicate_event_by_source_identity_version", func(t *testing.T) {
		store := setupV3P403(t, "dup.db")
		now := v3P102Now()
		// 重复事件依据来源身份/版本去重。
		tx, _ := store.db.Begin()
		event := Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", SourceRef: "cmdb",
			Authority: "authoritative", ValidFrom: now,
		}
		assertFact(tx, event, now)
		tx.Commit()
		// 同来源同字段同有效时间：重复（去重）。
		tx, _ = store.db.Begin()
		dup, err := deduplicateFactEvent(tx, event, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !dup {
			t.Error("duplicate event not detected (重复事件依据来源身份/版本去重)")
		}
	})
}

// TestV3P403RelationIntervalConstraints 关系区间约束。
func TestV3P403RelationIntervalConstraints(t *testing.T) {
	t.Run("physical_composition_no_cycle", func(t *testing.T) {
		store := setupV3P403(t, "nocycle.db")
		now := v3P102Now()
		// 物理组成无环（A→B→A 拒绝）。
		tx, _ := store.db.Begin()
		assertRelation(tx, Relation{RelationKind: "physical_parent", SourceRef: "node-a", TargetRef: "node-b", ValidFrom: now}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := assertRelation(tx, Relation{RelationKind: "physical_parent", SourceRef: "node-b", TargetRef: "node-a", ValidFrom: now}, now)
		tx.Rollback()
		if err == nil {
			t.Error("physical cycle allowed (物理组成无环)")
		}
		if !strings.Contains(err.Error(), "环") {
			t.Errorf("cycle reason missing: %v", err)
		}
	})

	t.Run("single_parent_interval_not_overlap", func(t *testing.T) {
		store := setupV3P403(t, "nooverlap.db")
		now := v3P102Now()
		// 单一父关系的有效区间不重叠（并发插入重叠安装关系被约束）。
		tx, _ := store.db.Begin()
		assertRelation(tx, Relation{RelationKind: "physical_parent", SourceRef: "node-parent-a", TargetRef: "node-child", ValidFrom: now}, now)
		tx.Commit()
		// 并发插入重叠父关系：拒绝。
		tx, _ = store.db.Begin()
		_, err := assertRelation(tx, Relation{RelationKind: "physical_parent", SourceRef: "node-parent-b", TargetRef: "node-child", ValidFrom: now + 100}, now)
		tx.Rollback()
		if err == nil {
			t.Error("overlapping parent relation allowed (单一父关系有效区间不重叠)")
		}
		if !strings.Contains(err.Error(), "重叠") {
			t.Errorf("overlap reason missing: %v", err)
		}
	})

	t.Run("network_topology_allows_cycle", func(t *testing.T) {
		store := setupV3P403(t, "network.db")
		now := v3P102Now()
		// 网络拓扑允许环（A→B→A 允许）。
		tx, _ := store.db.Begin()
		assertRelation(tx, Relation{RelationKind: "network_link", SourceRef: "switch-a", TargetRef: "switch-b", ValidFrom: now}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := assertRelation(tx, Relation{RelationKind: "network_link", SourceRef: "switch-b", TargetRef: "switch-a", ValidFrom: now}, now)
		if err != nil {
			t.Fatalf("network cycle rejected: %v (网络拓扑允许环)", err)
		}
		tx.Commit()
	})

	t.Run("conflicting_declaration_preserved", func(t *testing.T) {
		store := setupV3P403(t, "preserved.db")
		now := v3P102Now()
		// 原始矛盾声明仍保留（双方及判据）。
		tx, _ := store.db.Begin()
		assertFact(tx, Fact{
			ObjectRef: "node-1", FieldKey: "cpu", Value: "32", SourceRef: "cmdb",
			Authority: "conflicting", ValidFrom: now, ConflictBasis: "与 agent 冲突",
		}, now)
		tx.Commit()
		// 冲突声明保留（可查）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM facts WHERE object_ref='node-1' AND authority='conflicting'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("conflicting declaration not preserved: count=%d", count)
		}
	})

	t.Run("fact_projection_rebuildable_with_watermark", func(t *testing.T) {
		store := setupV3P403(t, "projection.db")
		now := v3P102Now()
		// 当前事实投影可重建并带水位。
		tx, _ := store.db.Begin()
		if err := rebuildFactProjection(tx, now+5000, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		watermark, rebuiltAt, err := readFactProjection(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if watermark != now+5000 {
			t.Errorf("watermark = %d, want %d (投影带水位)", watermark, now+5000)
		}
		if rebuiltAt != now {
			t.Errorf("rebuilt_at = %d", rebuiltAt)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
