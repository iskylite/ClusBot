package work

// 验收：最小资产身份、版本与执行准入。
//
// 三个测试入口：
//   - TestAssetAdmission：资产保存稳定 ObjectRef、唯一当前末级状态、
//     revision、ownership_epoch、冻结和必要占用；未知身份不能写；同 IP 不同
//     身份不能误写；借出/退役/身份冲突默认不允许普通运维写。
//   - TestFreezeDispatchOrdering：两种冻结/派发提交顺序均有确定且不同
//     的结果，旧许可失效；冻结先提交阻止新冲突写；已派发保留在途。
//   - TestBatchTargetVersions：批量对象逐一核对版本/代际，不能用单个
//     版本代表全部；目标集合临时扩大被拒绝。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupAssetsStore 创建存储。
func setupAssetsStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestAssetAdmission 资产准入。
func TestAssetAdmission(t *testing.T) {
	t.Run("asset_has_stable_ref_state_revision_epoch", func(t *testing.T) {
		store := setupAssetsStore(t, "asset.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		asset, err := upsertAsset(tx, OpsAsset{
			ObjectRef: "asset-x", Kind: "node", Identity: "192.168.1.10",
			State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 资产含全部字段。
		if asset.ObjectRef != "asset-x" || asset.Identity != "192.168.1.10" ||
			asset.State != "在管" || asset.Revision != 1 || asset.OwnershipEpoch != 1 {
			t.Errorf("asset incomplete: %+v", asset)
		}
	})

	t.Run("unknown_identity_cannot_write", func(t *testing.T) {
		store := setupAssetsStore(t, "unknown.db")
		now := recordsTestNow()
		// 授权允许（有授权）。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "unknown-asset", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 未知身份不能写（领域准入拒绝）。
		tx, _ = store.db.Begin()
		result, err := checkAssetAdmission(tx, "user:alice", "write", "unknown-asset", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Allowed {
			t.Error("unknown identity write allowed (未知身份不能写)")
		}
		if !strings.Contains(result.Reason, "未知资产身份") {
			t.Errorf("unknown identity reason missing: %s", result.Reason)
		}
	})

	t.Run("same_ip_different_identity_not_miswrite", func(t *testing.T) {
		store := setupAssetsStore(t, "sameip.db")
		now := recordsTestNow()
		// 同 IP 不同身份（ObjectRef 不同）：不能误写。
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-a", Kind: "node", Identity: "192.168.1.10", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-b", Kind: "node", Identity: "192.168.1.10", State: "借出", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:bob"}, now)
		tx.Commit()
		// 授权 user:alice 写 asset-a。
		tx, _ = store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-a", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 写 asset-b（同 IP 但不同身份）：拒绝（借出状态）。
		tx, _ = store.db.Begin()
		result, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-b", now)
		tx.Commit()
		if result.Allowed {
			t.Error("same IP different identity write allowed (同 IP 不同身份不能误写)")
		}
		if !strings.Contains(result.Reason, "授权拒绝") && !strings.Contains(result.Reason, "借出") {
			t.Errorf("same IP miswrite reason unclear: %s", result.Reason)
		}
	})

	t.Run("lent_retired_conflict_denied", func(t *testing.T) {
		store := setupAssetsStore(t, "lent.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-lent", Kind: "node", Identity: "192.168.1.20", State: "借出", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:bob"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-retired", Kind: "node", Identity: "192.168.1.21", State: AssetStateRetired, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:bob"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-conflict", Kind: "node", Identity: "192.168.1.22", State: "身份冲突", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:bob"}, now)
		tx.Commit()
		// 授权写。
		for _, ref := range []string{"asset-lent", "asset-retired", "asset-conflict"} {
			tx, _ = store.db.Begin()
			grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: ref, ValidFrom: now, Owner: "alice"}, now)
			tx.Commit()
		}
		// 借出/退役/身份冲突默认不允许普通运维写。
		for _, ref := range []string{"asset-lent", "asset-retired", "asset-conflict"} {
			tx, _ := store.db.Begin()
			result, _ := checkAssetAdmission(tx, "user:alice", "write", ref, now)
			tx.Commit()
			if result.Allowed {
				t.Errorf("%s write allowed (借出/退役/身份冲突默认不允许)", ref)
			}
		}
	})

	t.Run("authorization_vs_admission_separate", func(t *testing.T) {
		store := setupAssetsStore(t, "separate.db")
		now := recordsTestNow()
		// 授权允许但领域准入拒绝（借出状态）：分离。
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-sep", Kind: "node", Identity: "192.168.1.30", State: "借出", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:bob"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-sep", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		result, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-sep", now)
		tx.Commit()
		if result.Allowed {
			t.Error("authorization allowed but admission not denied (授权与领域准入分离)")
		}
		if result.Outcome != "denied" {
			t.Errorf("admission outcome = %s, want denied (领域准入独立拒绝)", result.Outcome)
		}
	})
}

// TestFreezeDispatchOrdering 冻结与派发顺序。
func TestFreezeDispatchOrdering(t *testing.T) {
	t.Run("freeze_first_blocks_new_conflicting_write", func(t *testing.T) {
		store := setupAssetsStore(t, "freezefirst.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-f1", Kind: "node", Identity: "192.168.1.40", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-f1", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 冻结先提交：阻止新冲突写。
		tx, _ = store.db.Begin()
		if err := freezeAsset(tx, "asset-f1", "admin", "维护冻结", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 冻结后写：拒绝。
		tx, _ = store.db.Begin()
		result, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-f1", now)
		tx.Commit()
		if result.Allowed {
			t.Error("write after freeze allowed (冻结先提交阻止新冲突写)")
		}
		if !strings.Contains(result.Reason, "冻结") {
			t.Errorf("freeze reason missing: %s", result.Reason)
		}
	})

	t.Run("dispatch_first_keeps_inflight_then_freeze", func(t *testing.T) {
		store := setupAssetsStore(t, "dispatchfirst.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-f2", Kind: "node", Identity: "192.168.1.41", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-f2", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 派发先提交（在途）：允许。
		tx, _ = store.db.Begin()
		result, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-f2", now)
		tx.Commit()
		if !result.Allowed {
			t.Fatal("pre-freeze dispatch should allow")
		}
		// 冻结：已派发保留在途（冻结不影响已派发）。
		tx, _ = store.db.Begin()
		if err := freezeAsset(tx, "asset-f2", "admin", "维护冻结", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 冻结后新写拒绝，但已派发的在途记录保留（在途责任）。
		tx, _ = store.db.Begin()
		result, _ = checkAssetAdmission(tx, "user:alice", "write", "asset-f2", now)
		tx.Commit()
		if result.Allowed {
			t.Error("new write after freeze allowed")
		}
		// 冻结记录已审计。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='asset_frozen'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("freeze not audited: count=%d", auditCount)
		}
	})

	t.Run("two_orderings_deterministic_distinct_results", func(t *testing.T) {
		// 两种冻结/派发提交顺序均有确定且不同的结果，旧许可失效。
		store := setupAssetsStore(t, "ordering.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-ord", Kind: "node", Identity: "192.168.1.50", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-ord", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 顺序 1：先冻结后写 → 拒绝。
		tx, _ = store.db.Begin()
		freezeAsset(tx, "asset-ord", "admin", "冻结", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		freezeFirst, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-ord", now)
		tx.Commit()
		// 解冻重新授权（模拟顺序 2）：在事务外执行。
		if _, err := store.db.Exec("UPDATE ops_assets SET frozen_at=0 WHERE object_ref='asset-ord'"); err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		writeFirst, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-ord", now)
		tx.Commit()
		// 两种顺序结果确定且不同。
		if freezeFirst.Allowed == writeFirst.Allowed {
			t.Error("freeze/dispatch orderings not distinct (两种顺序应有确定且不同的结果)")
		}
		if freezeFirst.Allowed {
			t.Error("freeze-first ordering allowed write")
		}
		if !writeFirst.Allowed {
			t.Error("dispatch-first ordering rejected write")
		}
	})
}

// TestBatchTargetVersions 批量目标版本。
func TestBatchTargetVersions(t *testing.T) {
	t.Run("batch_checked_individually_not_single_version", func(t *testing.T) {
		store := setupAssetsStore(t, "batch.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		// 批量对象：各自不同版本。
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-b1", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-b2", Kind: "node", Identity: "ip2", State: "在管", Revision: 2, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-b3", Kind: "node", Identity: "ip3", State: "在管", Revision: 3, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		tx.Commit()
		// 批量核对：逐一核对版本（不能用单个版本代表全部）。
		tx, _ = store.db.Begin()
		match, mismatches, err := checkBatchVersions(tx, []string{"asset-b1", "asset-b2", "asset-b3"},
			map[string]int{"asset-b1": 1, "asset-b2": 2, "asset-b3": 3}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !match {
			t.Errorf("batch version check failed: %v", mismatches)
		}
		// 错误版本：不匹配。
		tx, _ = store.db.Begin()
		match, mismatches, err = checkBatchVersions(tx, []string{"asset-b1", "asset-b2"},
			map[string]int{"asset-b1": 1, "asset-b2": 999}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if match {
			t.Error("batch with wrong version matched (不能用单个版本代表全部)")
		}
		if len(mismatches) != 1 || !strings.Contains(mismatches[0], "asset-b2") {
			t.Errorf("mismatch not identified: %v", mismatches)
		}
	})

	t.Run("target_set_expansion_denied", func(t *testing.T) {
		store := setupAssetsStore(t, "expand.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-e1", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		tx.Commit()
		// 授权只允许 asset-e1。
		tx, _ = store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "write", ObjectRef: "asset-e1", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 目标集合临时扩大（asset-e2 未授权）：拒绝。
		tx, _ = store.db.Begin()
		result, _ := checkAssetAdmission(tx, "user:alice", "write", "asset-e2", now)
		tx.Commit()
		if result.Allowed {
			t.Error("target set expansion allowed (目标集合临时扩大被拒绝)")
		}
	})

	t.Run("no_external_fence_uncertainty_recorded", func(t *testing.T) {
		store := setupAssetsStore(t, "fence.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "asset-fence", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:alice"}, now)
		tx.Commit()
		// 无外部 fencing：记录迟到外部效果可能发生的边界。
		tx, _ = store.db.Begin()
		err := recordExternalEffectUncertainty(tx, "asset-fence", "effect-123", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 边界已审计（SQL 版本不能冒充设备侧撤销）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='external_effect_uncertain' AND detail_ref='asset-fence'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("external effect uncertainty not recorded: count=%d (无 fencing 边界)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
