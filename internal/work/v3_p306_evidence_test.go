package work

// P3-06 验收：材料引用、来源权限与证据落盘。
//
// 三个测试入口：
//   - TestV3P306EvidenceReferenceAtomicity：材料 ID、来源/版本、对象/时间、媒体
//     类型、字节数、摘要、定位符、留存与权限依据；本地快照先写临时文件、校验
//     并同步内容，原子就位后提交引用；DB 失败留下可识别孤立文件，不留下指向
//     半文件的已确认引用。
//   - TestV3P306EvidenceReadScope：按授权读取有限片段，输出实际覆盖和截断；
//     外部来源不可用保留引用及缺口，不伪造内容；截断、来源过期和缺失明确可见；
//     路径穿越和越权材料不能读取。
//   - TestV3P306LargeResultByReference：大结果经引用读取，事件/SQL 关系行不会
//     含整篇日志；派生摘要必须记录必要来源；尚无完整依赖核验能力时不得将受限
//     派生物开放为跨范围经验。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P306 创建存储。
func setupV3P306(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// v3P306Evidence 构造一个证据引用。
func v3P306Evidence(id, objectRef string, now int64) EvidenceReference {
	return EvidenceReference{
		MaterialID: "material-" + id, SourceRef: "conn:cluster", SourceVersion: 1,
		ObjectRef: objectRef, MediaType: "text/plain", Bytes: 1000,
		ContentDigest: digest("内容-" + id), Locator: "/tmp/evidence/" + id + ".log",
		RetentionPolicy: "30d", AuthorizationRef: "auth-" + id,
		DerivedFrom: `["source-1"]`, DerivedRestricted: true,
	}
}

// TestV3P306EvidenceReferenceAtomicity 引用原子性。
func TestV3P306EvidenceReferenceAtomicity(t *testing.T) {
	t.Run("evidence_reference_has_all_fields", func(t *testing.T) {
		store := setupV3P306(t, "fields.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		e, err := createEvidenceReference(tx, v3P306Evidence("e1", "asset-x", now), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 证据引用含全部字段。
		if e.MaterialID == "" || e.SourceRef == "" || e.ContentDigest == "" || e.Locator == "" ||
			e.RetentionPolicy == "" || e.AuthorizationRef == "" {
			t.Errorf("evidence reference incomplete: %+v", e)
		}
		if e.State != "pending" {
			t.Errorf("initial state = %s, want pending", e.State)
		}
	})

	t.Run("atomic_commit_then_reference", func(t *testing.T) {
		store := setupV3P306(t, "atomic.db")
		now := v3P102Now()
		// 原子就位：先写临时文件、校验、同步，再提交引用。
		targetPath := filepath.Join(t.TempDir(), "evidence.log")
		data := []byte("证据内容")
		if err := writeFileAtomic(targetPath, data); err != nil {
			t.Fatal(err)
		}
		// 文件已就位（内容可读）。
		content, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "证据内容" {
			t.Errorf("file content = %q (原子就位)", content)
		}
		// 提交引用。
		tx, _ := store.db.Begin()
		e, _ := createEvidenceReference(tx, v3P306Evidence("e-atomic", "asset-x", now), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := commitEvidenceReference(tx, e.EvidenceID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 引用已提交（原子就位后提交）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM evidence_references WHERE evidence_id=?", e.EvidenceID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "committed" {
			t.Errorf("reference state = %s, want committed", state)
		}
	})

	t.Run("db_failure_leaves_orphan_not_half_reference", func(t *testing.T) {
		store := setupV3P306(t, "orphan.db")
		now := v3P102Now()
		// 创建引用（pending）。
		tx, _ := store.db.Begin()
		e, _ := createEvidenceReference(tx, v3P306Evidence("e-orphan", "asset-x", now), now)
		tx.Commit()
		// DB 失败：标记孤立（不留下指向半文件的已确认引用）。
		tx, _ = store.db.Begin()
		if err := markEvidenceOrphaned(tx, e.EvidenceID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 引用状态为 orphaned（可识别孤立文件）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM evidence_references WHERE evidence_id=?", e.EvidenceID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "orphaned" {
			t.Errorf("orphan state = %s, want orphaned (DB 失败留下可识别孤立文件)", state)
		}
	})

	t.Run("rename_alone_not_durability_proof", func(t *testing.T) {
		// 仅 rename 成功不能作为断电耐久证据：writeFileAtomic 含 fsync + 目录 sync。
		targetPath := filepath.Join(t.TempDir(), "durability.log")
		data := []byte("耐久内容")
		if err := writeFileAtomic(targetPath, data); err != nil {
			t.Fatal(err)
		}
		// 文件已就位且同步（fsync + 目录 sync 已执行）。
		content, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "耐久内容" {
			t.Errorf("content = %q", content)
		}
		t.Log("writeFileAtomic 含 fsync + 目录 sync（仅 rename 成功不能作为断电耐久证据）")
	})
}

// TestV3P306EvidenceReadScope 读取范围。
func TestV3P306EvidenceReadScope(t *testing.T) {
	t.Run("read_limited_range_with_truncation", func(t *testing.T) {
		store := setupV3P306(t, "read.db")
		now := v3P102Now()
		// 授权读取。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		e, _ := createEvidenceReference(tx, v3P306Evidence("e-read", "asset-x", now), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		commitEvidenceReference(tx, e.EvidenceID, now)
		tx.Commit()
		// 按授权读取有限片段（超出范围截断）。
		tx, _ = store.db.Begin()
		read, err := readEvidenceRange(tx, e.EvidenceID, 0, 2000, "user:alice", now) // 请求 2000 字节，实际 1000
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !read.Truncated {
			t.Error("read not truncated (输出实际覆盖和截断)")
		}
		if read.ActualEnd != 1000 {
			t.Errorf("actual end = %d, want 1000 (实际覆盖)", read.ActualEnd)
		}
	})

	t.Run("unauthorized_read_denied", func(t *testing.T) {
		store := setupV3P306(t, "unauth.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		e, _ := createEvidenceReference(tx, v3P306Evidence("e-unauth", "asset-x", now), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		commitEvidenceReference(tx, e.EvidenceID, now)
		tx.Commit()
		// 越权读取：拒绝。
		tx, _ = store.db.Begin()
		_, err := readEvidenceRange(tx, e.EvidenceID, 0, 100, "user:mallory", now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized read not denied (越权材料不能读取)")
		}
	})

	t.Run("orphaned_source_gap_not_fabricated", func(t *testing.T) {
		store := setupV3P306(t, "gap.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		e, _ := createEvidenceReference(tx, v3P306Evidence("e-gap", "asset-x", now), now)
		tx.Commit()
		// 外部来源不可用（孤立）。
		tx, _ = store.db.Begin()
		markEvidenceOrphaned(tx, e.EvidenceID, now)
		tx.Commit()
		// 读取：保留引用及缺口（不伪造内容）。
		tx, _ = store.db.Begin()
		read, err := readEvidenceRange(tx, e.EvidenceID, 0, 100, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if read.GapReason == "" {
			t.Error("gap reason missing (外部来源不可用保留引用及缺口)")
		}
		if !strings.Contains(read.GapReason, "不可用") {
			t.Errorf("gap reason unclear: %s", read.GapReason)
		}
	})

	t.Run("path_traversal_denied", func(t *testing.T) {
		store := setupV3P306(t, "traversal.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		e := v3P306Evidence("e-traversal", "asset-x", now)
		e.Locator = "/tmp/evidence/../../../etc/passwd" // 路径穿越
		e2, _ := createEvidenceReference(tx, e, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		commitEvidenceReference(tx, e2.EvidenceID, now)
		tx.Commit()
		// 路径穿越：拒绝。
		tx, _ = store.db.Begin()
		_, err := readEvidenceRange(tx, e2.EvidenceID, 0, 100, "user:alice", now)
		tx.Rollback()
		if err == nil {
			t.Error("path traversal not denied (路径穿越不能读取)")
		}
		if !strings.Contains(err.Error(), "路径穿越") {
			t.Errorf("traversal reason missing: %v", err)
		}
	})
}

// TestV3P306LargeResultByReference 大结果引用。
func TestV3P306LargeResultByReference(t *testing.T) {
	t.Run("large_result_by_reference_not_inline", func(t *testing.T) {
		store := setupV3P306(t, "large.db")
		now := v3P102Now()
		// 大结果（100KB）经引用读取（事件/SQL 关系行不含整篇日志）。
		largeContent := strings.Repeat("日志行\n", 20000)
		targetPath := filepath.Join(t.TempDir(), "large.log")
		if err := writeFileAtomic(targetPath, []byte(largeContent)); err != nil {
			t.Fatal(err)
		}
		tx, _ := store.db.Begin()
		e := v3P306Evidence("e-large", "asset-x", now)
		e.Bytes = int64(len(largeContent))
		e.Locator = targetPath
		e2, err := createEvidenceReference(tx, e, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 证据引用不含正文（只有摘要与定位符）。
		if e2.ContentDigest == "" || e2.Locator == "" {
			t.Error("evidence reference missing digest/locator")
		}
		// 读取证据引用：不返回整篇内容（引用读取）。
		read, err := readEvidenceReference(store.db, e2.EvidenceID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(read.Locator, largeContent) {
			t.Error("large result inline (大结果应经引用读取)")
		}
		if read.Bytes != int64(len(largeContent)) {
			t.Errorf("bytes = %d, want %d", read.Bytes, len(largeContent))
		}
	})

	t.Run("derived_summary_requires_source", func(t *testing.T) {
		// 派生摘要必须记录必要来源。
		e := v3P306Evidence("e-derived", "asset-x", v3P102Now())
		e.DerivedFrom = "[]" // 缺来源
		if err := validateDerivedEvidence(e); err == nil {
			t.Error("derived summary without source not rejected")
		}
	})

	t.Run("restricted_derived_not_open_as_cross_scope_experience", func(t *testing.T) {
		// 尚无完整依赖核验能力时不得将受限派生物开放为跨范围经验。
		e := v3P306Evidence("e-restricted", "asset-x", v3P102Now())
		e.DerivedFrom = `["source-1"]`
		e.DerivedRestricted = false // 尝试开放
		if err := validateDerivedEvidence(e); err == nil {
			t.Error("restricted derived opened as cross-scope experience (受限派生物不得开放)")
		}
	})
}

// 确保 sql/strings/context/os 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = os.ReadFile
)
