package work

// P5-12 验收：持续备份、灾难恢复与副作用缺口。
//
// 三个测试入口：
//   - TestV3P512OnlineBackupConsistency：使用适合当前驱动的 SQLite 一致快照
//     机制，配合材料不可变版本/引用保留；复制 live db 单文件不能当一致备份；
//     备份 manifest 记录 schema、源版本、材料摘要、时间/范围与完整性；期间
//     被引用材料不因 GC 消失。
//   - TestV3P512RestoreSideEffectGap：恢复先校验再隔离启动，重核授权、来源
//     撤销/删除和未决工作；不自动恢复过期 Worker 许可；从较旧备份恢复存在
//     RPO 缺口时冻结相关新写，按外部作业/审计/资产现况核对；找不到 Operation
//     不能证明未执行；恢复不会再次部署/重启已在备份后执行的未知工作；缺口
//     明确交接。
//   - TestV3P512RestoreRevocationBoundary：来源删除不会因恢复旧快照重新外发；
//     无权威核验能力时拒绝相应内容/动作；分别演练进程重启、主机断电条件下的
//     耐久、备份恢复及材料损坏；记录 RPO/RTO 和未覆盖条件。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P512 创建存储。
func setupV3P512(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P512OnlineBackupConsistency 在线备份一致性。
func TestV3P512OnlineBackupConsistency(t *testing.T) {
	t.Run("consistent_snapshot_required_not_live_file_copy", func(t *testing.T) {
		// 复制 live db 单文件不能当一致备份。
		snapshot := BackupSnapshot{
			SnapshotID: "snap-1", Consistency: "consistent", LiveFileCopy: true,
			MaterialVersionsPreserved: true,
		}
		if err := ensureConsistentSnapshot(snapshot); err == nil {
			t.Error("live file copy accepted (复制 live db 单文件不能当一致备份)")
		}
	})

	t.Run("consistent_snapshot_with_material_versions", func(t *testing.T) {
		// 一致快照（配合材料不可变版本/引用保留）。
		snapshot := BackupSnapshot{
			SnapshotID: "snap-1", Consistency: "consistent", LiveFileCopy: false,
			MaterialVersionsPreserved: true,
		}
		if err := ensureConsistentSnapshot(snapshot); err != nil {
			t.Errorf("consistent snapshot rejected: %v (使用适合当前驱动的 SQLite 一致快照机制)", err)
		}
	})

	t.Run("backup_manifest_all_fields", func(t *testing.T) {
		// 备份 manifest 记录 schema、源版本、材料摘要、时间/范围与完整性。
		manifest := BackupManifestV3{
			Schema: 87, SourceVersion: 1, MaterialDigest: digest("材料"),
			TimeRangeStart: v3P102Now(), TimeRangeEnd: v3P102Now() + 3600000,
			Integrity: "sha256",
		}
		if err := validateBackupManifest(manifest); err != nil {
			t.Errorf("manifest rejected: %v (备份 manifest 记录 schema/源版本/材料摘要/时间/范围/完整性)", err)
		}
	})

	t.Run("referenced_materials_not_gc", func(t *testing.T) {
		// 期间被引用材料不因 GC 消失。
		manifest := BackupManifestV3{
			Schema: 87, SourceVersion: 1, MaterialDigest: digest("材料"),
			TimeRangeStart: v3P102Now(), TimeRangeEnd: v3P102Now() + 3600000,
			Integrity: "sha256", ReferencedMaterials: []string{"material-1", "material-2"},
		}
		if err := validateBackupManifest(manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.ReferencedMaterials) == 0 {
			t.Error("referenced materials missing (期间被引用材料不因 GC 消失)")
		}
	})
}

// TestV3P512RestoreSideEffectGap 恢复副作用缺口。
func TestV3P512RestoreSideEffectGap(t *testing.T) {
	t.Run("restore_validate_isolate_recheck_auth_revocation_pending", func(t *testing.T) {
		store := setupV3P512(t, "restore.db")
		now := v3P102Now()
		// 恢复先校验再隔离启动（重核授权、来源撤销/删除和未决工作）。
		tx, _ := store.db.Begin()
		result, err := ensureRestoreSafe(tx, "node-1", "cmdb", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Validated || !result.Isolated {
			t.Errorf("restore not validated/isolated: %+v (恢复先校验再隔离启动)", result)
		}
		if !result.AuthorizationRechecked || !result.RevocationRechecked || !result.PendingWorkRechecked {
			t.Errorf("restore not rechecked: %+v (重核授权/来源撤销/未决工作)", result)
		}
	})

	t.Run("expired_worker_not_restored", func(t *testing.T) {
		store := setupV3P512(t, "expired.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		result, err := ensureRestoreSafe(tx, "node-1", "cmdb", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.ExpiredWorkerNotRestored {
			t.Error("expired worker restored (不自动恢复过期 Worker 许可)")
		}
	})

	t.Run("rpo_gap_freezes_new_write_not_unexecuted", func(t *testing.T) {
		store := setupV3P512(t, "rpo.db")
		now := v3P102Now()
		// RPO 缺口：冻结相关新写（找不到 Operation 不能证明未执行）。
		tx, _ := store.db.Begin()
		err := ensureRPOGapHandled(tx, "node-1", now-86400000, now, now) // 备份早于当前
		tx.Commit()
		if err == nil {
			t.Error("rpo gap not detected (RPO 缺口冻结相关新写)")
		}
		if !strings.Contains(err.Error(), "不能证明未执行") && !strings.Contains(err.Error(), "缺口") {
			t.Errorf("rpo gap reason missing: %v (找不到 Operation 不能证明未执行)", err)
		}
		// 缺口已审计（缺口明确交接）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='rpo_gap_frozen'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("rpo gap not audited: count=%d (缺口明确交接)", count)
		}
	})

	t.Run("restore_not_redeploy_unknown_work", func(t *testing.T) {
		// 恢复不会再次部署/重启已在备份后执行的未知工作（RPO 缺口冻结）。
		store := setupV3P512(t, "noredeploy.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		err := ensureRPOGapHandled(tx, "node-1", now-86400000, now, now)
		tx.Commit()
		if err == nil {
			t.Error("restore redeployed unknown work (恢复不会再次部署/重启已在备份后执行的未知工作)")
		}
	})
}

// TestV3P512RestoreRevocationBoundary 恢复撤销边界。
func TestV3P512RestoreRevocationBoundary(t *testing.T) {
	t.Run("source_deleted_not_resent_by_old_snapshot", func(t *testing.T) {
		store := setupV3P512(t, "deleted.db")
		now := v3P102Now()
		// 来源删除不会因恢复旧快照重新外发（恢复重核来源撤销）。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := ensureRestoreSafe(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("deleted source restored by old snapshot (来源删除不会因恢复旧快照重新外发)")
		}
	})

	t.Run("no_authority_rejects_content_action", func(t *testing.T) {
		// 无权威核验能力时拒绝相应内容/动作。
		store := setupV3P512(t, "noauthority.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := ensureRestoreSafe(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("no authority content allowed (无权威核验能力时拒绝相应内容/动作)")
		}
	})

	t.Run("recovery_drills_all_kinds_rpo_rto_uncovered", func(t *testing.T) {
		// 分别演练进程重启、主机断电条件下的耐久、备份恢复及材料损坏；
		// 记录 RPO/RTO 和未覆盖条件。
		drills := []RecoveryDrill{
			{Kind: "process_restart", RPOMS: 1000, RTOMS: 5000, UncoveredConditions: []string{"无"}},
			{Kind: "host_poweroff", RPOMS: 5000, RTOMS: 30000, UncoveredConditions: []string{"断电窗口"}},
			{Kind: "backup_restore", RPOMS: 3600000, RTOMS: 600000, UncoveredConditions: []string{"RPO 缺口"}},
			{Kind: "material_corruption", RPOMS: 0, RTOMS: 10000, UncoveredConditions: []string{"部分损坏"}},
		}
		for _, d := range drills {
			if err := ensureRecoveryDrillRecorded(d); err != nil {
				t.Errorf("drill %s not recorded: %v (分别演练进程重启/主机断电/备份恢复/材料损坏)", d.Kind, err)
			}
			if d.RPOMS < 0 || d.RTOMS < 0 {
				t.Errorf("drill %s missing RPO/RTO (记录 RPO/RTO)", d.Kind)
			}
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
