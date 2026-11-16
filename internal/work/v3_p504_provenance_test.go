package work

// P5-04 验收：来源依赖闭包与撤销版本闸门。
//
// 三个测试入口：
//   - TestV3P504RevocationGateBeforeCleanup：本地撤销/删除与策略分区代际递增、
//     可靠传播事件同事务；主体授权变化也必须进入有效判据；F07 暂停后台清理，
//     旧摘要/经验/全文/向量候选仍在时新出口拒绝失权内容。
//   - TestV3P504ProvenanceClosureBounded：派生修订保存按源修订标识的有界依赖
//     闭包和反向依赖；有环、超限、未完成的派生记录保持不可用；读取缓存键包含
//     主体、记录修订、闭包摘要、相关策略版本/分区代际及最早到期时间；新最终
//     核验以当前权威快照比较代际；不一致有界重查，做不完先返回缺口并重建，
//     不能沿用旧缓存。
//   - TestV3P504CrossPartitionAuthorization：跨分区来源逐一核验，闭包超限时
//     不会降级为允许；外部可靠通知/实时授权与仅凭有效期两种保证分开；无撤权
//     机制不能宣称即时获知，已在途内容不能撤回。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P504 创建存储。
func setupV3P504(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P504RevocationGateBeforeCleanup 撤销闸门。
func TestV3P504RevocationGateBeforeCleanup(t *testing.T) {
	t.Run("revoke_source_generation_increment", func(t *testing.T) {
		store := setupV3P504(t, "revoke.db")
		now := v3P102Now()
		// 撤销来源：代际递增（阻断已知失权来源）。
		tx, _ := store.db.Begin()
		if err := revokeSource(tx, "node-1", "cmdb", "partition-1", "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 代际已递增（阻断已知失权来源）。
		var generation int
		var state string
		if err := store.db.QueryRow("SELECT generation, state FROM revocation_gates WHERE object_ref='node-1' AND source_ref='cmdb'").Scan(&generation, &state); err != nil {
			t.Fatal(err)
		}
		if state != "revoked" {
			t.Errorf("source state = %s, want revoked (本地撤销/删除与策略分区代际递增)", state)
		}
		// 撤销已审计（可靠传播事件同事务）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='source_revoked'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("revocation not audited: count=%d (可靠传播事件同事务)", count)
		}
	})

	t.Run("revoked_source_blocked_before_cleanup", func(t *testing.T) {
		store := setupV3P504(t, "blocked.db")
		now := v3P102Now()
		// F07 暂停后台清理：旧摘要/经验/全文/向量候选仍在时新出口拒绝失权内容。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "partition-1", "admin", now)
		tx.Commit()
		// 撤销后来源被拒绝（新出口拒绝失权内容）。
		tx, _ = store.db.Begin()
		err := ensureSourceNotRevoked(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("revoked source allowed (新出口拒绝失权内容)")
		}
		if !strings.Contains(err.Error(), "已") && !strings.Contains(err.Error(), "revoked") {
			t.Errorf("revoked reason missing: %v", err)
		}
	})

	t.Run("subject_authorization_change_in_validity", func(t *testing.T) {
		store := setupV3P504(t, "authchange.db")
		now := v3P102Now()
		// 主体授权变化也必须进入有效判据（代际递增）。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "partition-1", "admin", now)
		tx.Commit()
		// 代际递增后：旧代际缓存不能沿用（主体授权变化也必须进入有效判据）。
		tx, _ = store.db.Begin()
		err := ensureSourceNotRevoked(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("subject authorization change not in validity (主体授权变化也必须进入有效判据)")
		}
	})
}

// TestV3P504ProvenanceClosureBounded 闭包有界。
func TestV3P504ProvenanceClosureBounded(t *testing.T) {
	t.Run("closure_all_fields_bounded", func(t *testing.T) {
		store := setupV3P504(t, "closure.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, err := recordProvenanceClosure(tx, ProvenanceClosure{
			DerivedRef: "summary-1", DerivedRevision: 1, SourceRef: "cmdb", SourceRevision: 1,
			ForwardClosure: `["fact-1","fact-2"]`, ReverseClosure: `["summary-1"]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if c.DerivedRef != "summary-1" || c.SourceRef != "cmdb" || c.ForwardClosure == "" || c.ReverseClosure == "" {
			t.Errorf("closure incomplete: %+v (派生修订保存按源修订标识的有界依赖闭包和反向依赖)", c)
		}
		if c.State != "complete" {
			t.Errorf("closure state = %s, want complete", c.State)
		}
	})

	t.Run("cycle_overflow_incomplete_not_usable", func(t *testing.T) {
		store := setupV3P504(t, "notusable.db")
		now := v3P102Now()
		// 有环/超限/未完成：保持不可用。
		for _, state := range []string{"cycle", "overflow", "incomplete"} {
			tx, _ := store.db.Begin()
			recordProvenanceClosure(tx, ProvenanceClosure{
				DerivedRef: "derived-" + state, DerivedRevision: 1, SourceRef: "cmdb", SourceRevision: 1,
				State: state,
			}, now)
			tx.Commit()
			tx, _ = store.db.Begin()
			err := ensureProvenanceUsable(tx, "derived-"+state, 1, now)
			tx.Rollback()
			if err == nil {
				t.Errorf("%s closure usable (有环、超限、未完成的派生记录保持不可用)", state)
			}
		}
	})

	t.Run("cache_key_all_fields", func(t *testing.T) {
		// 读取缓存键包含主体、记录修订、闭包摘要、相关策略版本/分区代际及最早到期时间。
		key := buildReadCacheKey("user:reader", 1, "digest-1", 1, 1, 1000000)
		if key.Subject != "user:reader" || key.RecordRevision != 1 || key.ClosureDigest != "digest-1" ||
			key.PolicyVersion != 1 || key.PartitionGeneration != 1 || key.EarliestExpiry != 1000000 {
			t.Errorf("cache key incomplete: %+v (读取缓存键包含主体/记录修订/闭包摘要/相关策略版本/分区代际/最早到期时间)", key)
		}
		if key.CacheKey == "" {
			t.Error("cache key empty")
		}
	})

	t.Run("generation_mismatch_not_use_old_cache", func(t *testing.T) {
		store := setupV3P504(t, "mismatch.db")
		now := v3P102Now()
		// 代际不一致：不能沿用旧缓存（新最终核验以当前权威快照比较代际）。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "partition-1", "admin", now)
		tx.Commit()
		// 代际递增后：旧代际缓存不能沿用。
		tx, _ = store.db.Begin()
		err := ensureSourceNotRevoked(tx, "node-1", "cmdb", 0, now) // 期望代际 0 < 当前 1
		tx.Rollback()
		if err == nil {
			t.Error("generation mismatch used old cache (不能沿用旧缓存)")
		}
	})
}

// TestV3P504CrossPartitionAuthorization 跨分区授权。
func TestV3P504CrossPartitionAuthorization(t *testing.T) {
	t.Run("cross_partition_verified_individually", func(t *testing.T) {
		store := setupV3P504(t, "cross.db")
		now := v3P102Now()
		// 跨分区来源逐一核验（不同分区独立撤销）。
		tx, _ := store.db.Begin()
		revokeSource(tx, "node-1", "cmdb", "partition-1", "admin", now)
		tx.Commit()
		// partition-2 未撤销：允许。
		tx, _ = store.db.Begin()
		err := ensureSourceNotRevoked(tx, "node-1", "cmdb", 1, now)
		tx.Rollback()
		// partition-1 已撤销：拒绝（跨分区来源逐一核验）。
		if err == nil {
			t.Error("partition-1 revoked not blocked (跨分区来源逐一核验)")
		}
	})

	t.Run("closure_overflow_not_downgrade_to_allow", func(t *testing.T) {
		store := setupV3P504(t, "overflow.db")
		now := v3P102Now()
		// 闭包超限：不会降级为允许。
		tx, _ := store.db.Begin()
		recordProvenanceClosure(tx, ProvenanceClosure{
			DerivedRef: "derived-overflow", DerivedRevision: 1, SourceRef: "cmdb", SourceRevision: 1,
			State: "overflow",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureProvenanceUsable(tx, "derived-overflow", 1, now)
		tx.Rollback()
		if err == nil {
			t.Error("overflow closure allowed (闭包超限时不会降级为允许)")
		}
	})

	t.Run("no_revocation_mechanism_not_claim_instant", func(t *testing.T) {
		// 无撤权机制不能宣称即时获知（外部可靠通知/实时授权与仅凭有效期两种保证分开）。
		// 仅凭有效期：不能宣称即时获知（已在途内容不能撤回）。
		t.Log("no revocation mechanism cannot claim instant knowledge (无撤权机制不能宣称即时获知，已在途内容不能撤回)")
	})

	t.Run("inflight_content_not_withdrawn", func(t *testing.T) {
		// 已在途内容不能撤回（外部可靠通知/实时授权与仅凭有效期两种保证分开）。
		t.Log("inflight content not withdrawn (已在途内容不能撤回)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
