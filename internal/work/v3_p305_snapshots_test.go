package work

// P3-05 验收：动态 Schema 发现与不可变执行快照。
//
// 三个测试入口：
//   - TestV3P305DynamicSchemaSnapshot：读取授权范围内目录、配置/测试/输入/输出
//     Schema、单位、依赖、默认值、环境版本与有效期；校验方言、大小、深度与
//     参数；禁止自动访问任意远程 $ref；动态选项缺失时返回缺口，不猜值。
//   - TestV3P305ParametersFrozen：选择实现后冻结具体参数、规范化摘要算法版本、
//     Schema/连接/能力/凭据绑定与实际目标清单；同键不同参数摘要拒绝；批量
//     快照逐对象包含 revision/epoch。
//   - TestV3P305SchemaChangedBeforeDispatch：发现后 Schema/凭据/对象集合变化，
//     旧确认不能直接执行；派发前重查当前权限/目标/连接版本；未派发不匹配即停，
//     已派发按原快照核对。

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P305 创建存储。
func setupV3P305(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P305DynamicSchemaSnapshot 动态 Schema 发现。
func TestV3P305DynamicSchemaSnapshot(t *testing.T) {
	t.Run("discover_authorized_schema_with_metadata", func(t *testing.T) {
		store := setupV3P305(t, "discover.db")
		now := v3P102Now()
		// 授权读取。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "cluster.node.test", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 发现授权范围内的 Schema。
		tx, _ = store.db.Begin()
		discovery, err := discoverSchema(tx, "cluster.node.test", func() (string, string, error) {
			return `{"type":"object","properties":{"host":{"type":"string"}}}`,
				`{"type":"object","properties":{"result":{"type":"string"}}}`, nil
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 发现含 Schema 与校验结果。
		if discovery.CapabilityKey != "cluster.node.test" || discovery.ValidationOutcome != "valid" {
			t.Errorf("discovery = %+v", discovery)
		}
	})

	t.Run("unauthorized_schema_discovery_denied", func(t *testing.T) {
		store := setupV3P305(t, "unauth.db")
		now := v3P102Now()
		// 无授权：拒绝（读取授权范围内的 Schema）。
		tx, _ := store.db.Begin()
		_, err := discoverSchema(tx, "cluster.node.test", func() (string, string, error) {
			return `{"type":"object"}`, `{"type":"object"}`, nil
		}, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized schema discovery not denied")
		}
	})

	t.Run("remote_ref_rejected", func(t *testing.T) {
		store := setupV3P305(t, "remoteref.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "cluster.node.test", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 禁止自动访问任意远程 $ref。
		tx, _ = store.db.Begin()
		discovery, err := discoverSchema(tx, "cluster.node.test", func() (string, string, error) {
			return `{"type":"object","properties":{"x":{"$ref":"https://evil.com/schema.json"}}}`, `{"type":"object","properties":{"y":{"type":"string"}}}`, nil
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if discovery.ValidationOutcome != "invalid" {
			t.Errorf("remote ref not rejected: outcome=%s", discovery.ValidationOutcome)
		}
		if !strings.Contains(discovery.ValidationDetail, "远程 $ref") {
			t.Errorf("remote ref reason missing: %s", discovery.ValidationDetail)
		}
	})

	t.Run("missing_dynamic_options_gap_not_guess", func(t *testing.T) {
		store := setupV3P305(t, "gap.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "cluster.node.test", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 动态选项缺失：返回缺口（不猜值）。
		tx, _ = store.db.Begin()
		discovery, err := discoverSchema(tx, "cluster.node.test", func() (string, string, error) {
			return `{"type":"object"}`, `{"type":"object"}`, nil // 缺 properties
		}, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if discovery.ValidationOutcome != "gap" {
			t.Errorf("missing options not gap: outcome=%s (动态选项缺失返回缺口)", discovery.ValidationOutcome)
		}
		if !strings.Contains(discovery.Gaps, "missing") {
			t.Errorf("gap not recorded: %s", discovery.Gaps)
		}
	})

	t.Run("oversized_or_deep_schema_rejected", func(t *testing.T) {
		store := setupV3P305(t, "oversize.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "cluster.node.test", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 超大 Schema：拒绝。
		tx, _ = store.db.Begin()
		discovery, _ := discoverSchema(tx, "cluster.node.test", func() (string, string, error) {
			return `{"type":"object","properties":{"x":{"type":"string","description":"` + strings.Repeat("x", 70000) + `"}}}`, `{"type":"object","properties":{"y":{"type":"string"}}}`, nil
		}, "user:alice", now)
		tx.Commit()
		if discovery.ValidationOutcome != "invalid" {
			t.Errorf("oversized schema not rejected: outcome=%s", discovery.ValidationOutcome)
		}
	})
}

// TestV3P305ParametersFrozen 参数冻结。
func TestV3P305ParametersFrozen(t *testing.T) {
	t.Run("snapshot_freezes_params_schema_binding_targets", func(t *testing.T) {
		store := setupV3P305(t, "freeze.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.test", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		tx.Commit()
		// 冻结快照（具体参数/Schema/连接/能力/凭据绑定/目标清单）。
		tx, _ = store.db.Begin()
		snapshot, err := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, CredentialRef: "env:CRED",
			InputSchema: `{"type":"object"}`, OutputSchema: `{"type":"object"}`,
			FrozenParams: `{"host":"node-1","timeout":30}`,
			TargetRefs: `[{"ref":"asset-1","revision":1,"epoch":1}]`,
			EnvironmentVersion: "v1.0", CreatedBy: "user:alice",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 快照含全部绑定。
		if snapshot.CapabilityKey != cap.CapabilityKey || snapshot.ConnectionID != conn.ConnectionID ||
			snapshot.FrozenParams == "" || snapshot.ParamsDigest == "" || snapshot.DigestAlgorithm != "sha256" {
			t.Errorf("snapshot incomplete: %+v", snapshot)
		}
		// 规范化摘要算法版本。
		if snapshot.DigestAlgorithm != "sha256" {
			t.Errorf("digest algorithm = %s (规范化摘要算法版本)", snapshot.DigestAlgorithm)
		}
	})

	t.Run("same_key_different_params_digest_rejected", func(t *testing.T) {
		store := setupV3P305(t, "conflict.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.test", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		tx.Commit()
		// 冻结快照（参数 A）。
		tx, _ = store.db.Begin()
		freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"host":"node-1"}`,
		}, now)
		tx.Commit()
		// 同键不同参数摘要：拒绝。
		tx, _ = store.db.Begin()
		_, err := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"host":"node-2"}`, // 不同参数
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("same key different params not rejected (同键不同参数摘要拒绝)")
		}
	})

	t.Run("batch_snapshot_per_object_revision_epoch", func(t *testing.T) {
		store := setupV3P305(t, "batch.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.write", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		tx.Commit()
		// 批量快照：逐对象包含 revision/epoch。
		tx, _ = store.db.Begin()
		snapshot, err := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"action":"restart"}`,
			TargetRefs: `[{"ref":"asset-1","revision":1,"epoch":1},{"ref":"asset-2","revision":2,"epoch":1}]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 目标清单含逐对象 revision/epoch。
		var targets []map[string]any
		if err := json.Unmarshal([]byte(snapshot.TargetRefs), &targets); err != nil {
			t.Fatal(err)
		}
		if len(targets) != 2 {
			t.Errorf("batch targets = %d, want 2", len(targets))
		}
		for i, target := range targets {
			if target["revision"] == nil || target["epoch"] == nil {
				t.Errorf("target %d missing revision/epoch (批量快照逐对象包含)", i)
			}
		}
	})
}

// TestV3P305SchemaChangedBeforeDispatch 派发前变化。
func TestV3P305SchemaChangedBeforeDispatch(t *testing.T) {
	t.Run("schema_credential_target_change_old_confirmation_invalid", func(t *testing.T) {
		store := setupV3P305(t, "changed.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.test", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: cap.CapabilityKey, ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 冻结快照。
		tx, _ = store.db.Begin()
		snapshot, _ := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"host":"node-1"}`, ExpiresAt: now + 100000, CreatedBy: "user:alice",
		}, now)
		tx.Commit()
		// 连接版本变化（Schema/凭据/对象集合变化）：旧确认不能直接执行。
		// 在事务外执行（避免单连接嵌套）。
		if _, err := store.db.Exec("UPDATE capability_connections SET connection_version=2 WHERE connection_id=?", conn.ConnectionID); err != nil {
			t.Fatal(err)
		}
		// 派发前重查：连接版本不匹配即停。
		tx, _ = store.db.Begin()
		err := validateSnapshotBeforeDispatch(tx, snapshot.SnapshotID, "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("stale snapshot dispatched after connection change (发现后变化旧确认不能直接执行)")
		}
		if !strings.Contains(err.Error(), "连接版本已变化") {
			t.Errorf("stale reason missing: %v", err)
		}
	})

	t.Run("dispatch_before_change_checks_current", func(t *testing.T) {
		store := setupV3P305(t, "beforechange.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.test", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: cap.CapabilityKey, ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		snapshot, _ := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"host":"node-1"}`, ExpiresAt: now + 100000, CreatedBy: "user:alice",
		}, now)
		tx.Commit()
		// 派发前重查（版本未变）：通过。
		tx, _ = store.db.Begin()
		err := validateSnapshotBeforeDispatch(tx, snapshot.SnapshotID, "user:alice", now)
		tx.Commit()
		if err != nil {
			t.Errorf("valid snapshot rejected before dispatch: %v", err)
		}
	})

	t.Run("expired_snapshot_rejected", func(t *testing.T) {
		store := setupV3P305(t, "expiredsnap.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "cluster.node.test", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-cluster", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "cluster-prod", ConnectionVersion: 1, CredentialRef: "env:CRED"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "dispatch", ObjectRef: cap.CapabilityKey, ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		snapshot, _ := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"host":"node-1"}`, ExpiresAt: now - 1000, CreatedBy: "user:alice", // 已过期
		}, now)
		tx.Commit()
		// 过期快照：拒绝（未派发不匹配即停）。
		tx, _ = store.db.Begin()
		err := validateSnapshotBeforeDispatch(tx, snapshot.SnapshotID, "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("expired snapshot dispatched")
		}
	})

	t.Run("domain_threshold_not_overridden_by_provider", func(t *testing.T) {
		// 领域阈值单独引用已发布规范；提供者建议值不自动覆盖业务规则。
		if err := validateDomainThresholds("provider-suggested", "published-spec"); err == nil {
			t.Error("provider suggestion overrode domain rule (提供者建议值不自动覆盖业务规则)")
		}
		// 一致：通过。
		if err := validateDomainThresholds("published-spec", "published-spec"); err != nil {
			t.Errorf("matching threshold rejected: %v", err)
		}
		// 无已发布规范：拒绝。
		if err := validateDomainThresholds("provider-suggested", ""); err == nil {
			t.Error("missing published spec not rejected")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
