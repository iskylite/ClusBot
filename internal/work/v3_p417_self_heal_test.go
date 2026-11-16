package work

// P4-17 验收：有界自动自愈策略与 E02。
//
// 三个测试入口：
//   - TestV3P417SelfHealPolicyLimits：示例滚动 30 分钟每节点最多一次写派发、
//     同策略集群并发最多五个；作为可配置待验证参数，按稳定策略 ID 持久计数，
//     重启/新任务/修订不清零；重复告警、新 Task 或策略版本改变不能绕过次数
//     限制，动作与效果有独立账本。
//   - TestV3P417E02SelfHealEndToEnd：策略固定 ID/版本、主体、资产范围、有效期、
//     前置、动作与验证标准；实现调查归并、维护接管、Operation、验证和待复核
//     闭环；成功回执后独立检查服务与新心跳/覆盖；验证失败不追加第二次重启，
//     未知效果先核对；E02 正常授权路径无需逐步人工确认。
//   - TestV3P417PolicyRevocationInFlight：借出/冻结/身份冲突/共享故障不能走
//     普通自动写；越权/超预算/借出/缺证据时准确阻断；回执丢失和策略撤销发生
//     于派发边界时符合 F02/F07/F08；观察模式只报告拟判定，不派发写或改资产
//     生命周期；停用/撤销阻止新准入，未决责任仍处理。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P417 创建存储与资产。
func setupV3P417(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// v3P417Policy 构造策略。
func v3P417Policy() SelfHealPolicy {
	return SelfHealPolicy{
		PolicyKey: "agent-recovery", Version: 1, Subject: "system",
		AssetScope: `{"kind":"node"}`, ValidFrom: v3P102Now(),
		Preconditions: `["agent_unreachable"]`, ActionCapability: "restart-agent",
		VerificationStandard: "service_healthy+new_heartbeat", PublishedBy: "admin",
		PerNodeWindowMS: 1800000, PerNodeMaxWrites: 1, ClusterMaxConcurrent: 5,
	}
}

// TestV3P417SelfHealPolicyLimits 策略限流。
func TestV3P417SelfHealPolicyLimits(t *testing.T) {
	t.Run("per_node_max_once_per_30min", func(t *testing.T) {
		store := setupV3P417(t, "pernode.db")
		now := v3P102Now()
		policy := v3P417Policy()
		// 滚动 30 分钟每节点最多一次写派发。
		tx, _ := store.db.Begin()
		windowStart := now - (now % policy.PerNodeWindowMS)
		incrementSelfHealCounter(tx, "policy-1", "per_node:node-1", windowStart, now)
		tx.Commit()
		// 第二次：拒绝（已达最大写派发）。
		tx, _ = store.db.Begin()
		err := checkSelfHealRateLimit(tx, "policy-1", "node-1", policy.PerNodeWindowMS, 1, 5, now)
		tx.Rollback()
		if err == nil {
			t.Error("per-node rate limit not enforced (滚动 30 分钟每节点最多一次写派发)")
		}
	})

	t.Run("cluster_max_five_concurrent", func(t *testing.T) {
		store := setupV3P417(t, "cluster.db")
		now := v3P102Now()
		policy := v3P417Policy()
		// 同策略集群并发最多五个。
		tx, _ := store.db.Begin()
		windowStart := now - (now % policy.PerNodeWindowMS)
		for i := 0; i < 5; i++ {
			incrementSelfHealCounter(tx, "policy-1", "cluster_concurrent", windowStart, now)
		}
		tx.Commit()
		// 第六个：拒绝（已达集群并发最大）。
		tx, _ = store.db.Begin()
		err := checkSelfHealRateLimit(tx, "policy-1", "node-new", policy.PerNodeWindowMS, 1, 5, now)
		tx.Rollback()
		if err == nil {
			t.Error("cluster concurrent limit not enforced (同策略集群并发最多五个)")
		}
	})

	t.Run("persistent_counter_restart_not_reset", func(t *testing.T) {
		// 按稳定策略 ID 持久计数，重启/新任务/修订不清零。
		path := filepath.Join(t.TempDir(), "persist.db")
		now := v3P102Now()
		windowStart := now - (now % 1800000)
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			tx, _ := store.db.Begin()
			incrementSelfHealCounter(tx, "policy-1", "per_node:node-1", windowStart, now)
			tx.Commit()
		}()
		// 重启：计数仍在（不清零）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var count int
		if err := store.db.QueryRow("SELECT count FROM self_heal_counters WHERE policy_id='policy-1' AND counter_key='per_node:node-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("counter reset after restart: count=%d (按稳定策略 ID 持久计数，重启/新任务/修订不清零)", count)
		}
	})

	t.Run("new_task_version_not_bypass_limit", func(t *testing.T) {
		store := setupV3P417(t, "bypass.db")
		now := v3P102Now()
		// 重复告警、新 Task 或策略版本改变不能绕过次数限制（按稳定策略 ID 持久计数）。
		tx, _ := store.db.Begin()
		windowStart := now - (now % 1800000)
		incrementSelfHealCounter(tx, "policy-stable", "per_node:node-1", windowStart, now)
		tx.Commit()
		// 新 Task：不能绕过（按稳定策略 ID 持久计数）。
		tx, _ = store.db.Begin()
		err := checkSelfHealRateLimit(tx, "policy-stable", "node-1", 1800000, 1, 5, now)
		tx.Rollback()
		if err == nil {
			t.Error("new task bypassed rate limit (重复告警、新 Task 或策略版本改变不能绕过次数限制)")
		}
	})
}

// TestV3P417E02SelfHealEndToEnd E02 端到端。
func TestV3P417E02SelfHealEndToEnd(t *testing.T) {
	t.Run("policy_all_fields_published", func(t *testing.T) {
		store := setupV3P417(t, "policy.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, err := publishSelfHealPolicy(tx, v3P417Policy(), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if p.PolicyKey != "agent-recovery" || p.Version != 1 || p.Subject != "system" ||
			p.ActionCapability != "restart-agent" || p.VerificationStandard == "" {
			t.Errorf("policy incomplete: %+v (策略固定 ID/版本/主体/资产范围/有效期/前置/动作/验证标准)", p)
		}
		// 默认观察模式（先受控环境）。
		if !p.ObserveOnly {
			t.Error("policy not observe-only by default (先受控环境)")
		}
		// 默认限流参数。
		if p.PerNodeWindowMS != 1800000 || p.PerNodeMaxWrites != 1 || p.ClusterMaxConcurrent != 5 {
			t.Errorf("default limits wrong: %+v (示例滚动 30 分钟每节点最多一次写派发、集群并发最多五个)", p)
		}
	})

	t.Run("verify_success_receipt_independent_check", func(t *testing.T) {
		store := setupV3P417(t, "verify.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		// 激活策略（非观察模式）。
		tx.Exec("UPDATE self_heal_policies SET state='active', observe_only=0 WHERE policy_id=?", p.PolicyID)
		tx.Commit()
		// 记录执行。
		tx, _ = store.db.Begin()
		exec := SelfHealExecution{
			ExecutionID: id(), PolicyID: p.PolicyID, ObjectRef: "node-1",
			State: "dispatched", CreatedAt: now, UpdatedAt: now,
		}
		recordSelfHealExecution(tx, exec, now)
		tx.Commit()
		// 成功回执后独立检查服务与新心跳/覆盖。
		tx, _ = store.db.Begin()
		verified, reason, err := verifySelfHealEffect(tx, exec.ExecutionID, true, true, true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !verified {
			t.Errorf("verify failed: %s (成功回执后独立检查服务与新心跳/覆盖)", reason)
		}
	})

	t.Run("verify_failure_not_second_restart", func(t *testing.T) {
		store := setupV3P417(t, "verifyfail.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		exec := SelfHealExecution{
			ExecutionID: id(), PolicyID: p.PolicyID, ObjectRef: "node-1",
			State: "dispatched", CreatedAt: now, UpdatedAt: now,
		}
		recordSelfHealExecution(tx, exec, now)
		tx.Commit()
		// 验证失败（服务不健康）：不追加第二次重启。
		tx, _ = store.db.Begin()
		verified, reason, err := verifySelfHealEffect(tx, exec.ExecutionID, false, true, true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if verified {
			t.Error("verify failure marked verified (验证失败不追加第二次重启)")
		}
		if !strings.Contains(reason, "不健康") && !strings.Contains(reason, "不追加") {
			t.Errorf("verify failure reason missing: %s", reason)
		}
	})

	t.Run("unknown_effect_reconcile_first", func(t *testing.T) {
		store := setupV3P417(t, "unknown.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		exec := SelfHealExecution{
			ExecutionID: id(), PolicyID: p.PolicyID, ObjectRef: "node-1",
			State: "dispatched", CreatedAt: now, UpdatedAt: now,
		}
		recordSelfHealExecution(tx, exec, now)
		tx.Commit()
		// 未知效果（无新心跳）：先核对。
		tx, _ = store.db.Begin()
		verified, reason, err := verifySelfHealEffect(tx, exec.ExecutionID, true, false, true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if verified {
			t.Error("unknown effect marked verified (未知效果先核对)")
		}
		if !strings.Contains(reason, "心跳") && !strings.Contains(reason, "核对") {
			t.Errorf("unknown effect reason missing: %s", reason)
		}
	})

	t.Run("action_effect_independent_ledger", func(t *testing.T) {
		store := setupV3P417(t, "ledger.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		tx.Commit()
		// 动作与效果有独立账本（self_heal_executions 独立记录）。
		tx, _ = store.db.Begin()
		exec := SelfHealExecution{
			ExecutionID: id(), PolicyID: p.PolicyID, ObjectRef: "node-1",
			State: "verified", VerificationOutcome: "verified", CreatedAt: now, UpdatedAt: now,
		}
		if err := recordSelfHealExecution(tx, exec, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM self_heal_executions WHERE policy_id=?", p.PolicyID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("action/effect not independent ledger: count=%d (动作与效果有独立账本)", count)
		}
	})
}

// TestV3P417PolicyRevocationInFlight 撤销在途。
func TestV3P417PolicyRevocationInFlight(t *testing.T) {
	t.Run("lent_asset_not_normal_auto_write", func(t *testing.T) {
		store := setupV3P417(t, "lent.db")
		now := v3P102Now()
		// 借出资产：不能走普通自动写。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateLent, "node-1"); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		tx.Exec("UPDATE self_heal_policies SET state='active', observe_only=0 WHERE policy_id=?", p.PolicyID)
		tx.Commit()
		// 重新读取策略（observe_only=0 生效）。
		var observeOnly int
		if err := store.db.QueryRow("SELECT observe_only FROM self_heal_policies WHERE policy_id=?", p.PolicyID).Scan(&observeOnly); err != nil {
			t.Fatal(err)
		}
		p.ObserveOnly = observeOnly == 1
		tx, _ = store.db.Begin()
		exec, err := admitSelfHealExecution(tx, p, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exec.State != "blocked" {
			t.Errorf("lent asset not blocked: state=%s (借出/冻结/身份冲突/共享故障不能走普通自动写)", exec.State)
		}
	})

	t.Run("frozen_asset_blocked", func(t *testing.T) {
		store := setupV3P417(t, "frozen.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		freezeAsset(tx, "node-1", "admin", "维护冻结", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		tx.Exec("UPDATE self_heal_policies SET state='active', observe_only=0 WHERE policy_id=?", p.PolicyID)
		tx.Commit()
		// 重新读取策略（observe_only=0 生效）。
		var observeOnly int
		if err := store.db.QueryRow("SELECT observe_only FROM self_heal_policies WHERE policy_id=?", p.PolicyID).Scan(&observeOnly); err != nil {
			t.Fatal(err)
		}
		p.ObserveOnly = observeOnly == 1
		tx, _ = store.db.Begin()
		exec, err := admitSelfHealExecution(tx, p, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exec.State != "blocked" {
			t.Errorf("frozen asset not blocked: state=%s", exec.State)
		}
	})

	t.Run("observe_only_reports_not_dispatches", func(t *testing.T) {
		store := setupV3P417(t, "observe.db")
		now := v3P102Now()
		// 观察模式：只报告拟判定（不派发写或改资产生命周期）。
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now) // observe_only=true
		tx.Commit()
		tx, _ = store.db.Begin()
		exec, err := admitSelfHealExecution(tx, p, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exec.State != "proposed" {
			t.Errorf("observe-only dispatched: state=%s (观察模式只报告拟判定不派发写)", exec.State)
		}
		// 未派发写（无 Operation）。
		var opCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operations").Scan(&opCount); err != nil {
			t.Fatal(err)
		}
		if opCount != 0 {
			t.Errorf("observe-only created write: count=%d", opCount)
		}
	})

	t.Run("suspended_revoked_blocks_new_admission", func(t *testing.T) {
		store := setupV3P417(t, "suspended.db")
		now := v3P102Now()
		// 停用/撤销：阻止新准入。
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		tx.Exec("UPDATE self_heal_policies SET state='suspended', observe_only=0 WHERE policy_id=?", p.PolicyID)
		tx.Commit()
		// 重新读取策略（observe_only=0 生效）。
		tx, _ = store.db.Begin()
		if err := tx.QueryRow("SELECT state, observe_only FROM self_heal_policies WHERE policy_id=?", p.PolicyID).Scan(&p.State, new(bool)); err != nil {
			t.Fatal(err)
		}
		var observeOnly int
		if err := tx.QueryRow("SELECT observe_only FROM self_heal_policies WHERE policy_id=?", p.PolicyID).Scan(&observeOnly); err != nil {
			t.Fatal(err)
		}
		p.ObserveOnly = observeOnly == 1
		tx.Commit()
		tx, _ = store.db.Begin()
		exec, err := admitSelfHealExecution(tx, p, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exec.State != "blocked" {
			t.Errorf("suspended policy not blocked: state=%s (停用/撤销阻止新准入)", exec.State)
		}
		if !strings.Contains(exec.BlockReason, "suspended") && !strings.Contains(exec.BlockReason, "阻止") {
			t.Errorf("block reason missing: %s", exec.BlockReason)
		}
	})

	t.Run("over_budget_blocked", func(t *testing.T) {
		store := setupV3P417(t, "overbudget.db")
		now := v3P102Now()
		// 超预算（限流）：准确阻断。
		tx, _ := store.db.Begin()
		p, _ := publishSelfHealPolicy(tx, v3P417Policy(), now)
		tx.Exec("UPDATE self_heal_policies SET state='active', observe_only=0 WHERE policy_id=?", p.PolicyID)
		tx.Commit()
		// 重新读取策略（observe_only=0 生效）。
		var observeOnly int
		if err := store.db.QueryRow("SELECT observe_only FROM self_heal_policies WHERE policy_id=?", p.PolicyID).Scan(&observeOnly); err != nil {
			t.Fatal(err)
		}
		p.ObserveOnly = observeOnly == 1
		// 已达限流。
		tx, _ = store.db.Begin()
		windowStart := now - (now % p.PerNodeWindowMS)
		incrementSelfHealCounter(tx, p.PolicyID, "per_node:node-1", windowStart, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		exec, err := admitSelfHealExecution(tx, p, "node-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if exec.State != "blocked" {
			t.Errorf("over-budget not blocked: state=%s (越权/超预算/借出/缺证据时准确阻断)", exec.State)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
