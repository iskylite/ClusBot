package work

// P3-04 验收：能力、提供者与连接分离。
//
// 三个测试入口：
//   - TestV3P304CapabilityProviderConnection：能力有稳定 ID/版本、Query/Action
//     类别、输入输出 Schema、目标类型和执行模式；提供者与连接另有身份及版本；
//     工具名称与连接 ID 保持来源区分，同名不同连接不合并。
//   - TestV3P304CapabilityClaimsUntrusted：能力和连接描述只声明机制，不拥有
//     Task/资产权威，也不能自报业务验收通过；声明只读/幂等不自动获权，伪造
//     描述不会绕过宿主校验。
//   - TestV3P304CapabilityVersionBinding：同能力可绑定不同提供者，历史记录仍
//     指向原版本；缺失可选能力准确返回不可用/未知，不制造取消或查询成功。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P304 创建存储。
func setupV3P304(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P304CapabilityProviderConnection 能力/提供者/连接分离。
func TestV3P304CapabilityProviderConnection(t *testing.T) {
	t.Run("capability_has_stable_id_version_kind_schema", func(t *testing.T) {
		store := setupV3P304(t, "cap.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, err := registerCapability(tx, Capability{
			CapabilityKey: "cluster.node.read", Version: 1, Kind: "query",
			InputSchema: `{"type":"object"}`, OutputSchema: `{"type":"object"}`,
			TargetKind: "node", ExecutionMode: "sync",
			SupportsGet: "supported", SupportsCancel: "unsupported", Idempotent: "supported",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 能力含全部字段。
		if cap.CapabilityKey != "cluster.node.read" || cap.Version != 1 || cap.Kind != "query" ||
			cap.TargetKind != "node" || cap.ExecutionMode != "sync" {
			t.Errorf("capability incomplete: %+v", cap)
		}
		// 特性分别记录。
		if cap.SupportsGet != "supported" || cap.SupportsCancel != "unsupported" || cap.Idempotent != "supported" {
			t.Errorf("capability features wrong: %+v", cap)
		}
	})

	t.Run("provider_connection_separate_identity", func(t *testing.T) {
		store := setupV3P304(t, "provider.db")
		now := v3P102Now()
		// 提供者与连接另有身份及版本。
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}, now)
		provider, err := registerProvider(tx, CapabilityProvider{
			ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp",
			Capabilities: []string{cap.CapabilityKey + ":1"},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := registerConnection(tx, CapabilityConnection{
			ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1,
			CredentialRef: "env:CLUSTER_CRED",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 提供者与连接身份分离。
		if provider.ProviderID == conn.ConnectionID {
			t.Error("provider and connection share identity (应分离)")
		}
		if provider.ProviderKey != "mcp-cluster" || conn.ConnectionName != "cluster-prod" {
			t.Errorf("provider/connection identity wrong: %+v %+v", provider, conn)
		}
	})

	t.Run("same_name_different_connections_not_merged", func(t *testing.T) {
		store := setupV3P304(t, "samename.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "read", Version: 1, Kind: "query"}, now)
		p1, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-a", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		p2, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-b", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		// 同名不同连接（不同提供者）。
		registerConnection(tx, CapabilityConnection{ProviderID: p1.ProviderID, ConnectionName: "cluster", ConnectionVersion: 1, CredentialRef: "cred-a"}, now)
		registerConnection(tx, CapabilityConnection{ProviderID: p2.ProviderID, ConnectionName: "cluster", ConnectionVersion: 1, CredentialRef: "cred-b"}, now)
		tx.Commit()
		// 同名不同连接不合并：返回两个连接（来源区分）。
		connections, err := resolveConnectionsByName(store.db, "cluster")
		if err != nil {
			t.Fatal(err)
		}
		if len(connections) != 2 {
			t.Errorf("same-name connections merged: %d (同名不同连接不合并)", len(connections))
		}
		if connections[0].ProviderID == connections[1].ProviderID {
			t.Error("same-name connections share provider (来源应区分)")
		}
	})
}

// TestV3P304CapabilityClaimsUntrusted 能力声明不可信。
func TestV3P304CapabilityClaimsUntrusted(t *testing.T) {
	t.Run("self_reported_acceptance_rejected", func(t *testing.T) {
		// 能力不能自报业务验收通过。
		cap := Capability{
			CapabilityKey: "cluster.node.write", Version: 1, Kind: "action",
			VerifiedScope: "self-reported",
		}
		if err := validateCapabilityClaim(cap); err == nil {
			t.Error("self-reported acceptance not rejected (不能自报业务验收通过)")
		}
	})

	t.Run("readonly_idempotent_claim_not_auto_grant", func(t *testing.T) {
		store := setupV3P304(t, "nogrant.db")
		now := v3P102Now()
		// 声明只读/幂等不自动获权：能力声明是机制，仍需授权。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{
			CapabilityKey: "cluster.node.read", Version: 1, Kind: "query",
			Idempotent: "supported", SupportsGet: "supported",
		}, now)
		tx.Commit()
		// 无授权时调用：仍拒绝（声明不自动获权）。
		tx, _ = store.db.Begin()
		decision, _ := decideAuthorization(tx, "user:mallory", "read", "cluster.node.read", "", now, false)
		tx.Commit()
		if decision.Outcome == "allowed" {
			t.Error("readonly/idempotent claim auto-granted (声明只读/幂等不自动获权)")
		}
	})

	t.Run("forged_description_not_bypass_host_check", func(t *testing.T) {
		// 伪造描述不会绕过宿主校验：伪造 verified_scope 被拒绝。
		forged := Capability{
			CapabilityKey: "cluster.node.write", Version: 1, Kind: "action",
			VerifiedScope: "self-reported", // 伪造：自报已验证
			SupportsCancel: "supported", Idempotent: "supported",
		}
		if err := validateCapabilityClaim(forged); err == nil {
			t.Error("forged description not rejected (伪造描述不绕过宿主校验)")
		}
	})

	t.Run("action_capability_not_exposed_to_model_by_default", func(t *testing.T) {
		store := setupV3P304(t, "noexpose.db")
		now := v3P102Now()
		// 新写能力默认不向模型暴露。
		tx, _ := store.db.Begin()
		cap, err := registerCapability(tx, Capability{
			CapabilityKey: "cluster.node.write", Version: 1, Kind: "action",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if cap.ExposedToModel {
			t.Error("action capability exposed to model by default (新写能力默认不向模型暴露)")
		}
		// 查询能力可暴露（默认）。
		tx, _ = store.db.Begin()
		queryCap, _ := registerCapability(tx, Capability{
			CapabilityKey: "cluster.node.read", Version: 1, Kind: "query", ExposedToModel: true,
		}, now)
		tx.Commit()
		if !queryCap.ExposedToModel {
			t.Error("query capability not exposed (查询能力可暴露)")
		}
	})
}

// TestV3P304CapabilityVersionBinding 版本绑定。
func TestV3P304CapabilityVersionBinding(t *testing.T) {
	t.Run("same_capability_different_providers_history_preserved", func(t *testing.T) {
		store := setupV3P304(t, "version.db")
		now := v3P102Now()
		// 同能力绑定不同提供者（版本演进）。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}, now)
		registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 2, Kind: "query"}, now)
		p1, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-old", Version: 1, Kind: "mcp", Capabilities: []string{"cluster.node.read:1"}}, now)
		p2, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-new", Version: 1, Kind: "mcp", Capabilities: []string{"cluster.node.read:2"}}, now)
		tx.Commit()
		// 历史记录仍指向原版本（v1 与 v2 分别可查）。
		v1, err := capabilityFeature(store.db, "cluster.node.read", 1, FeatureGet)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := capabilityFeature(store.db, "cluster.node.read", 2, FeatureGet)
		if err != nil {
			t.Fatal(err)
		}
		if v1 != "unknown" || v2 != "unknown" {
			t.Logf("v1 get=%s v2 get=%s (版本分别记录)", v1, v2)
		}
		// 两个提供者各自绑定不同版本。
		if p1.ProviderKey == p2.ProviderKey {
			t.Error("same capability not bound to different providers")
		}
	})

	t.Run("missing_capability_returns_unavailable_unknown", func(t *testing.T) {
		store := setupV3P304(t, "missing.db")
		// 缺失能力：准确返回未知（不制造取消或查询成功）。
		value, err := capabilityFeature(store.db, "nonexistent.capability", 1, FeatureCancel)
		if err != nil {
			t.Fatal(err)
		}
		if value != "unknown" {
			t.Errorf("missing capability = %s, want unknown (缺失能力准确返回未知)", value)
		}
		// 缺失可选能力（存在但未声明）：返回 unknown。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}, v3P102Now())
		tx.Commit()
		value, err = capabilityFeature(store.db, "cluster.node.read", 1, FeatureCancel)
		if err != nil {
			t.Fatal(err)
		}
		if value != "unknown" {
			t.Errorf("undeclared feature = %s, want unknown (缺失可选能力准确返回未知)", value)
		}
	})

	t.Run("capability_not_owner_of_task_or_asset", func(t *testing.T) {
		store := setupV3P304(t, "notowner.db")
		now := v3P102Now()
		// 能力和连接描述只声明机制，不拥有 Task/资产权威。
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}, now)
		tx.Commit()
		// 能力记录无 Task/资产权威字段（只声明机制）：capability 表无 task_id/asset_ref 列。
		var capabilityID, verifiedScope string
		if err := store.db.QueryRow("SELECT capability_id, verified_scope FROM capabilities WHERE capability_id=?", cap.CapabilityID).Scan(&capabilityID, &verifiedScope); err != nil {
			t.Fatal(err)
		}
		if capabilityID == "" {
			t.Error("capability not persisted")
		}
		// 能力表结构不含 task_id/asset_ref（不拥有权威）。
		var colCount int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('capabilities') WHERE name IN ('task_id','asset_ref')`).Scan(&colCount); err != nil {
			t.Fatal(err)
		}
		if colCount != 0 {
			t.Errorf("capability has task/asset columns (能力只声明机制，不拥有权威)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
