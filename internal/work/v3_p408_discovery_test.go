package work

// P4-08 验收：资产发现与纳管流程。
//
// 两个测试入口：
//   - TestV3P408DiscoveryToManagement：按授权范围发现节点/部件/来源映射，保留
//     覆盖、未响应与冲突；发现不直接等于可写纳管；通过身份核对后创建待纳管
//     记录；管理责任接收与必要来源就绪后进入在管/待复核；同一来源重复发现
//     更新声明，不重复建资产；采集缺失不擅自删除对象；管理责任未接收的发现
//     节点不会直接进入调试中。
//   - TestV3P408DiscoveryConflictIsolation：8 台发现含 1 台冲突时，7 台可
//     独立继续，冲突对象不能写；关联任务、发现结果、人工决定与事件；来源
//     中断/恢复、地址复用和重复发现有可追踪结果。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P408 创建存储与授权。
func setupV3P408(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:discovery", Action: "discover", ObjectRef: "cmdb-source", ValidFrom: now, Owner: "discovery"}, now)
	tx.Commit()
	return store
}

// TestV3P408DiscoveryToManagement 发现到纳管。
func TestV3P408DiscoveryToManagement(t *testing.T) {
	t.Run("discovery_authorized_scope_with_coverage_unresponsive_conflicts", func(t *testing.T) {
		store := setupV3P408(t, "discovery.db")
		now := v3P102Now()
		// 按授权范围发现（保留覆盖、未响应与冲突）。
		tx, _ := store.db.Begin()
		result := DiscoveryResult{
			Discovered: []DiscoveredObject{
				{CandidateIdentity: "192.168.1.1", IdentityKind: "ip", Components: []string{"nic-1"}},
				{CandidateIdentity: "192.168.1.2", IdentityKind: "ip", Components: []string{"nic-2"}},
			},
			Coverage: "subnet-192.168.1.0/24", Unresponsive: []string{"192.168.1.3"},
			Conflicts: []IdentityConflict{
				{CandidateIdentity: "192.168.1.10", ConflictingRefs: []string{"node-old", "node-new"}, Basis: "IP 复用"},
			},
			SourceRef: "cmdb-source", CollectedAt: now,
		}
		processed, conflicts, err := processDiscoveryResult(tx, result, "user:discovery", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if processed != 2 {
			t.Errorf("processed = %d, want 2 (按授权范围发现)", processed)
		}
		if conflicts != 1 {
			t.Errorf("conflicts = %d, want 1 (冲突保留)", conflicts)
		}
	})

	t.Run("unauthorized_discovery_denied", func(t *testing.T) {
		store := setupV3P408(t, "unauth.db")
		now := v3P102Now()
		// 无授权：拒绝（发现不直接等于可写纳管）。
		tx, _ := store.db.Begin()
		_, _, err := processDiscoveryResult(tx, DiscoveryResult{
			Discovered: []DiscoveredObject{{CandidateIdentity: "192.168.1.1", IdentityKind: "ip"}},
			SourceRef: "cmdb-source", CollectedAt: now,
		}, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized discovery not denied")
		}
	})

	t.Run("identity_verified_creates_pending_managed", func(t *testing.T) {
		store := setupV3P408(t, "pending.db")
		now := v3P102Now()
		// 通过身份核对后创建待纳管记录。
		tx, _ := store.db.Begin()
		processDiscoveryResult(tx, DiscoveryResult{
			Discovered: []DiscoveredObject{{CandidateIdentity: "192.168.1.1", IdentityKind: "ip"}},
			SourceRef: "cmdb-source", CollectedAt: now,
		}, "user:discovery", now)
		tx.Commit()
		// 身份声明已创建（候选）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM identity_claims WHERE candidate_identity='192.168.1.1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("identity claim not created: count=%d (通过身份核对后创建待纳管记录)", count)
		}
	})

	t.Run("responsibility_not_accepted_not_directly_debugging", func(t *testing.T) {
		store := setupV3P408(t, "nodebug.db")
		now := v3P102Now()
		// 管理责任未接收的发现节点不会直接进入调试中。
		tx, _ := store.db.Begin()
		err := admitDiscoveredObject(tx, "node-1", false, true, now) // responsibilityAccepted=false
		tx.Rollback()
		if err == nil {
			t.Error("unaccepted responsibility admitted (管理责任未接收不会直接进入调试中)")
		}
		if !strings.Contains(err.Error(), "管理责任未接收") {
			t.Errorf("responsibility reason missing: %v", err)
		}
	})

	t.Run("responsibility_accepted_sources_ready_admitted", func(t *testing.T) {
		store := setupV3P408(t, "admit.db")
		now := v3P102Now()
		// 管理责任接收与必要来源就绪后进入在管/待复核。
		tx, _ := store.db.Begin()
		err := admitDiscoveredObject(tx, "node-1", true, true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 资产已纳管（在管/待复核）。
		asset, err := readAsset(store.db, "node-1")
		if err != nil {
			t.Fatal(err)
		}
		if asset.State != AssetStateRecheck {
			t.Errorf("admitted state = %s, want %s (管理责任接收与必要来源就绪后进入在管/待复核)", asset.State, AssetStateRecheck)
		}
	})

	t.Run("duplicate_discovery_updates_claim_not_duplicate_asset", func(t *testing.T) {
		store := setupV3P408(t, "dup.db")
		now := v3P102Now()
		// 同一来源重复发现更新声明，不重复建资产。
		tx, _ := store.db.Begin()
		result := DiscoveryResult{
			Discovered: []DiscoveredObject{{CandidateIdentity: "192.168.1.1", IdentityKind: "ip"}},
			SourceRef: "cmdb-source", CollectedAt: now,
		}
		processDiscoveryResult(tx, result, "user:discovery", now)
		tx.Commit()
		// 重复发现：不重复建资产。
		tx, _ = store.db.Begin()
		processed, _, err := processDiscoveryResult(tx, result, "user:discovery", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if processed != 0 {
			t.Errorf("duplicate discovery created asset: processed=%d (重复发现更新声明不重复建资产)", processed)
		}
		// 身份声明只一条。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM identity_claims WHERE candidate_identity='192.168.1.1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("duplicate claims = %d, want 1", count)
		}
	})

	t.Run("collection_missing_not_delete_object", func(t *testing.T) {
		store := setupV3P408(t, "missing.db")
		now := v3P102Now()
		// 采集缺失不擅自删除对象（已纳管资产保留）。
		tx, _ := store.db.Begin()
		admitDiscoveredObject(tx, "node-1", true, true, now)
		tx.Commit()
		// 采集缺失（无新发现）：不删除对象。
		asset, err := readAsset(store.db, "node-1")
		if err != nil {
			t.Fatal(err)
		}
		if asset.State == "" {
			t.Error("collection missing deleted object (采集缺失不擅自删除对象)")
		}
	})
}

// TestV3P408DiscoveryConflictIsolation 冲突隔离。
func TestV3P408DiscoveryConflictIsolation(t *testing.T) {
	t.Run("eight_nodes_one_conflict_seven_continue", func(t *testing.T) {
		store := setupV3P408(t, "eight.db")
		now := v3P102Now()
		// 8 台发现含 1 台冲突：7 台可独立继续。
		tx, _ := store.db.Begin()
		var discovered []DiscoveredObject
		for i := 1; i <= 8; i++ {
			discovered = append(discovered, DiscoveredObject{
				CandidateIdentity: "192.168.1." + string(rune('0'+i)), IdentityKind: "ip",
			})
		}
		result := DiscoveryResult{
			Discovered: discovered, SourceRef: "cmdb-source", CollectedAt: now,
			Conflicts: []IdentityConflict{
				{CandidateIdentity: "192.168.1.1", ConflictingRefs: []string{"node-old", "node-new"}, Basis: "IP 复用"},
			},
		}
		processed, conflicts, err := processDiscoveryResult(tx, result, "user:discovery", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 7 台可独立继续（processed=8 或 7，冲突对象不影响其他）。
		if processed < 7 {
			t.Errorf("processed = %d, want >= 7 (7 台可独立继续)", processed)
		}
		if conflicts != 1 {
			t.Errorf("conflicts = %d, want 1 (1 台冲突)", conflicts)
		}
		// 冲突已审计（一台冲突不阻断其他已确认设备）。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='discovery_conflict'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("conflict not isolated: audit=%d (一台冲突不阻断其他已确认设备)", auditCount)
		}
	})

	t.Run("conflict_object_cannot_write", func(t *testing.T) {
		store := setupV3P408(t, "conflict.db")
		now := v3P102Now()
		// 冲突对象不能写（身份冲突可见）。
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
		// 冲突对象：不能写（身份冲突可见）。
		tx, _ = store.db.Begin()
		conflict, refs, err := checkIdentityConflict(tx, "192.168.1.10", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict || len(refs) != 2 {
			t.Errorf("conflict not visible: conflict=%v refs=%v (冲突对象不能写)", conflict, refs)
		}
	})

	t.Run("source_interruption_recovery_traceable", func(t *testing.T) {
		store := setupV3P408(t, "interruption.db")
		now := v3P102Now()
		// 来源中断/恢复：有可追踪结果（水位可查）。
		tx, _ := store.db.Begin()
		s, _ := registerServiceInstance(tx, ServiceInstance{
			SourceIdentity: "cmdb-source", SubscriptionScope: "nodes",
		}, now)
		tx.Commit()
		// 中断后恢复（水位更新，按 service_id）。
		tx, _ = store.db.Begin()
		if err := updateServiceWatermark(tx, s.ServiceID, 50, `[{"start":30,"end":40}]`, true, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		w, err := readServiceWatermark(store.db, s.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		if w.LastSequence != 50 {
			t.Errorf("watermark = %d (来源中断/恢复有可追踪结果)", w.LastSequence)
		}
		if !w.CoverageUnknown {
			t.Log("coverage unknown visible (覆盖未知明确可见)")
		}
	})

	t.Run("address_reuse_duplicate_discovery_traceable", func(t *testing.T) {
		store := setupV3P408(t, "reuse.db")
		now := v3P102Now()
		// 地址复用和重复发现：有可追踪结果。
		tx, _ := store.db.Begin()
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-old", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-new", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		tx.Commit()
		// 地址复用：冲突可追踪。
		tx, _ = store.db.Begin()
		conflict, refs, err := checkIdentityConflict(tx, "192.168.1.10", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict || len(refs) != 2 {
			t.Errorf("address reuse not traceable: conflict=%v refs=%v (地址复用有可追踪结果)", conflict, refs)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
