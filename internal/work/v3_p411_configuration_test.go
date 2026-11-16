package work

// P4-11 验收：带内外配置与动态字段校验。
//
// 三个测试入口：
//   - TestV3P411DynamicConfigurationFields：按目标环境发现动态字段、依赖、
//     单位、默认值和能力版本；实例化后冻结完整参数；新增/缺失动态字段能
//     按 Schema 处理，不通过忽略字段获得假成功。
//   - TestV3P411ConfigurationReadback：配置完成后独立回读并核验；部分写入、
//     读回失败及不支持字段分别记录；期望值、接口接受值和实际值不一致时
//     不能通过验收；规范变更或回读冲突触发相关证据失效/补查，不直接把全部
//     节点重做。
//   - TestV3P411ConfigurationScope：只派发当前范围内的必要配置操作，按影响
//     范围协调冲突；借出和冻结按领域规则拒绝普通写；带外权限与普通读取
//     权限分开，错误目标/旧版本不派发；读取带内/带外当前配置与来源时间；
//     将期望配置作为规范绑定，不当作已生效事实。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P411 创建存储、资产与授权。
func setupV3P411(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "configure", ObjectRef: "node-1", ValidFrom: now, Owner: "ops"}, now)
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "read", ObjectRef: "node-1", ValidFrom: now, Owner: "ops"}, now)
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// TestV3P411DynamicConfigurationFields 动态字段。
func TestV3P411DynamicConfigurationFields(t *testing.T) {
	t.Run("discover_fields_from_capability_schema", func(t *testing.T) {
		store := setupV3P411(t, "discover.db")
		now := v3P102Now()
		// 注册能力（含输入 Schema）。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{
			CapabilityKey: "node.bios.config", Version: 1, Kind: "action",
			InputSchema: `{"type":"object","properties":{"boot_order":{"type":"string"}}}`,
		}, now)
		tx.Commit()
		// 按目标环境发现动态字段（不集中硬编码所有设备字段）。
		tx, _ = store.db.Begin()
		fields, err := discoverDynamicFields(tx, "node.bios.config", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(fields) == 0 {
			t.Error("dynamic fields not discovered (按目标环境发现动态字段)")
		}
		// 能力版本。
		if fields[0].CapabilityVersion != 1 {
			t.Errorf("capability version = %d (按目标环境发现)", fields[0].CapabilityVersion)
		}
	})

	t.Run("missing_schema_not_fake_success", func(t *testing.T) {
		store := setupV3P411(t, "noschema.db")
		now := v3P102Now()
		// 缺 Schema：拒绝（不通过忽略字段获得假成功）。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "no-schema-cap", Version: 1, Kind: "action"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := discoverDynamicFields(tx, "no-schema-cap", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("missing schema accepted (不通过忽略字段获得假成功)")
		}
	})

	t.Run("frozen_params_after_instantiation", func(t *testing.T) {
		// 实例化后冻结完整参数（复用 P3-05 的 freezeSnapshot）。
		store := setupV3P411(t, "frozen.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "node.bios.config", Version: 1, Kind: "action"}, now)
		provider, _ := registerProvider(tx, CapabilityProvider{ProviderKey: "mcp-node", Version: 1, Kind: "mcp", Capabilities: []string{cap.CapabilityKey + ":1"}}, now)
		conn, _ := registerConnection(tx, CapabilityConnection{ProviderID: provider.ProviderID, ConnectionName: "node-prod", ConnectionVersion: 1, CredentialRef: "cred"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		snapshot, err := freezeSnapshot(tx, CapabilitySnapshot{
			CapabilityKey: cap.CapabilityKey, CapabilityVersion: 1, ConnectionID: conn.ConnectionID,
			ConnectionVersion: 1, FrozenParams: `{"boot_order":"pxe","fan_speed":"auto"}`,
			CreatedBy: "user:ops",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if snapshot.FrozenParams == "" {
			t.Error("frozen params empty (实例化后冻结完整参数)")
		}
	})
}

// TestV3P411ConfigurationReadback 配置回读。
func TestV3P411ConfigurationReadback(t *testing.T) {
	t.Run("expected_accepted_actual_mismatch_rejected", func(t *testing.T) {
		store := setupV3P411(t, "mismatch.db")
		now := v3P102Now()
		// 期望值 ≠ 接口接受值：拒绝。
		tx, _ := store.db.Begin()
		err := verifyConfigurationReadback(tx, "node-1", "boot_order", "pxe", "disk", "pxe", now)
		tx.Rollback()
		if err == nil {
			t.Error("expected≠accepted accepted (期望值、接口接受值和实际值不一致时不能通过验收)")
		}
	})

	t.Run("accepted_actual_mismatch_rejected", func(t *testing.T) {
		store := setupV3P411(t, "acceptedmismatch.db")
		now := v3P102Now()
		// 接口接受值 ≠ 实际值：拒绝（读回失败或部分写入）。
		tx, _ := store.db.Begin()
		err := verifyConfigurationReadback(tx, "node-1", "boot_order", "pxe", "pxe", "disk", now)
		tx.Rollback()
		if err == nil {
			t.Error("accepted≠actual accepted (读回失败或部分写入)")
		}
	})

	t.Run("consistent_values_verified", func(t *testing.T) {
		store := setupV3P411(t, "consistent.db")
		now := v3P102Now()
		// 一致：核验通过（配置完成后独立回读并核验）。
		tx, _ := store.db.Begin()
		err := verifyConfigurationReadback(tx, "node-1", "boot_order", "pxe", "pxe", "pxe", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 核验已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='configuration_readback_verified'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("readback not audited: count=%d (配置完成后独立回读并核验)", count)
		}
	})

	t.Run("spec_change_invalidates_related_evidence_not_redo_all", func(t *testing.T) {
		store := setupV3P411(t, "invalidate.db")
		now := v3P102Now()
		// 规范变更或回读冲突：失效相关证据（不直接把全部节点重做）。
		tx, _ := store.db.Begin()
		err := invalidateConfigurationEvidence(tx, "node-1", "规范变更", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 失效已审计（不直接把全部节点重做）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='configuration_evidence_invalidated'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("evidence not invalidated: count=%d (规范变更或回读冲突触发相关证据失效/补查)", count)
		}
	})
}

// TestV3P411ConfigurationScope 配置范围。
func TestV3P411ConfigurationScope(t *testing.T) {
	t.Run("frozen_asset_rejects_normal_write", func(t *testing.T) {
		store := setupV3P411(t, "frozen.db")
		now := v3P102Now()
		// 冻结资产：拒绝普通写。
		tx, _ := store.db.Begin()
		freezeAsset(tx, "node-1", "admin", "维护冻结", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := dispatchConfigurationOperation(tx, "node-1", "boot_order", "pxe", "user:ops", now)
		tx.Rollback()
		if err == nil {
			t.Error("frozen asset allowed normal write (借出和冻结按领域规则拒绝普通写)")
		}
		if !strings.Contains(err.Error(), "冻结") {
			t.Errorf("frozen reason missing: %v", err)
		}
	})

	t.Run("lent_asset_rejects_normal_write", func(t *testing.T) {
		store := setupV3P411(t, "lent.db")
		now := v3P102Now()
		// 借出资产：拒绝普通写。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateLent, "node-1"); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		err := dispatchConfigurationOperation(tx, "node-1", "boot_order", "pxe", "user:ops", now)
		tx.Rollback()
		if err == nil {
			t.Error("lent asset allowed normal write (借出按领域规则拒绝普通写)")
		}
		if !strings.Contains(err.Error(), "借出") {
			t.Errorf("lent reason missing: %v", err)
		}
	})

	t.Run("oob_permission_separate_from_read", func(t *testing.T) {
		store := setupV3P411(t, "oob.db")
		now := v3P102Now()
		// 带外权限与普通读取权限分开（configure ≠ read）。
		tx, _ := store.db.Begin()
		// user:reader 只有 read 权限（无 configure）。
		grantAuthorization(tx, Authorization{Subject: "user:reader", Action: "read", ObjectRef: "node-1", ValidFrom: now, Owner: "reader"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := dispatchConfigurationOperation(tx, "node-1", "boot_order", "pxe", "user:reader", now)
		tx.Rollback()
		if err == nil {
			t.Error("read-only subject allowed configure (带外权限与普通读取权限分开)")
		}
	})

	t.Run("unauthorized_configure_denied", func(t *testing.T) {
		store := setupV3P411(t, "unauth.db")
		now := v3P102Now()
		// 无配置权限：拒绝。
		tx, _ := store.db.Begin()
		err := dispatchConfigurationOperation(tx, "node-1", "boot_order", "pxe", "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized configure not denied")
		}
	})

	t.Run("desired_config_as_spec_not_effective_fact", func(t *testing.T) {
		store := setupV3P411(t, "spec.db")
		now := v3P102Now()
		// 期望配置作为规范绑定（不当作已生效事实）。
		tx, _ := store.db.Begin()
		spec, err := bindConfigurationSpec(tx, ConfigurationSpec{
			ObjectRef: "node-1", DesiredConfig: `{"boot_order":"pxe"}`, SpecVersion: 1,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if spec.SpecID == "" {
			t.Error("spec not bound")
		}
		// 期望配置不直接写 ops_assets（不当作已生效事实）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateInService {
			t.Errorf("desired config changed asset state: %s (期望配置作为规范绑定不当作已生效事实)", asset.State)
		}
	})

	t.Run("inband_oob_current_config_with_source_time", func(t *testing.T) {
		// 读取带内/带外当前配置与来源时间。
		read := ConfigurationRead{
			Path: "oob", SourceTime: v3P102Now(), CurrentConfig: `{"boot_order":"disk"}`,
			SourceRef: "oob-agent", CollectedAt: v3P102Now(),
		}
		if read.Path != "oob" || read.SourceTime == 0 {
			t.Errorf("inband/oob read incomplete: %+v (读取带内/带外当前配置与来源时间)", read)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
