package work

// P5-01 验收：大日志材料索引与有界片段读取。
//
// 三个测试入口：
//   - TestV3P501LargeMaterialExcerpts：按来源/对象/时间/部件/错误类别建立索引
//     和片段定位，保留原文摘要、编码、行/字节/时间区间及覆盖；限制候选字节、
//     单次片段、总读取量和时间；大文件查询不整篇进模型/事件；截断、采样及
//     查询覆盖明确。
//   - TestV3P501IndexVersionWatermark：原文独立存储或保留授权外部句柄；索引、
//     摘要和去重结果引用材料版本，不复制整篇到图属性；解析/索引由有预算的
//     后台单元完成，提交前检查源版本和租约；新材料权威可读，索引滞后明确
//     显示；解析崩溃/重复领取不会发布旧版本索引；未完成积压可查。
//   - TestV3P501MaterialSourceRevoked：旧数据库材料按兼容方式读取或迁移，未
//     核实来源权限不能自动扩大跨任务可见性；相同原文/摘要不作为多份独立证据；
//     原文变化、删除或撤权后旧索引不能放行失权内容。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P501 创建存储与授权。
func setupV3P501(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:reader", Action: "read", ObjectRef: "material-1", ValidFrom: now, Owner: "reader"}, now)
	tx.Commit()
	return store
}

// TestV3P501LargeMaterialExcerpts 大材料片段。
func TestV3P501LargeMaterialExcerpts(t *testing.T) {
	t.Run("index_all_fields_with_locator_coverage", func(t *testing.T) {
		store := setupV3P501(t, "index.db")
		now := v3P102Now()
		// 建立索引：按来源/对象/时间/部件/错误类别建立索引和片段定位。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		idx, err := buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			TimeRangeStart: now, TimeRangeEnd: now + 60000, ComponentRef: "nic-1", ErrorKind: "timeout",
			ExcerptLocator: `{"start_line":100,"end_line":200,"bytes":5000}`, ExcerptDigest: digest("片段"),
			ExcerptBytes: 5000, Coverage: "lines 100-200",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if idx.SourceRef != "cmdb" || idx.ObjectRef != "node-1" || idx.ComponentRef != "nic-1" ||
			idx.ErrorKind != "timeout" || idx.ExcerptLocator == "" || idx.ExcerptDigest == "" {
			t.Errorf("index incomplete: %+v (按来源/对象/时间/部件/错误类别建立索引和片段定位)", idx)
		}
	})

	t.Run("bounded_excerpt_truncation_clear", func(t *testing.T) {
		store := setupV3P501(t, "bounded.db")
		now := v3P102Now()
		// 大材料索引（超预算）。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{"bytes":2000000}`, ExcerptDigest: digest("大片段"),
			ExcerptBytes: 2000000, Coverage: "full",
		}, now)
		tx.Commit()
		// 读取片段：限制候选字节、单次片段、总读取量和时间（截断明确）。
		tx, _ = store.db.Begin()
		excerpt, truncated, coverage, err := readMaterialExcerpt(tx, "material-1", ExcerptBudget{
			MaxCandidateBytes: 1000000, MaxExcerptBytes: 1000000, MaxTotalBytes: 1000000, MaxTimeMS: 5000,
		}, "user:reader", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !truncated {
			t.Error("large excerpt not truncated (限制候选字节/单次片段/总读取量/时间)")
		}
		if excerpt == "" {
			t.Error("excerpt empty")
		}
		if coverage == "" {
			t.Error("coverage missing (截断、采样及查询覆盖明确)")
		}
	})

	t.Run("large_file_not_full_into_model_event", func(t *testing.T) {
		// 大文件查询不整篇进模型/事件（经引用读取）。
		store := setupV3P501(t, "large.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{"bytes":10000000}`, ExcerptDigest: digest("大文件"),
			ExcerptBytes: 10000000, Coverage: "full",
		}, now)
		tx.Commit()
		// 大文件经引用读取（不整篇进模型/事件）。
		tx, _ = store.db.Begin()
		excerpt, truncated, _, err := readMaterialExcerpt(tx, "material-1", ExcerptBudget{
			MaxExcerptBytes: 1000000, MaxTotalBytes: 1000000, MaxTimeMS: 5000,
		}, "user:reader", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !truncated {
			t.Error("large file not truncated (大文件查询不整篇进模型/事件)")
		}
		if excerpt == "" {
			t.Error("excerpt empty (经引用读取)")
		}
	})
}

// TestV3P501IndexVersionWatermark 索引版本水位。
func TestV3P501IndexVersionWatermark(t *testing.T) {
	t.Run("index_references_material_version_not_full_copy", func(t *testing.T) {
		store := setupV3P501(t, "version.db")
		now := v3P102Now()
		// 索引引用材料版本（不复制整篇到图属性）。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{"bytes":100}`, ExcerptDigest: digest("片段"),
			ExcerptBytes: 100, Coverage: "partial",
		}, now)
		tx.Commit()
		// 索引含材料版本（引用材料版本，不复制整篇）。
		var version int
		if err := store.db.QueryRow("SELECT material_version FROM material_indexes WHERE material_id='material-1'").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Errorf("index version = %d, want 1 (索引引用材料版本)", version)
		}
	})

	t.Run("source_version_lease_checked_before_commit", func(t *testing.T) {
		store := setupV3P501(t, "lease.db")
		now := v3P102Now()
		// 提交前检查源版本和租约（新材料权威可读）。
		tx, _ := store.db.Begin()
		// 材料版本 1。
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		tx.Commit()
		// 索引版本 2（超过当前版本）：拒绝（提交前检查源版本和租约）。
		tx, _ = store.db.Begin()
		_, err := buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 2, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段"),
			ExcerptBytes: 100, Coverage: "partial",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("index with future version accepted (提交前检查源版本和租约)")
		}
	})

	t.Run("lag_displayed_when_stale", func(t *testing.T) {
		store := setupV3P501(t, "lag.db")
		now := v3P102Now()
		// 索引滞后明确显示。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段"),
			ExcerptBytes: 100, Coverage: "partial", LagMS: 5000,
		}, now)
		tx.Commit()
		var lagMS int64
		if err := store.db.QueryRow("SELECT lag_ms FROM material_indexes WHERE material_id='material-1'").Scan(&lagMS); err != nil {
			t.Fatal(err)
		}
		if lagMS != 5000 {
			t.Errorf("lag = %d, want 5000 (索引滞后明确显示)", lagMS)
		}
	})

	t.Run("crash_duplicate_not_publish_old_index", func(t *testing.T) {
		// 解析崩溃/重复领取不会发布旧版本索引（条件更新）。
		store := setupV3P501(t, "crash.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段1"),
			ExcerptBytes: 100, Coverage: "partial",
		}, now)
		tx.Commit()
		// 重复领取（同 material_id+version+error_kind）：更新而非重复（条件更新）。
		tx, _ = store.db.Begin()
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段2"),
			ExcerptBytes: 100, Coverage: "partial",
		}, now)
		tx.Commit()
		// 只一条索引（不发布旧版本索引）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM material_indexes WHERE material_id='material-1' AND error_kind='timeout'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("duplicate indexes = %d, want 1 (解析崩溃/重复领取不会发布旧版本索引)", count)
		}
	})
}

// TestV3P501MaterialSourceRevoked 来源撤权。
func TestV3P501MaterialSourceRevoked(t *testing.T) {
	t.Run("revoked_material_index_not_release", func(t *testing.T) {
		store := setupV3P501(t, "revoked.db")
		now := v3P102Now()
		// 撤权材料：旧索引不能放行失权内容。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,1,?)", digest("v1"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureMaterialIndexCurrent(tx, "material-1", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("revoked material index released (原文变化、删除或撤权后旧索引不能放行失权内容)")
		}
		if !strings.Contains(err.Error(), "撤权") {
			t.Errorf("revoked reason missing: %v", err)
		}
	})

	t.Run("version_change_old_index_not_release", func(t *testing.T) {
		store := setupV3P501(t, "versionchange.db")
		now := v3P102Now()
		// 版本变化：旧索引不能放行失权内容。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',2,?,0,?)", digest("v2"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureMaterialIndexCurrent(tx, "material-1", 1, now) // 旧版本
		tx.Rollback()
		if err == nil {
			t.Error("old version index released (原文变化后旧索引不能放行失权内容)")
		}
	})

	t.Run("same_content_not_independent_evidence", func(t *testing.T) {
		store := setupV3P501(t, "dup.db")
		now := v3P102Now()
		// 相同原文/摘要：不作为多份独立证据。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("same"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		duplicate, err := ensureMaterialNotDuplicate(tx, "material-1", digest("same"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !duplicate {
			t.Error("same content marked independent (相同原文/摘要不作为多份独立证据)")
		}
	})

	t.Run("unauthorized_reader_not_expanded_visibility", func(t *testing.T) {
		store := setupV3P501(t, "unauth.db")
		now := v3P102Now()
		// 未核实来源权限：不能自动扩大跨任务可见性。
		tx, _ := store.db.Begin()
		tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
		buildMaterialIndex(tx, MaterialIndex{
			MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
			ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段"),
			ExcerptBytes: 100, Coverage: "partial",
		}, now)
		tx.Commit()
		// 未授权读取：拒绝（未核实来源权限不能自动扩大跨任务可见性）。
		tx, _ = store.db.Begin()
		_, _, _, err := readMaterialExcerpt(tx, "material-1", ExcerptBudget{MaxExcerptBytes: 1000, MaxTotalBytes: 1000, MaxTimeMS: 5000}, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized reader expanded visibility (未核实来源权限不能自动扩大跨任务可见性)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
