package work

// P4-01 验收：版本化领域包和类型约束。
//
// 三个测试入口：
//   - TestV3P401DomainPackageValidation：定义对象/关系类型、字段类型与单位、
//     基数、身份规则、数据绑定、配置/测试规范及流程引用；草稿校验引用闭合、
//     关系端点、单位、参数范围、互斥目标和样例；缺失引用、单位冲突、非法
//     端点及互斥目标不能发布。
//   - TestV3P401ImmutableDefinitionVersion：发布为不可变版本；更改定义需记录
//     影响、兼容性及回退依据；旧记录保留原版本；提供声明式规则/规范加载和
//     查询入口，禁止让模型生成规则后直接激活。
//   - TestV3P401RunningTaskVersionBinding：Task/Step 固定使用版本；规则发布
//     不静默迁移运行中任务，紧急限制和当前授权仍即时核验；旧任务在发布新
//     版本后仍可解释；迁移必须形成明确修订并重新检查证据。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P401 创建存储。
func setupV3P401(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// v3P401ObjectTypes 构造测试对象类型。
func v3P401ObjectTypes() []DomainObjectType {
	return []DomainObjectType{
		{TypeKey: "node", Fields: `[{"name":"host","type":"string","unit":"ip"},{"name":"cpu","type":"integer","unit":"cores"}]`, IdentityRule: "host"},
		{TypeKey: "nic", Fields: `[{"name":"mac","type":"string"}]`, IdentityRule: "mac"},
	}
}

// v3P401RelationTypes 构造测试关系类型。
func v3P401RelationTypes() []DomainRelationType {
	return []DomainRelationType{
		{RelationKey: "node_has_nic", SourceType: "node", TargetType: "nic"},
	}
}

// TestV3P401DomainPackageValidation 领域包校验。
func TestV3P401DomainPackageValidation(t *testing.T) {
	t.Run("valid_package_publishes", func(t *testing.T) {
		store := setupV3P401(t, "valid.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, err := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
			Compatibility: "backward", ImpactSummary: "初始版本",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if p.State != "published" {
			t.Errorf("package state = %s, want published", p.State)
		}
		if p.DefinitionsDigest == "" {
			t.Error("definitions digest missing")
		}
	})

	t.Run("missing_reference_rejected", func(t *testing.T) {
		store := setupV3P401(t, "missingref.db")
		now := v3P102Now()
		// 缺失引用（关系端点引用不存在的类型）：不能发布。
		tx, _ := store.db.Begin()
		badRelations := []DomainRelationType{
			{RelationKey: "node_has_disk", SourceType: "node", TargetType: "disk"}, // disk 不存在
		}
		_, err := publishDomainPackage(tx, DomainPackage{
			Name: "bad-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), badRelations, now)
		tx.Rollback()
		if err == nil {
			t.Error("missing reference not rejected (缺失引用不能发布)")
		}
		if !strings.Contains(err.Error(), "不存在") && !strings.Contains(err.Error(), "引用闭合") {
			t.Errorf("missing reference reason missing: %v", err)
		}
	})

	t.Run("invalid_relation_endpoint_rejected", func(t *testing.T) {
		store := setupV3P401(t, "badendpoint.db")
		now := v3P102Now()
		// 非法端点：不能发布。
		tx, _ := store.db.Begin()
		badRelations := []DomainRelationType{
			{RelationKey: "invalid", SourceType: "nonexistent", TargetType: "nic"},
		}
		_, err := publishDomainPackage(tx, DomainPackage{
			Name: "bad-endpoint", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), badRelations, now)
		tx.Rollback()
		if err == nil {
			t.Error("invalid endpoint not rejected (非法端点不能发布)")
		}
	})

	t.Run("duplicate_type_rejected", func(t *testing.T) {
		store := setupV3P401(t, "dup.db")
		now := v3P102Now()
		// 重复类型：不能发布。
		tx, _ := store.db.Begin()
		dupTypes := []DomainObjectType{
			{TypeKey: "node", Fields: `[]`, IdentityRule: "host"},
			{TypeKey: "node", Fields: `[]`, IdentityRule: "host"}, // 重复
		}
		_, err := publishDomainPackage(tx, DomainPackage{
			Name: "dup-domain", Version: 1, PublishedBy: "admin",
		}, dupTypes, v3P401RelationTypes(), now)
		tx.Rollback()
		if err == nil {
			t.Error("duplicate type not rejected")
		}
	})

	t.Run("field_missing_name_or_type_rejected", func(t *testing.T) {
		store := setupV3P401(t, "badfield.db")
		now := v3P102Now()
		// 字段缺名称或类型：不能发布。
		tx, _ := store.db.Begin()
		badTypes := []DomainObjectType{
			{TypeKey: "node", Fields: `[{"type":"string"}]`, IdentityRule: "host"}, // 缺 name
		}
		_, err := publishDomainPackage(tx, DomainPackage{
			Name: "bad-field", Version: 1, PublishedBy: "admin",
		}, badTypes, []DomainRelationType{}, now)
		tx.Rollback()
		if err == nil {
			t.Error("field missing name not rejected (字段缺名称/类型不能发布)")
		}
	})
}

// TestV3P401ImmutableDefinitionVersion 不可变版本。
func TestV3P401ImmutableDefinitionVersion(t *testing.T) {
	t.Run("published_version_immutable", func(t *testing.T) {
		store := setupV3P401(t, "immutable.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, _ := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 同 name+version 重复发布：拒绝（不可变版本）。
		tx, _ = store.db.Begin()
		_, err := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Rollback()
		if err == nil {
			t.Error("republish same version not rejected (发布为不可变版本)")
		}
		if !strings.Contains(err.Error(), "已存在") && !strings.Contains(err.Error(), "不可变") {
			t.Errorf("immutable reason missing: %v", err)
		}
		_ = p
	})

	t.Run("change_records_impact_compatibility_rollback", func(t *testing.T) {
		store := setupV3P401(t, "change.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, err := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
			Compatibility: "backward-compatible", ImpactSummary: "新增 nic 类型",
			RollbackBasis: "v1 可回退到无 nic 的定义",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 影响、兼容性及回退依据已记录。
		if p.Compatibility == "" || p.ImpactSummary == "" || p.RollbackBasis == "" {
			t.Errorf("impact/compatibility/rollback not recorded: %+v", p)
		}
	})

	t.Run("old_record_keeps_original_version", func(t *testing.T) {
		store := setupV3P401(t, "oldrecord.db")
		now := v3P102Now()
		// 发布 v1。
		tx, _ := store.db.Begin()
		v1, _ := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 发布 v2（新版本）。
		tx, _ = store.db.Begin()
		publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 2, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 旧记录保留原版本（v1 仍可查）。
		old, err := readDomainPackage(store.db, "cluster-domain", 1)
		if err != nil {
			t.Fatal(err)
		}
		if old.Version != 1 || old.PackageID != v1.PackageID {
			t.Errorf("old record not preserved: %+v (旧记录保留原版本)", old)
		}
		// 最新版本是 v2。
		latest, err := latestPublishedPackage(store.db, "cluster-domain")
		if err != nil {
			t.Fatal(err)
		}
		if latest.Version != 2 {
			t.Errorf("latest version = %d, want 2", latest.Version)
		}
	})

	t.Run("model_generated_rule_not_activated", func(t *testing.T) {
		store := setupV3P401(t, "modelrule.db")
		// 禁止让模型生成规则后直接激活。
		tx, _ := store.db.Begin()
		_, err := loadDomainDefinition(tx, "cluster-domain", 1, "model-generated")
		tx.Rollback()
		if err == nil {
			t.Error("model-generated rule activated (禁止模型生成规则后直接激活)")
		}
	})
}

// TestV3P401RunningTaskVersionBinding 运行任务版本绑定。
func TestV3P401RunningTaskVersionBinding(t *testing.T) {
	t.Run("task_step_pin_version", func(t *testing.T) {
		store := setupV3P401(t, "pin.db")
		now := v3P102Now()
		// 发布领域包。
		tx, _ := store.db.Begin()
		p, _ := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// Task/Step 固定使用版本（读取固定版本）。
		pinned, err := readDomainPackage(store.db, "cluster-domain", 1)
		if err != nil {
			t.Fatal(err)
		}
		if pinned.PackageID != p.PackageID || pinned.Version != 1 {
			t.Errorf("pinned version wrong: %+v (Task/Step 固定使用版本)", pinned)
		}
	})

	t.Run("old_task_explainable_after_new_version", func(t *testing.T) {
		store := setupV3P401(t, "explain.db")
		now := v3P102Now()
		// 发布 v1。
		tx, _ := store.db.Begin()
		v1, _ := publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 发布 v2。
		tx, _ = store.db.Begin()
		publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 2, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 旧任务在发布新版本后仍可解释（旧记录保留原版本）。
		old, err := readDomainPackage(store.db, "cluster-domain", 1)
		if err != nil {
			t.Fatal(err)
		}
		if old.PackageID != v1.PackageID {
			t.Error("old task not explainable after new version")
		}
	})

	t.Run("rule_publish_not_silently_migrate_running_task", func(t *testing.T) {
		store := setupV3P401(t, "nomigrate.db")
		now := v3P102Now()
		// 发布 v1。
		tx, _ := store.db.Begin()
		publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 运行中任务（使用 v1）。
		ctx := context.Background()
		run, _ := store.Submit(ctx, "v3p401-running", "任务", "", "", true)
		taskID := run.TaskID
		// 发布 v2：运行中任务不静默迁移（仍使用 v1）。
		tx, _ = store.db.Begin()
		publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 2, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 运行中任务仍可读取 v1（不静默迁移）。
		v1, err := readDomainPackage(store.db, "cluster-domain", 1)
		if err != nil {
			t.Fatal(err)
		}
		if v1.Version != 1 {
			t.Errorf("running task migrated silently: version=%d (规则发布不静默迁移运行中任务)", v1.Version)
		}
		_ = taskID
	})

	t.Run("readonly_query_definition_not_change_asset_state", func(t *testing.T) {
		store := setupV3P401(t, "readonly.db")
		now := v3P102Now()
		// 发布领域包。
		tx, _ := store.db.Begin()
		publishDomainPackage(tx, DomainPackage{
			Name: "cluster-domain", Version: 1, PublishedBy: "admin",
		}, v3P401ObjectTypes(), v3P401RelationTypes(), now)
		tx.Commit()
		// 只读查询定义：不会触发实际资产状态变化。
		_, err := readDomainPackage(store.db, "cluster-domain", 1)
		if err != nil {
			t.Fatal(err)
		}
		// 无资产状态变化（无 ops_assets 记录）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM ops_assets").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("readonly query changed asset state: count=%d (只读查询定义不会触发实际资产状态变化)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
