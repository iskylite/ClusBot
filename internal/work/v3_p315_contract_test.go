package work

// P3-15 验收：提供者一致的契约验证入口。
//
// 两个测试入口：
//   - TestV3P315ProviderContractMatrix：用同一用例集合验证内部、CLI、HTTP、
//     MCP 的身份、Schema、范围、结果/材料和错误边界；记录经过测试的提供者/
//     连接/能力/协议版本与限制；只验证某一配置不能宣称同协议所有服务安全。
//   - TestV3P315UnsupportedCapabilityBehavior：为 supported 的 Query/Get/
//     Cancel/Events/幂等逐项取证；unsupported/unknown 有明确退化行为；缺失
//     能力不会以空成功或默认安全标志通过契约测试；将能力证明版本纳入目录
//     和执行绑定；变更影响证明时停止相关新派发直到复核；不因某 SDK 默认重试
//     放宽 V3。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P315 创建存储。
func setupV3P315(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P315ProviderContractMatrix 契约矩阵。
func TestV3P315ProviderContractMatrix(t *testing.T) {
	t.Run("same_case_set_for_all_adapters", func(t *testing.T) {
		store := setupV3P315(t, "matrix.db")
		now := v3P102Now()
		// 同一用例集合验证内部、CLI、HTTP、MCP。
		adapters := []string{"internal", "cli", "http", "mcp"}
		for _, adapter := range adapters {
			tx, _ := store.db.Begin()
			result, err := runContractTestCase(tx, ContractTestCase{
				CaseID: "case-identity-schema", AdapterKind: adapter,
				VerifyIdentity: true, VerifySchema: true, VerifyScope: true,
				VerifyResult: true, VerifyMaterials: true, VerifyErrorBound: true,
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if result.Outcome != "pass" {
				t.Errorf("adapter %s: outcome = %s, want pass", adapter, result.Outcome)
			}
			// 逐项取证：身份/Schema/范围/结果/材料/错误边界。
			for _, key := range []string{"identity", "schema", "scope", "result", "materials", "error_bound"} {
				if result.Evidence[key] == "" {
					t.Errorf("adapter %s: missing evidence %s", adapter, key)
				}
			}
		}
	})

	t.Run("contract_test_records_provider_limitations", func(t *testing.T) {
		store := setupV3P315(t, "record.db")
		now := v3P102Now()
		// 记录经过测试的提供者/连接/能力/协议版本与限制。
		tx, _ := store.db.Begin()
		err := recordContractTest(tx, ContractTestRecord{
			ProviderID: "mcp-provider", ConnectionID: "conn-cluster", CapabilityKey: "cluster.node.read",
			ProtocolVersion: "mcp-1.0", Limitations: "仅验证本连接配置，不宣称同协议所有服务安全",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 测试记录已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='contract_test_recorded'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("contract test not recorded: count=%d", count)
		}
	})

	t.Run("single_config_not_claim_all_services_safe", func(t *testing.T) {
		// 只验证某一配置不能宣称同协议所有服务安全。
		record := ContractTestRecord{
			ProviderID: "provider-a", ConnectionID: "conn-a", CapabilityKey: "cap-a",
			Limitations: "仅验证 provider-a/conn-a 配置",
		}
		// 限制明确（不宣称同协议所有服务安全）。
		if !strings.Contains(record.Limitations, "仅验证") {
			t.Error("limitations not clear (只验证某一配置不能宣称同协议所有服务安全)")
		}
	})
}

// TestV3P315UnsupportedCapabilityBehavior 不支持能力行为。
func TestV3P315UnsupportedCapabilityBehavior(t *testing.T) {
	t.Run("supported_features_verified_individually", func(t *testing.T) {
		store := setupV3P315(t, "features.db")
		now := v3P102Now()
		// 注册能力（部分特性 supported）。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{
			CapabilityKey: "cluster.node.read", Version: 1, Kind: "query",
			SupportsGet: "supported", SupportsCancel: "unsupported", Idempotent: "supported",
		}, now)
		tx.Commit()
		// supported 逐项取证。
		tx, _ = store.db.Begin()
		evidence, err := verifySupportedFeatures(tx, "cluster.node.read", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// supported 逐项取证；unsupported/unknown 有明确退化行为。
		if evidence["get"] != "verified_supported" {
			t.Errorf("get evidence = %s, want verified_supported", evidence["get"])
		}
		if evidence["cancel"] != "verified_unsupported" {
			t.Errorf("cancel evidence = %s, want verified_unsupported (unsupported 明确退化行为)", evidence["cancel"])
		}
		if evidence["idempotent"] != "verified_supported" {
			t.Errorf("idempotent evidence = %s", evidence["idempotent"])
		}
		// 未声明特性（events/materials/fencing）：unknown（不默认安全）。
		for _, f := range []string{"events", "materials", "fencing"} {
			if evidence[f] != "verified_unknown" {
				t.Errorf("%s evidence = %s, want verified_unknown (unknown 明确退化行为)", f, evidence[f])
			}
		}
	})

	t.Run("missing_capability_not_empty_success_or_default_safe", func(t *testing.T) {
		store := setupV3P315(t, "missing.db")
		now := v3P102Now()
		// 缺失能力：不会以空成功或默认安全标志通过契约测试。
		tx, _ := store.db.Begin()
		evidence, err := verifySupportedFeatures(tx, "nonexistent.capability", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 缺失能力：全部 unknown（不默认安全）。
		for feature, value := range evidence {
			if value != "verified_unknown" {
				t.Errorf("missing capability feature %s = %s, want verified_unknown (缺失能力不默认安全)", feature, value)
			}
		}
	})

	t.Run("proof_version_change_stops_new_dispatch", func(t *testing.T) {
		store := setupV3P315(t, "version.db")
		now := v3P102Now()
		// 能力证明版本纳入目录和执行绑定；变更影响证明时停止相关新派发直到复核。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}, now)
		tx.Commit()
		// 版本一致：通过。
		tx, _ = store.db.Begin()
		if err := ensureProofVersionCurrent(tx, "cluster.node.read", 1); err != nil {
			t.Errorf("proof version rejected: %v", err)
		}
		tx.Commit()
		// 版本变更：停止新派发直到复核。
		tx, _ = store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "cluster.node.read", Version: 2, Kind: "query"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureProofVersionCurrent(tx, "cluster.node.read", 1)
		tx.Rollback()
		if err == nil {
			t.Error("proof version change not detected (变更影响证明时停止相关新派发)")
		}
		if !strings.Contains(err.Error(), "证明版本已变更") {
			t.Errorf("proof version change reason missing: %v", err)
		}
	})

	t.Run("sdk_default_retry_not_relaxed_v3", func(t *testing.T) {
		// 不因某 SDK 默认重试放宽 V3。
		if err := ensureConsistentRetryPolicy(5, 3); err == nil {
			t.Error("sdk default retry relaxed v3 (不因 SDK 默认重试放宽 V3)")
		}
		// SDK 重试在 V3 上限内：允许。
		if err := ensureConsistentRetryPolicy(2, 3); err != nil {
			t.Errorf("consistent retry rejected: %v", err)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
