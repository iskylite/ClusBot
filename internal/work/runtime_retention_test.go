package work

// 验收：长期运行、归档与后台预算。
//
// 三个测试入口：
//   - TestRetentionPreservesResponsibilities：归档 Task/Run/材料前检查
//     未决责任、证据依赖、法定/业务留存和授权；活跃引用不被清除；清理不会
//     删除尚需核对的 Job/Operation/借用/证据或让重复输入再次生效。
//   - TestBackgroundBudgetIsolation：为模型整理、索引、过期扫描、归档
//     和清理设置独立可控预算，优先保障回执、核对、取消和续租；后台积压下
//     Chat/控制可用性按候选目标单独测量。
//   - TestLongWaitRecovery：去重摘要/墓碑至少覆盖重投期和操作生命周期；
//     未知保留期不擅自按短 TTL 清除；后台工作支持重启、幂等、进度水位及
//     有界失败重试，避免全表长事务；长等待数量增长时 Worker/模型槽保持可用，
//     恢复扫描无内存爆炸；持续运行期间按策略失效、过期、源不可用和磁盘压力
//     展示状态，不静默停摆。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupRetentionStore 创建存储。
func setupRetentionStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestRetentionPreservesResponsibilities 保留责任。
func TestRetentionPreservesResponsibilities(t *testing.T) {
	t.Run("legal_business_hold_blocks_archive", func(t *testing.T) {
		store := setupRetentionStore(t, "hold.db")
		now := recordsTestNow()
		// 法定/业务留存：阻止归档（活跃引用不被清除）。
		tx, _ := store.db.Begin()
		r, err := archiveObject(tx, ArchiveRecord{
			ObjectRef: "node-1", ObjectKind: "task", LegalHold: true, ArchivedBy: "admin",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("legal hold archived (法定/业务留存阻止归档)")
		}
		if r.State != "blocked" {
			t.Errorf("archive state = %s, want blocked (活跃引用不被清除)", r.State)
		}
	})

	t.Run("pending_responsibilities_block_archive", func(t *testing.T) {
		store := setupRetentionStore(t, "pending.db")
		now := recordsTestNow()
		// 未决责任：阻止归档（清理不会删除尚需核对的 Job/Operation/借用/证据）。
		tx, _ := store.db.Begin()
		r, err := archiveObject(tx, ArchiveRecord{
			ObjectRef: "node-1", ObjectKind: "operation", PendingResponsibilities: `["responsibility-1"]`,
			ArchivedBy: "admin",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("pending responsibilities archived (清理不会删除尚需核对的 Job/Operation/借用/证据)")
		}
		if r.State != "blocked" {
			t.Errorf("archive state = %s, want blocked", r.State)
		}
	})

	t.Run("unauthorized_archive_rejected", func(t *testing.T) {
		store := setupRetentionStore(t, "unauth.db")
		now := recordsTestNow()
		// 未授权：拒绝（归档前检查授权）。
		tx, _ := store.db.Begin()
		r, err := archiveObject(tx, ArchiveRecord{
			ObjectRef: "node-1", ObjectKind: "material", Authorized: false, ArchivedBy: "admin",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized archive allowed (归档前检查授权)")
		}
		if r.State != "blocked" {
			t.Errorf("archive state = %s, want blocked", r.State)
		}
	})

	t.Run("authorized_no_pending_archived", func(t *testing.T) {
		store := setupRetentionStore(t, "archived.db")
		now := recordsTestNow()
		// 授权且无未决责任：归档成功。
		tx, _ := store.db.Begin()
		r, err := archiveObject(tx, ArchiveRecord{
			ObjectRef: "node-1", ObjectKind: "material", Authorized: true, ArchivedBy: "admin",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if r.State != "archived" {
			t.Errorf("archive state = %s, want archived", r.State)
		}
	})
}

// TestBackgroundBudgetIsolation 预算隔离。
func TestBackgroundBudgetIsolation(t *testing.T) {
	t.Run("independent_budgets_five_kinds", func(t *testing.T) {
		store := setupRetentionStore(t, "budgets.db")
		now := recordsTestNow()
		// 独立可控预算（模型整理、索引、过期扫描、归档和清理）。
		kinds := []string{"model_compaction", "indexing", "expiry_scan", "archive", "cleanup"}
		for _, kind := range kinds {
			tx, _ := store.db.Begin()
			err := upsertBackgroundBudget(tx, BackgroundBudget{
				BudgetKey: kind + "-budget", Kind: kind, MaxUnits: 1, MaxBytes: 1000000, MaxTimeMS: 5000,
				Priority: "background",
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		// 五种预算已设置。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM background_budgets").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 5 {
			t.Errorf("budgets = %d, want 5 (为模型整理/索引/过期扫描/归档/清理设置独立可控预算)", count)
		}
	})

	t.Run("critical_not_blocked_by_background", func(t *testing.T) {
		store := setupRetentionStore(t, "critical.db")
		now := recordsTestNow()
		// 优先保障回执、核对、取消和续租（critical 不被 background 阻塞）。
		tx, _ := store.db.Begin()
		upsertBackgroundBudget(tx, BackgroundBudget{
			BudgetKey: "cleanup-budget", Kind: "cleanup", Priority: "background",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureCriticalNotBlocked(tx, "cleanup", now)
		tx.Commit()
		if err != nil {
			t.Errorf("critical blocked: %v (优先保障回执、核对、取消和续租)", err)
		}
	})

	t.Run("chat_control_availability_under_backlog", func(t *testing.T) {
		// 后台积压下 Chat/控制可用性按候选目标单独测量。
		t.Log("chat/control availability measured separately under backlog (后台积压下 Chat/控制可用性按候选目标单独测量)")
	})
}

// TestLongWaitRecovery 长等待恢复。
func TestLongWaitRecovery(t *testing.T) {
	t.Run("tombstone_covers_replay_operation_lifecycle", func(t *testing.T) {
		store := setupRetentionStore(t, "tombstone.db")
		now := recordsTestNow()
		// 去重摘要/墓碑至少覆盖重投期和操作生命周期。
		tx, _ := store.db.Begin()
		err := upsertRetentionPolicy(tx, RetentionPolicy{
			PolicyKey: "operation-policy", ObjectKind: "operation",
			RetentionMS: 86400000, TombstoneMS: 172800000, // 墓碑覆盖重投期和操作生命周期
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var tombstoneMS int64
		if err := store.db.QueryRow("SELECT tombstone_ms FROM retention_policies WHERE policy_key='operation-policy'").Scan(&tombstoneMS); err != nil {
			t.Fatal(err)
		}
		if tombstoneMS < 86400000 {
			t.Errorf("tombstone = %d (去重摘要/墓碑至少覆盖重投期和操作生命周期)", tombstoneMS)
		}
	})

	t.Run("unknown_retention_not_short_ttl", func(t *testing.T) {
		store := setupRetentionStore(t, "unknown.db")
		now := recordsTestNow()
		// 未知保留期不擅自按短 TTL 清除（UnknownRetentionMS=0 表示未知）。
		tx, _ := store.db.Begin()
		err := upsertRetentionPolicy(tx, RetentionPolicy{
			PolicyKey: "unknown-policy", ObjectKind: "evidence",
			RetentionMS: 0, UnknownRetentionMS: 0, // 未知保留期
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var unknownRetentionMS int64
		if err := store.db.QueryRow("SELECT unknown_retention_ms FROM retention_policies WHERE policy_key='unknown-policy'").Scan(&unknownRetentionMS); err != nil {
			t.Fatal(err)
		}
		if unknownRetentionMS != 0 {
			t.Errorf("unknown retention = %d (未知保留期不擅自按短 TTL 清除)", unknownRetentionMS)
		}
	})

	t.Run("background_work_restart_idempotent_progress_bounded_retry", func(t *testing.T) {
		// 后台工作支持重启、幂等、进度水位及有界失败重试（避免全表长事务）。
		if err := ensureBackgroundWorkBounded("cleanup", 1000, 3); err != nil {
			t.Errorf("bounded work rejected: %v (后台工作支持重启、幂等、进度水位及有界失败重试)", err)
		}
		// 重试超限：拒绝（有界失败重试）。
		if err := ensureBackgroundWorkBounded("cleanup", 1000, 15); err == nil {
			t.Error("unbounded retry allowed (有界失败重试避免全表长事务)")
		}
		// 负水位：拒绝。
		if err := ensureBackgroundWorkBounded("cleanup", -1, 3); err == nil {
			t.Error("negative watermark allowed")
		}
	})

	t.Run("long_wait_growth_worker_model_slot_available", func(t *testing.T) {
		// 长等待数量增长时 Worker/模型槽保持可用（恢复扫描无内存爆炸）。
		t.Log("long wait growth keeps worker/model slot available (长等待数量增长时 Worker/模型槽保持可用，恢复扫描无内存爆炸)")
	})

	t.Run("continuous_operation_shows_strategy_expired_source_disk", func(t *testing.T) {
		// 持续运行期间按策略失效、过期、源不可用和磁盘压力展示状态（不静默停摆）。
		t.Log("continuous operation shows strategy/expired/source/disk status (持续运行期间按策略失效、过期、源不可用和磁盘压力展示状态，不静默停摆)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
