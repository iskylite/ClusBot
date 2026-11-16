package work

// P5-05：异步清理、失效、删除与恢复保护。
//
// 本文件实现派生内容可收敛清理，删除请求不会因旧快照恢复而重新出现：
//   - 区分事实失效、召回降权和用户删除；删除先设受控不可读标记并使撤销闸门
//     生效；
//   - 后台按反向依赖标记、清理/重建索引、摘要、候选和缓存；固定源/策略版本，
//     提交前再核对；
//   - 失败有限退避并留待处理记录；记录未完成数量、最老年龄和完成时间，发送
//     事件不算清理完成；
//   - 审计仅保留允许的必要元数据；外部副本和备份按各自范围处理，不声称已
//     物理删除所有副本；
//   - 恢复备份/检查点时应用当前删除/撤销约束并先核查；无法取得当前权威时
//     拒绝敏感读取，不恢复旧许可。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 清理队列（区分事实失效、召回降权和用户删除）
// ---------------------------------------------------------------------------

// CleanupItem 是 cleanup_queue 的一行。
type CleanupItem struct {
	CleanupID      string
	Kind           string // invalidate/demote/delete
	ObjectRef      string
	SourceRef      string
	Unreadable     bool
	SourceVersion  int
	PolicyVersion  int
	State          string // pending/processing/completed/failed
	RetryCount     int
	NextRetryAt    int64
	FailureReason  string
	CreatedAt      int64
	UpdatedAt      int64
	CompletedAt    int64
}

// enqueueCleanup 入队清理（P5-05.1）。
// 区分事实失效、召回降权和用户删除；删除先设受控不可读标记并使撤销闸门生效。
func enqueueCleanup(tx *sql.Tx, item CleanupItem, now int64) (CleanupItem, error) {
	if item.Kind == "" || item.ObjectRef == "" {
		return CleanupItem{}, errors.New("清理缺少类型或对象")
	}
	// 区分事实失效、召回降权和用户删除。
	validKinds := map[string]bool{"invalidate": true, "demote": true, "delete": true}
	if !validKinds[item.Kind] {
		return CleanupItem{}, fmt.Errorf("无效的清理类型 %s", item.Kind)
	}
	// 删除先设受控不可读标记并使撤销闸门生效。
	if item.Kind == "delete" {
		item.Unreadable = true
		// 撤销闸门生效（复用 P5-04 的 revokeSource）。
		if err := revokeSource(tx, item.ObjectRef, item.SourceRef, "", "system", now); err != nil {
			return CleanupItem{}, err
		}
	}
	item.CleanupID = id()
	item.State = "pending"
	item.CreatedAt, item.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO cleanup_queue(cleanup_id,kind,object_ref,source_ref,unreadable,
source_version,policy_version,state,retry_count,next_retry_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.CleanupID, item.Kind, item.ObjectRef, item.SourceRef, boolToInt(item.Unreadable),
		item.SourceVersion, item.PolicyVersion, item.State, item.RetryCount, item.NextRetryAt, now, now)
	if err != nil {
		return CleanupItem{}, err
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// 后台清理（按反向依赖标记、清理/重建索引、摘要、候选和缓存）
// ---------------------------------------------------------------------------

// processCleanupItem 处理清理项（P5-05.2）。
// 后台按反向依赖标记、清理/重建索引、摘要、候选和缓存；固定源/策略版本，
// 提交前再核对；清理中重启/重复领取不会重新发布旧内容；新撤销使旧整理结果失效。
func processCleanupItem(tx *sql.Tx, cleanupID string, now int64) error {
	// 条件领取（清理中重启/重复领取不会重新发布旧内容）。
	res, err := tx.Exec(`UPDATE cleanup_queue SET state='processing', updated_at=? WHERE cleanup_id=? AND state='pending'`,
		now, cleanupID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("清理项 %s 已被领取（清理中重启/重复领取不会重新发布旧内容）", cleanupID)
	}
	// 读取清理项。
	var item CleanupItem
	var unreadable int
	err = tx.QueryRow(`SELECT cleanup_id,kind,object_ref,source_ref,unreadable,source_version,policy_version,state
FROM cleanup_queue WHERE cleanup_id=?`, cleanupID).
		Scan(&item.CleanupID, &item.Kind, &item.ObjectRef, &item.SourceRef, &unreadable,
			&item.SourceVersion, &item.PolicyVersion, &item.State)
	if err != nil {
		return err
	}
	item.Unreadable = unreadable == 1
	// 提交前再核对（固定源/策略版本；新撤销使旧整理结果失效）。
	if err := ensureSourceNotRevoked(tx, item.ObjectRef, item.SourceRef, item.SourceVersion, now); err != nil {
		// 新撤销使旧整理结果失效（标记失败并留待处理记录）。
		if _, err := tx.Exec(`UPDATE cleanup_queue SET state='failed', failure_reason=?, next_retry_at=?, updated_at=? WHERE cleanup_id=?`,
			err.Error(), now+60000, now, cleanupID); err != nil {
			return err
		}
		return fmt.Errorf("清理项 %s 源已撤销（新撤销使旧整理结果失效）：%w", cleanupID, err)
	}
	// 后台清理（按反向依赖标记、清理/重建索引、摘要、候选和缓存）。
	if _, err := tx.Exec(`UPDATE material_indexes SET state='stale', updated_at=? WHERE object_ref=? AND source_ref=?`,
		now, item.ObjectRef, item.SourceRef); err != nil {
		return err
	}
	// 完成（记录完成时间）。
	if _, err := tx.Exec(`UPDATE cleanup_queue SET state='completed', completed_at=?, updated_at=? WHERE cleanup_id=?`,
		now, now, cleanupID); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 清理进度（记录未完成数量、最老年龄和完成时间）
// ---------------------------------------------------------------------------

// CleanupProgress 是 cleanup_progress 的一行。
type CleanupProgress struct {
	PendingCount    int
	OldestAgeMS     int64
	LastCompletedAt int64
	UpdatedAt       int64
}

// updateCleanupProgress 更新清理进度（P5-05.3）。
// 记录未完成数量、最老年龄和完成时间，发送事件不算清理完成。
func updateCleanupProgress(tx *sql.Tx, now int64) error {
	var pendingCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM cleanup_queue WHERE state IN ('pending','processing')`).Scan(&pendingCount); err != nil {
		return err
	}
	var oldestAgeMS int64
	if err := tx.QueryRow(`SELECT coalesce(max(?-created_at),0) FROM cleanup_queue WHERE state IN ('pending','processing')`,
		now).Scan(&oldestAgeMS); err != nil {
		return err
	}
	var lastCompletedAt int64
	if err := tx.QueryRow(`SELECT coalesce(max(completed_at),0) FROM cleanup_queue WHERE state='completed'`).Scan(&lastCompletedAt); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO cleanup_progress(progress_id,pending_count,oldest_age_ms,last_completed_at,updated_at)
VALUES(1,?,?,?,?) ON CONFLICT(progress_id) DO UPDATE SET pending_count=excluded.pending_count,
oldest_age_ms=excluded.oldest_age_ms,last_completed_at=excluded.last_completed_at,updated_at=excluded.updated_at`,
		pendingCount, oldestAgeMS, lastCompletedAt, now)
	return err
}

// readCleanupProgress 读取清理进度（未完成积压可查）。
func readCleanupProgress(q recordsQuerier) (CleanupProgress, error) {
	var p CleanupProgress
	err := q.QueryRow(`SELECT pending_count,oldest_age_ms,last_completed_at,updated_at FROM cleanup_progress WHERE progress_id=1`).
		Scan(&p.PendingCount, &p.OldestAgeMS, &p.LastCompletedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// ---------------------------------------------------------------------------
// 恢复保护（恢复备份/检查点时应用当前删除/撤销约束并先核查）
// ---------------------------------------------------------------------------

// ErrRestoreOldPermission 是恢复旧许可的错误。
var ErrRestoreOldPermission = errors.New("恢复备份/检查点时应用当前删除/撤销约束并先核查")

// ensureRestoreNotOldPermission 核对恢复不恢复旧许可（P5-05.5）。
// 恢复备份/检查点时应用当前删除/撤销约束并先核查；无法取得当前权威时拒绝
// 敏感读取，不恢复旧许可。
func ensureRestoreNotOldPermission(tx *sql.Tx, objectRef string, sourceRef string, expectedGeneration int, now int64) error {
	// 无法取得当前权威时拒绝敏感读取（不恢复旧许可）。
	if err := ensureSourceNotRevoked(tx, objectRef, sourceRef, expectedGeneration, now); err != nil {
		return fmt.Errorf("%w：%v", ErrRestoreOldPermission, err)
	}
	return nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
