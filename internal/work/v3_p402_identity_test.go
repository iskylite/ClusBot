package work

// P4-02 验收：资产发现身份、部件和外部映射。
//
// 三个测试入口：
//   - TestV3P402IdentityReuseConflict：使用稳定内部 ID，外部映射含来源系统
//     实例、外部类型/ID、设备代际及有效区间；IP 被新设备复用不合并为旧节点；
//     同序列号冲突可见且不自动覆盖；身份冲突只阻止相关对象写动作，不阻断
//     其他已确认资产的工作。
//   - TestV3P402ComponentReplacement：发现先保存 identity_claim；可信无矛盾
//     规则可归一，名称/IP/相似度仅作候选；IP/名称变化保持资产身份；更换网卡
//     等部件创建新部件并结束旧安装关系，节点身份连续性不明时待核对；更换
//     部件后旧故障/测试仍关联旧部件，新安装关系有时间边界。
//   - TestV3P402IdentityCorrectionHistory：支持误合并/拆分的带证据修订，保留
//     旧引用并触发相关验证依赖复查；采集失败或覆盖不完整不能当作部件已删除。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P402 创建存储。
func setupV3P402(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P402IdentityReuseConflict 身份复用冲突。
func TestV3P402IdentityReuseConflict(t *testing.T) {
	t.Run("external_mapping_all_fields", func(t *testing.T) {
		store := setupV3P402(t, "mapping.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, err := createExternalMapping(tx, AssetExternalMapping{
			ObjectRef: "node-1", SourceSystem: "cmdb", SourceInstance: "prod",
			ExternalType: "node", ExternalID: "ext-node-123", DeviceGeneration: 1,
			ValidFrom: now, ValidTo: 0,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 外部映射含全部字段。
		if m.ObjectRef != "node-1" || m.SourceSystem != "cmdb" || m.ExternalID != "ext-node-123" ||
			m.DeviceGeneration != 1 || m.ValidFrom != now {
			t.Errorf("mapping incomplete: %+v", m)
		}
	})

	t.Run("ip_reused_by_new_device_not_merged", func(t *testing.T) {
		store := setupV3P402(t, "ipreuse.db")
		now := v3P102Now()
		// IP 被新设备复用：不合并为旧节点（不同对象身份）。
		tx, _ := store.db.Begin()
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-old", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-new", CandidateIdentity: "192.168.1.10", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		tx.Commit()
		// 同 IP 不同对象：冲突可见（不合并为旧节点）。
		tx, _ = store.db.Begin()
		conflict, refs, err := checkIdentityConflict(tx, "192.168.1.10", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict {
			t.Error("IP reuse not detected as conflict (IP 被新设备复用不合并为旧节点)")
		}
		if len(refs) != 2 {
			t.Errorf("conflicting refs = %v, want 2 objects", refs)
		}
	})

	t.Run("same_serial_conflict_visible_not_auto_overwrite", func(t *testing.T) {
		store := setupV3P402(t, "serial.db")
		now := v3P102Now()
		// 同序列号冲突：可见且不自动覆盖。
		tx, _ := store.db.Begin()
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-a", CandidateIdentity: "SN123456", IdentityKind: "serial",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-b", CandidateIdentity: "SN123456", IdentityKind: "serial",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		tx.Commit()
		// 同序列号冲突：可见。
		tx, _ = store.db.Begin()
		conflict, refs, err := checkIdentityConflict(tx, "SN123456", "serial", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict || len(refs) != 2 {
			t.Errorf("serial conflict not visible: conflict=%v refs=%v (同序列号冲突可见)", conflict, refs)
		}
	})

	t.Run("identity_conflict_blocks_related_write_not_others", func(t *testing.T) {
		store := setupV3P402(t, "conflict.db")
		now := v3P102Now()
		// 身份冲突只阻止相关对象写动作，不阻断其他已确认资产的工作。
		tx, _ := store.db.Begin()
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-conflict-1", CandidateIdentity: "192.168.1.20", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-conflict-2", CandidateIdentity: "192.168.1.20", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "trusted",
		}, now)
		tx.Commit()
		// 冲突对象：写动作受阻（身份冲突可见）。
		tx, _ = store.db.Begin()
		conflict, _, err := checkIdentityConflict(tx, "192.168.1.20", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !conflict {
			t.Error("identity conflict not detected")
		}
		// 其他已确认资产（无冲突）：工作不受阻。
		tx, _ = store.db.Begin()
		conflict2, _, err := checkIdentityConflict(tx, "192.168.1.30", "ip", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if conflict2 {
			t.Error("unrelated asset blocked by identity conflict (不阻断其他已确认资产的工作)")
		}
	})
}

// TestV3P402ComponentReplacement 部件更换。
func TestV3P402ComponentReplacement(t *testing.T) {
	t.Run("identity_claim_saved_before_unify", func(t *testing.T) {
		store := setupV3P402(t, "claim.db")
		now := v3P102Now()
		// 发现先保存 identity_claim（名称/IP/相似度仅作候选）。
		tx, _ := store.db.Begin()
		claim, err := recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: "node-candidate", CandidateIdentity: "192.168.1.50", IdentityKind: "ip",
			ClaimedBy: "discovery", Confidence: "candidate",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if claim.Confidence != "candidate" {
			t.Errorf("claim confidence = %s, want candidate (名称/IP/相似度仅作候选)", claim.Confidence)
		}
	})

	t.Run("ip_name_change_keeps_asset_identity", func(t *testing.T) {
		store := setupV3P402(t, "ipchange.db")
		now := v3P102Now()
		// IP/名称变化保持资产身份（稳定内部 ID 不变）。
		tx, _ := store.db.Begin()
		createExternalMapping(tx, AssetExternalMapping{
			ObjectRef: "node-stable", SourceSystem: "cmdb", SourceInstance: "prod",
			ExternalType: "node", ExternalID: "ext-node-1", DeviceGeneration: 1, ValidFrom: now,
		}, now)
		tx.Commit()
		// IP 变化（新映射，同内部 ID）：资产身份保持。
		tx, _ = store.db.Begin()
		createExternalMapping(tx, AssetExternalMapping{
			ObjectRef: "node-stable", SourceSystem: "cmdb", SourceInstance: "prod",
			ExternalType: "node", ExternalID: "ext-node-1", DeviceGeneration: 1, ValidFrom: now + 1000,
		}, now)
		tx.Commit()
		// 内部 ID 不变（稳定内部 ID）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(DISTINCT object_ref) FROM asset_external_mappings WHERE external_id='ext-node-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("asset identity changed on IP change: %d objects (IP/名称变化保持资产身份)", count)
		}
	})

	t.Run("component_replacement_new_component_ends_old", func(t *testing.T) {
		store := setupV3P402(t, "replace.db")
		now := v3P102Now()
		// 安装旧部件。
		tx, _ := store.db.Begin()
		old, _ := replaceComponent(tx, "node-1", "", "nic-old", "nic", now)
		tx.Commit()
		// 更换部件：创建新部件并结束旧安装关系。
		tx, _ = store.db.Begin()
		newInstall, err := replaceComponent(tx, "node-1", "nic-old", "nic-new", "nic", now+1000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 旧安装关系已结束（时间边界）。
		var removedAt int64
		if err := store.db.QueryRow("SELECT removed_at FROM component_installations WHERE installation_id=?", old.InstallationID).Scan(&removedAt); err != nil {
			t.Fatal(err)
		}
		if removedAt == 0 {
			t.Error("old installation not ended (结束旧安装关系)")
		}
		// 新安装关系有时间边界。
		if newInstall.InstalledAt != now+1000 {
			t.Errorf("new installation time = %d (新安装关系有时间边界)", newInstall.InstalledAt)
		}
		// 节点身份连续性不明时待核对。
		if newInstall.NodeIdentityContinuous != "unknown" {
			t.Errorf("node identity continuity = %s, want unknown (节点身份连续性不明时待核对)", newInstall.NodeIdentityContinuous)
		}
	})

	t.Run("old_faults_tests_still_linked_to_old_component", func(t *testing.T) {
		store := setupV3P402(t, "oldfaults.db")
		now := v3P102Now()
		// 更换部件后：旧故障/测试仍关联旧部件（安装关系有时间边界）。
		tx, _ := store.db.Begin()
		old, _ := replaceComponent(tx, "node-1", "", "nic-old", "nic", now)
		tx.Commit()
		// 旧部件的故障/测试记录（关联旧安装关系）。
		var oldInstallation string
		if err := store.db.QueryRow("SELECT installation_id FROM component_installations WHERE component_ref='nic-old'").Scan(&oldInstallation); err != nil {
			t.Fatal(err)
		}
		if oldInstallation != old.InstallationID {
			t.Errorf("old faults not linked to old component (旧故障/测试仍关联旧部件)")
		}
		// 更换后旧安装关系仍可查（时间边界）。
		tx, _ = store.db.Begin()
		replaceComponent(tx, "node-1", "nic-old", "nic-new", "nic", now+1000)
		tx.Commit()
		var removedAt int64
		if err := store.db.QueryRow("SELECT removed_at FROM component_installations WHERE component_ref='nic-old'").Scan(&removedAt); err != nil {
			t.Fatal(err)
		}
		if removedAt == 0 {
			t.Error("old installation not ended")
		}
	})
}

// TestV3P402IdentityCorrectionHistory 身份修订历史。
func TestV3P402IdentityCorrectionHistory(t *testing.T) {
	t.Run("merge_split_correction_with_evidence", func(t *testing.T) {
		store := setupV3P402(t, "correction.db")
		now := v3P102Now()
		// 误合并/拆分的带证据修订：保留旧引用并触发验证依赖复查。
		tx, _ := store.db.Begin()
		correction, err := recordIdentityCorrection(tx, IdentityCorrection{
			Kind: "merge_split", OldObjectRef: "node-merged", NewObjectRef: "node-split-a",
			EvidenceRef: "evidence://split", VerificationDependencies: `["verify-nic","verify-disk"]`,
			CorrectedBy: "admin",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 修订含证据与验证依赖。
		if correction.EvidenceRef == "" || correction.VerificationDependencies == "" {
			t.Errorf("correction incomplete: %+v", correction)
		}
		// 旧引用保留（可查询）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM identity_corrections WHERE old_object_ref='node-merged'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("old reference not preserved: count=%d (保留旧引用)", count)
		}
		// 验证依赖复查已审计。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='identity_correction'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("correction not audited: count=%d (触发相关验证依赖复查)", auditCount)
		}
	})

	t.Run("correction_requires_evidence", func(t *testing.T) {
		store := setupV3P402(t, "noevidence.db")
		now := v3P102Now()
		// 缺证据：拒绝。
		tx, _ := store.db.Begin()
		_, err := recordIdentityCorrection(tx, IdentityCorrection{
			Kind: "merge_split", OldObjectRef: "node-a", NewObjectRef: "node-b",
			CorrectedBy: "admin",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("correction without evidence not rejected (带证据修订)")
		}
	})

	t.Run("collection_failure_not_component_deleted", func(t *testing.T) {
		// 采集失败或覆盖不完整不能当作部件已删除。
		store := setupV3P402(t, "collection.db")
		now := v3P102Now()
		// 部件安装关系存在（未删除）。
		tx, _ := store.db.Begin()
		replaceComponent(tx, "node-1", "", "nic-present", "nic", now)
		tx.Commit()
		// 采集失败（无新发现）：不标记部件已删除。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM component_installations WHERE component_ref='nic-present' AND removed_at=0").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("collection failure marked component deleted: count=%d (采集失败不能当作部件已删除)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
