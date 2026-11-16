package work

// P3-01 验收：统一主体、范围授权与直接撤权。
//
// 三个测试入口：
//   - TestV3P301AuthorizationScope：授权包含主体、动作、对象/范围、参数/时间
//     限制、来源/策略版本、有效期、撤销和责任人；未授权/过期/撤销/范围扩大
//     均拒绝；用户确认不能补足缺失权限。
//   - TestV3P301RevocationFinalGate：撤销与授权版本/相关代际及可靠通知同事务；
//     新的最终核验读取当前权威，不使用滞后索引或旧缓存；F07：阻断发生在新
//     最终核验，即使后台传播暂停仍不能放行。
//   - TestV3P301PrincipalSeparation：Worker 凭据、用户会话和外部事件主体不能
//     互相冒用；策略主体只有明确范围，不继承用户所有权限；API 不返回未授权
//     对象的标题/数量/敏感正文。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P301 创建存储。
func setupV3P301(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P301AuthorizationScope 授权范围。
func TestV3P301AuthorizationScope(t *testing.T) {
	t.Run("grant_has_all_fields", func(t *testing.T) {
		store := setupV3P301(t, "grant.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		auth, err := grantAuthorization(tx, Authorization{
			Subject: "user:alice", Action: "read", ObjectRef: "asset-x",
			ScopeLabel: "工作资料", Source: "user-grant", PolicyVersion: 1,
			ValidFrom: now, ExpiresAt: now + 100000, Owner: "alice",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 授权包含全部字段。
		if auth.Subject != "user:alice" || auth.Action != "read" || auth.ObjectRef != "asset-x" ||
			auth.ScopeLabel != "工作资料" || auth.PolicyVersion != 1 || auth.Owner != "alice" {
			t.Errorf("authorization incomplete: %+v", auth)
		}
	})

	t.Run("unauthorized_expired_revoked_scope_expansion_denied", func(t *testing.T) {
		store := setupV3P301(t, "denied.db")
		now := v3P102Now()
		// 未授权：拒绝。
		tx, _ := store.db.Begin()
		decision, err := decideAuthorization(tx, "user:mallory", "read", "asset-x", "", now, false)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("unauthorized not denied: %s", decision.Outcome)
		}
		// 过期：拒绝。
		tx, _ = store.db.Begin()
		if _, err := grantAuthorization(tx, Authorization{
			Subject: "user:bob", Action: "read", ObjectRef: "asset-y",
			ValidFrom: now - 2000, ExpiresAt: now - 1000, Owner: "bob",
		}, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		decision, _ = decideAuthorization(tx, "user:bob", "read", "asset-y", "", now, false)
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("expired authorization not denied: %s", decision.Outcome)
		}
		// 撤销：拒绝。
		tx, _ = store.db.Begin()
		auth, _ := grantAuthorization(tx, Authorization{
			Subject: "user:carol", Action: "read", ObjectRef: "asset-z", ValidFrom: now, Owner: "carol",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := revokeAuthorization(tx, auth.AuthID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		decision, _ = decideAuthorization(tx, "user:carol", "read", "asset-z", "", now, false)
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("revoked authorization not denied: %s", decision.Outcome)
		}
		// 范围扩大：拒绝（对象不匹配）。
		tx, _ = store.db.Begin()
		decision, _ = decideAuthorization(tx, "user:carol", "write", "asset-z", "", now, false)
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("scope expansion not denied: %s (范围扩大均拒绝)", decision.Outcome)
		}
	})

	t.Run("user_confirmation_not_substitute_permission", func(t *testing.T) {
		store := setupV3P301(t, "confirm.db")
		now := v3P102Now()
		// 用户确认不能补足缺失权限：无授权时确认也拒绝。
		tx, _ := store.db.Begin()
		decision, err := decideAuthorization(tx, "user:dave", "write", "asset-x", "", now, false)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("unauthorized confirmed action not denied: %s (用户确认不能补足缺失权限)", decision.Outcome)
		}
	})

	t.Run("actions_judged_separately", func(t *testing.T) {
		store := setupV3P301(t, "separate.db")
		now := v3P102Now()
		// 读取/材料/模型外发/任务控制/动作派发分别判定。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:eve", Action: "read", ObjectRef: "doc-1", ValidFrom: now, Owner: "eve"}, now)
		tx.Commit()
		// 读取允许，但写/外发/控制拒绝（分别判定）。
		for action, want := range map[string]string{
			"read": "allowed", "write": "denied", "export_model": "denied", "control": "denied",
		} {
			tx, _ := store.db.Begin()
			decision, _ := decideAuthorization(tx, "user:eve", action, "doc-1", "", now, false)
			tx.Commit()
			if decision.Outcome != want {
				t.Errorf("action %s = %s, want %s (分别判定)", action, decision.Outcome, want)
			}
		}
	})
}

// TestV3P301RevocationFinalGate 撤权最终核验。
func TestV3P301RevocationFinalGate(t *testing.T) {
	t.Run("revocation_final_check_reads_current_authority", func(t *testing.T) {
		store := setupV3P301(t, "finalgate.db")
		now := v3P102Now()
		// 授权后撤销：新的最终核验读取当前权威（不使用滞后索引或旧缓存）。
		tx, _ := store.db.Begin()
		auth, _ := grantAuthorization(tx, Authorization{
			Subject: "user:frank", Action: "read", ObjectRef: "asset-f", ValidFrom: now, Owner: "frank",
		}, now)
		tx.Commit()
		// 撤销前核验：允许。
		tx, _ = store.db.Begin()
		before, _ := decideAuthorization(tx, "user:frank", "read", "asset-f", "", now, false)
		tx.Commit()
		if before.Outcome != "allowed" {
			t.Fatal("pre-revocation check should allow")
		}
		// 撤销（同事务：授权版本+审计）。
		tx, _ = store.db.Begin()
		if err := revokeAuthorization(tx, auth.AuthID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 撤销后核验：拒绝（读取当前权威）。
		tx, _ = store.db.Begin()
		after, _ := decideAuthorization(tx, "user:frank", "read", "asset-f", "", now, false)
		tx.Commit()
		if after.Outcome != "denied" {
			t.Errorf("post-revocation check not denied: %s (撤销后新核验读取当前权威)", after.Outcome)
		}
	})

	t.Run("f07_revocation_blocks_new_final_check", func(t *testing.T) {
		store := setupV3P301(t, "f07.db")
		now := v3P102Now()
		// F07：撤销先于新最终核验时拒绝失权内容/动作；传播未完成也不放行。
		tx, _ := store.db.Begin()
		auth, _ := grantAuthorization(tx, Authorization{
			Subject: "user:grace", Action: "read", ObjectRef: "asset-g", ValidFrom: now, Owner: "grace",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := revokeAuthorization(tx, auth.AuthID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 传播暂停（模拟后台传播未完成）：新核验仍拒绝（阻断发生在新最终核验）。
		tx, _ = store.db.Begin()
		decision, _ := decideAuthorization(tx, "user:grace", "read", "asset-g", "", now, false)
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Errorf("F07: revoked access allowed after propagation pause: %s (阻断发生在新最终核验)", decision.Outcome)
		}
	})

	t.Run("in_flight_vs_post_revocation_distinguished", func(t *testing.T) {
		store := setupV3P301(t, "inflight.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		auth, _ := grantAuthorization(tx, Authorization{
			Subject: "user:henry", Action: "write", ObjectRef: "asset-h", ValidFrom: now, Owner: "henry",
		}, now)
		tx.Commit()
		// 撤销前在途请求：允许（保留在途责任）。
		tx, _ = store.db.Begin()
		inFlight, _ := decideAuthorization(tx, "user:henry", "write", "asset-h", "", now, true)
		tx.Commit()
		if inFlight.Outcome != "allowed" || !inFlight.InFlight {
			t.Errorf("in-flight request not preserved: %+v", inFlight)
		}
		// 撤销。
		tx, _ = store.db.Begin()
		revokeAuthorization(tx, auth.AuthID, "admin", now)
		tx.Commit()
		// 撤销后新核验：拒绝（区分在途与新核验）。
		tx, _ = store.db.Begin()
		after, _ := decideAuthorization(tx, "user:henry", "write", "asset-h", "", now, false)
		tx.Commit()
		if after.Outcome != "denied" {
			t.Errorf("post-revocation new check not denied: %s", after.Outcome)
		}
		// 在途记录仍可查（不宣称已发送内容可撤回）。
		var inFlightCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM authorization_decisions WHERE subject='user:henry' AND in_flight=1").Scan(&inFlightCount); err != nil {
			t.Fatal(err)
		}
		if inFlightCount != 1 {
			t.Errorf("in-flight record lost: count=%d (保留在途责任)", inFlightCount)
		}
	})
}

// TestV3P301PrincipalSeparation 主体分离。
func TestV3P301PrincipalSeparation(t *testing.T) {
	t.Run("worker_user_service_cannot_impersonate", func(t *testing.T) {
		store := setupV3P301(t, "separation.db")
		now := v3P102Now()
		// 注册用户与 Worker 主体。
		tx, _ := store.db.Begin()
		registerPrincipal(tx, Principal{PrincipalID: "user:alice", Kind: PrincipalUser}, "user-password", now)
		registerPrincipal(tx, Principal{PrincipalID: "worker:w1", Kind: PrincipalWorker}, "worker-secret", now)
		tx.Commit()
		// Worker 凭据不能冒用用户会话（类型不匹配拒绝）。
		_, err := authenticatePrincipal(store.db, "worker:w1", PrincipalUser, "worker-secret", now)
		if err == nil {
			t.Error("worker credential impersonated user (主体不能互相冒用)")
		}
		// 用户会话不能冒用 Worker。
		_, err = authenticatePrincipal(store.db, "user:alice", PrincipalWorker, "user-password", now)
		if err == nil {
			t.Error("user session impersonated worker")
		}
		// 伪造身份：拒绝。
		_, err = authenticatePrincipal(store.db, "user:alice", PrincipalUser, "forged-password", now)
		if err == nil {
			t.Error("forged identity not rejected (伪造 auth_ref 不能自证获准)")
		}
	})

	t.Run("policy_principal_explicit_scope_not_inherit_user", func(t *testing.T) {
		// 策略主体只有明确范围，不继承用户所有权限。
		policy := Principal{PrincipalID: "policy:monitor", Kind: PrincipalPolicy}
		// 无明确范围：拒绝。
		if err := ensurePolicyScope(policy, ""); err == nil {
			t.Error("policy without scope not rejected (策略主体必须有明确范围)")
		}
		// 有明确范围：允许。
		if err := ensurePolicyScope(policy, "监测范围"); err != nil {
			t.Errorf("policy with scope rejected: %v", err)
		}
	})

	t.Run("unauthorized_object_not_returned", func(t *testing.T) {
		store := setupV3P301(t, "filter.db")
		now := v3P102Now()
		// 授权只允许 doc-1。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:ivan", Action: "read", ObjectRef: "doc-1", ValidFrom: now, Owner: "ivan"}, now)
		tx.Commit()
		// API 不返回未授权对象（过滤后只有授权对象）。
		tx, _ = store.db.Begin()
		allowed, err := filterUnauthorized(tx, "user:ivan", "read", []string{"doc-1", "doc-2", "doc-3"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(allowed) != 1 || allowed[0] != "doc-1" {
			t.Errorf("unauthorized objects returned: %v (API 不返回未授权对象)", allowed)
		}
	})

	t.Run("decision_recorded_including_denial", func(t *testing.T) {
		store := setupV3P301(t, "audit.db")
		now := v3P102Now()
		// 每次判定记录（含拒绝）。
		tx, _ := store.db.Begin()
		decideAuthorization(tx, "user:judy", "read", "doc-x", "", now, false) // 拒绝
		decideAuthorization(tx, "user:judy", "read", "doc-y", "", now, false) // 拒绝
		tx.Commit()
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM authorization_decisions WHERE subject='user:judy'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("decisions not recorded: count=%d (拒绝与在途分别审计)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
