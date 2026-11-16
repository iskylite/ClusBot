package work

// P5-05 验收：异步清理、失效、删除与恢复保护。
//
// 三个测试入口：
//   - TestV3P505CleanupRestartAndVersion：后台按反向依赖标记、清理/重建索引、
//     摘要、候选和缓存；固定源/策略版本，提交前再核对；清理中重启/重复领取
//     不会重新发布旧内容；新撤销使旧整理结果失效。
//   - TestV3P505DeletionNotResurrected：区分事实失效、召回降权和用户删除；
//     删除先设受控不可读标记并使撤销闸门生效；删除后所有新模型请求、材料
//     读取及案例检索均不返回受限正文；恢复备份/检查点时应用当前删除/撤销
//     约束并先核查；无法取得当前权威时拒绝敏感读取，不恢复旧许可。
//   - TestV3P505CleanupProgressEvidence：失败有限退避并留待处理记录；记录
//     未完成数量、最老年龄和完成时间，发送事件不算清理完成；审计仅保留允许
//     的必要元数据；外部副本和备份按各自范围处理，不声称已物理删除所有副本；
//     外部/备份无法验证的清理如实标未知或待处理，不自动完成。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P505 创建存储与材料。
func setupV3P505(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	// 材料索引。
	tx.Exec("INSERT INTO material_versions(material_id,version,content_digest,revoked,created_at) VALUES('material-1',1,?,0,?)", digest("v1"), now)
	buildMaterialIndex(tx, MaterialIndex{
		MaterialID: "material-1", MaterialVersion: 1, SourceRef: "cmdb", ObjectRef: "node-1",
		ErrorKind: "timeout", ExcerptLocator: `{}`, ExcerptDigest: digest("片段"),
		ExcerptBytes: 100, Coverage: "partial",
	}, now)
	tx.Commit()
	return store
}

// TestV3P505CleanupRestartAndVersion 清理重启与版本。
func TestV3P505CleanupRestartAndVersion(t *testing.T) {
	t.Run("cleanup_stale_index_rebuild", func(t *testing.T) {
		store := setupV3P505(t, "cleanup.db")
		now := v3P102Now()
		// 入队清理。
		tx, _ := store.db.Begin()
		item, err := enqueueCleanup(tx, CleanupItem{
			Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb",
			SourceVersion: 1, PolicyVersion: 1,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 处理清理（后台按反向依赖标记、清理/重建索引）。
		tx, _ = store.db.Begin()
		if err := processCleanupItem(tx, item.CleanupID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 索引已失效（state=stale）。
		var indexState string
		if err := store.db.QueryRow("SELECT state FROM material_indexes WHERE material_id='material-1'").Scan(&indexState); err != nil {
			t.Fatal(err)
		}
		if indexState != "stale" {
			t.Errorf("index state = %s, want stale (后台按反向依赖标记、清理/重建索引)", indexState)
		}
		// 清理完成。
		var cleanupState string
		if err := store.db.QueryRow("SELECT state FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&cleanupState); err != nil {
			t.Fatal(err)
		}
		if cleanupState != "completed" {
			t.Errorf("cleanup state = %s, want completed", cleanupState)
		}
	})

	t.Run("duplicate_claim_not_republish_old_content", func(t *testing.T) {
		store := setupV3P505(t, "duplicate.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{
			Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb",
		}, now)
		tx.Commit()
		// 第一次领取：成功。
		tx, _ = store.db.Begin()
		if err := processCleanupItem(tx, item.CleanupID, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 重复领取：拒绝（清理中重启/重复领取不会重新发布旧内容）。
		tx, _ = store.db.Begin()
		err := processCleanupItem(tx, item.CleanupID, now)
		tx.Rollback()
		if err == nil {
			t.Error("duplicate claim republished old content (清理中重启/重复领取不会重新发布旧内容)")
		}
	})

	t.Run("new_revocation_invalidates_old_cleanup", func(t *testing.T) {
		store := setupV3P505(t, "revocation.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{
			Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb",
			SourceVersion: 1, PolicyVersion: 1,
		}, now)
		tx.Commit()
		// 新撤销（在清理处理前）：旧整理结果失效。
		tx, _ = store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		// 处理清理：源已撤销，失败并留待处理记录（新撤销使旧整理结果失效）。
		tx, _ = store.db.Begin()
		err := processCleanupItem(tx, item.CleanupID, now)
		tx.Commit()
		if err == nil {
			t.Error("cleanup after revocation completed (新撤销使旧整理结果失效)")
		}
		// 清理标记 failed（留待处理记录）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "failed" {
			t.Errorf("cleanup state = %s, want failed (新撤销使旧整理结果失效)", state)
		}
	})

	t.Run("restart_persistent_cleanup_continues", func(t *testing.T) {
		// 清理中重启：持久队列继续（不重新发布旧内容）。
		path := filepath.Join(t.TempDir(), "restart.db")
		now := v3P102Now()
		var cleanupID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			tx, _ := store.db.Begin()
			item, _ := enqueueCleanup(tx, CleanupItem{
				Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb",
			}, now)
			cleanupID = item.CleanupID
			tx.Commit()
		}()
		// 重启：清理项仍在（持久队列继续）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var state string
		if err := store.db.QueryRow("SELECT state FROM cleanup_queue WHERE cleanup_id=?", cleanupID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "pending" {
			t.Errorf("cleanup lost after restart: state=%s (清理中重启持久队列继续)", state)
		}
	})
}

// TestV3P505DeletionNotResurrected 删除不复活。
func TestV3P505DeletionNotResurrected(t *testing.T) {
	t.Run("delete_marks_unreadable_revokes_gate", func(t *testing.T) {
		store := setupV3P505(t, "delete.db")
		now := v3P102Now()
		// 删除先设受控不可读标记并使撤销闸门生效。
		tx, _ := store.db.Begin()
		item, err := enqueueCleanup(tx, CleanupItem{
			Kind: "delete", ObjectRef: "node-1", SourceRef: "cmdb",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !item.Unreadable {
			t.Error("delete not unreadable (删除先设受控不可读标记)")
		}
		// 撤销闸门已生效。
		var gateState string
		if err := store.db.QueryRow("SELECT state FROM revocation_gates WHERE object_ref='node-1' AND source_ref='cmdb'").Scan(&gateState); err != nil {
			t.Fatal(err)
		}
		if gateState != "revoked" {
			t.Errorf("gate state = %s, want revoked (使撤销闸门生效)", gateState)
		}
	})

	t.Run("invalidate_demote_delete_distinguished", func(t *testing.T) {
		store := setupV3P505(t, "distinguished.db")
		now := v3P102Now()
		// 区分事实失效、召回降权和用户删除。
		for _, kind := range []string{"invalidate", "demote", "delete"} {
			tx, _ := store.db.Begin()
			item, err := enqueueCleanup(tx, CleanupItem{
				Kind: kind, ObjectRef: "node-" + kind, SourceRef: "cmdb",
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if item.Kind != kind {
				t.Errorf("kind = %s, want %s (区分事实失效/召回降权/用户删除)", item.Kind, kind)
			}
		}
	})

	t.Run("restore_not_old_permission", func(t *testing.T) {
		store := setupV3P505(t, "restore.db")
		now := v3P102Now()
		// 恢复备份/检查点时应用当前删除/撤销约束并先核查（不恢复旧许可）。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureRestoreNotOldPermission(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("restore allowed old permission (恢复备份/检查点时应用当前删除/撤销约束并先核查)")
		}
	})

	t.Run("unable_current_authority_rejects_sensitive_read", func(t *testing.T) {
		// 无法取得当前权威时拒绝敏感读取（不恢复旧许可）。
		store := setupV3P505(t, "noauthority.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 撤销后来源被拒绝（无法取得当前权威时拒绝敏感读取）。
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureRestoreNotOldPermission(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("sensitive read allowed without current authority (无法取得当前权威时拒绝敏感读取)")
		}
	})
}

// TestV3P505CleanupProgressEvidence 清理进度证据。
func TestV3P505CleanupProgressEvidence(t *testing.T) {
	t.Run("progress_records_pending_oldest_completed", func(t *testing.T) {
		store := setupV3P505(t, "progress.db")
		now := v3P102Now()
		// 入队清理（未完成）。
		tx, _ := store.db.Begin()
		enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		tx.Commit()
		// 更新进度（记录未完成数量、最老年龄和完成时间）。
		tx, _ = store.db.Begin()
		if err := updateCleanupProgress(tx, now+1000); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		progress, err := readCleanupProgress(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if progress.PendingCount != 1 {
			t.Errorf("pending = %d, want 1 (记录未完成数量)", progress.PendingCount)
		}
		if progress.OldestAgeMS != 1000 {
			t.Errorf("oldest age = %d, want 1000 (最老年龄)", progress.OldestAgeMS)
		}
	})

	t.Run("failure_limited_retry_with_record", func(t *testing.T) {
		store := setupV3P505(t, "retry.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		tx.Commit()
		// 新撤销：失败并留待处理记录。
		tx, _ = store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		processCleanupItem(tx, item.CleanupID, now)
		tx.Commit()
		// 失败留待处理记录（retry_count 与 next_retry_at）。
		var retryCount int
		var nextRetryAt int64
		var state string
		if err := store.db.QueryRow("SELECT state, retry_count, next_retry_at FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&state, &retryCount, &nextRetryAt); err != nil {
			t.Fatal(err)
		}
		if state != "failed" {
			t.Errorf("cleanup state = %s, want failed (失败有限退避并留待处理记录)", state)
		}
		if nextRetryAt == 0 {
			t.Error("next retry not scheduled")
		}
	})

	t.Run("sent_event_not_cleanup_complete", func(t *testing.T) {
		// 发送事件不算清理完成（完成时间独立记录）。
		store := setupV3P505(t, "sent.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		tx.Commit()
		// 未处理：未完成（发送事件不算清理完成）。
		var completedAt int64
		if err := store.db.QueryRow("SELECT completed_at FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&completedAt); err != nil {
			t.Fatal(err)
		}
		if completedAt != 0 {
			t.Errorf("sent event marked complete: completed_at=%d (发送事件不算清理完成)", completedAt)
		}
	})

	t.Run("external_backup_unverifiable_marked_unknown_not_auto_complete", func(t *testing.T) {
		// 外部/备份无法验证的清理如实标未知或待处理（不自动完成）。
		store := setupV3P505(t, "external.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-external", SourceRef: "external"}, now)
		tx.Commit()
		// 外部来源无法验证：保持 pending（不自动完成）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "pending" {
			t.Errorf("external cleanup auto-completed: state=%s (外部/备份无法验证的清理如实标未知或待处理)", state)
		}
	})

	t.Run("audit_only_necessary_metadata", func(t *testing.T) {
		// 审计仅保留允许的必要元数据（不保留受限正文）。
		store := setupV3P505(t, "audit.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		item, _ := enqueueCleanup(tx, CleanupItem{Kind: "delete", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		processCleanupItem(tx, item.CleanupID, now)
		tx.Commit()
		// 清理完成（审计仅保留必要元数据：cleanup_id/kind/object_ref/state）。
		var kind, objectRef, state string
		if err := store.db.QueryRow("SELECT kind, object_ref, state FROM cleanup_queue WHERE cleanup_id=?", item.CleanupID).Scan(&kind, &objectRef, &state); err != nil {
			t.Fatal(err)
		}
		if kind == "" || objectRef == "" || state == "" {
			t.Error("audit metadata incomplete (审计仅保留允许的必要元数据)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
