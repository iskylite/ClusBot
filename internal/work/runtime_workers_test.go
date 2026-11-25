package work

// 验收：工作者实例与受限身份。
//
// 三个测试入口：
//   - TestWorkerIdentity：登记 worker_id、角色、支持单元类型、实例版本、
//     最近联系、启用/撤销及有效期；不能所有实例都叫 personal-worker；本地嵌入
//     执行器也通过同一服务协议校验。
//   - TestWorkerScopeDenied：未认证、过期、撤销或伪造 worker_id 的请求
//     拒绝；工作者凭据不能调用用户设置、连接授权或任意资产管理入口。
//   - TestWorkerCredentialRevoked：同角色的两个实例可独立识别，身份轮换
//     不使旧令牌继续领取；秘密不进入日志、Manifest 或普通回执。

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupWorkersStore 创建存储。
func setupWorkersStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// workerRegisterRequest 构造注册请求。
func workerRegisterRequest(role string, kinds []WorkUnitKind, scope string) RegisterWorkerRequest {
	return RegisterWorkerRequest{
		Role:            role,
		SupportedKinds:  kinds,
		InstanceVersion: "v1.0",
		ExpiresAt:       recordsTestNow() + 100000,
		Credential:      "secret-" + role,
		AllowedScope:    scope,
		RegisteredBy:    "admin",
	}
}

// TestWorkerIdentity 实例身份。
func TestWorkerIdentity(t *testing.T) {
	t.Run("register_instance_with_all_fields", func(t *testing.T) {
		store := setupWorkersStore(t, "register.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, err := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel, WorkUnitDeterministic}, "工作资料"), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 实例身份：系统生成（worker_id 是实例，不是角色名）。
		if worker.WorkerID == "" || worker.WorkerID == "personal-worker" || worker.WorkerID == "delivery" {
			t.Errorf("worker_id = %s (worker_id 是实例，不能所有实例都叫 personal-worker 或角色名)", worker.WorkerID)
		}
		if worker.Role != "delivery" || len(worker.SupportedKinds) != 2 || worker.InstanceVersion != "v1.0" ||
			!worker.Enabled || worker.AllowedScope != "工作资料" || worker.RegisteredBy != "admin" {
			t.Errorf("worker instance incomplete: %+v", worker)
		}
		// 凭据只保存摘要（不存明文秘密）。
		if worker.CredentialDigest == "" || strings.Contains(worker.CredentialDigest, "secret") {
			t.Errorf("credential stored as plaintext: %s (秘密不进入库)", worker.CredentialDigest)
		}
		// 最近联系时间已记录。
		if worker.LastContactAt != now {
			t.Errorf("last contact = %d, want %d", worker.LastContactAt, now)
		}
	})

	t.Run("two_instances_same_role_independently_identified", func(t *testing.T) {
		store := setupWorkersStore(t, "two.db")
		now := recordsTestNow()
		// 同角色的两个实例：独立身份（worker_id 不同）。
		tx, _ := store.db.Begin()
		first, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		second, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		tx.Commit()
		if first.WorkerID == second.WorkerID {
			t.Error("two instances same role share worker_id (同角色实例应独立识别)")
		}
		// 各自独立认证。
		w1, err := authenticateWorker(store.db, first.WorkerID, "secret-delivery", now)
		if err != nil {
			t.Fatal(err)
		}
		w2, err := authenticateWorker(store.db, second.WorkerID, "secret-delivery", now)
		if err != nil {
			t.Fatal(err)
		}
		if w1.WorkerID != first.WorkerID || w2.WorkerID != second.WorkerID {
			t.Errorf("instances not independently authenticated: %s vs %s", w1.WorkerID, w2.WorkerID)
		}
	})

	t.Run("embedded_executor_same_protocol", func(t *testing.T) {
		store := setupWorkersStore(t, "embedded.db")
		now := recordsTestNow()
		// 本地嵌入执行器：同一服务协议校验（注册 + 认证），不直接访问 SQLite。
		tx, _ := store.db.Begin()
		embedded, err := registerWorker(tx, embeddedWorkerIdentity(), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if embedded.Role != "embedded" || embedded.RegisteredBy != "host" {
			t.Errorf("embedded identity = %+v", embedded)
		}
		// 嵌入执行器同样经凭据认证（同一服务协议）。
		// 宿主可信身份：每次启动生成的凭据（测试用任意凭据会失败，因为摘要不匹配）。
		_, err = authenticateWorker(store.db, embedded.WorkerID, "wrong-credential", now)
		if err == nil {
			t.Error("embedded executor authenticated with wrong credential (应经同一协议校验)")
		}
	})
}

// TestWorkerScopeDenied 范围拒绝。
func TestWorkerScopeDenied(t *testing.T) {
	t.Run("unauthenticated_expired_revoked_forged_denied", func(t *testing.T) {
		store := setupWorkersStore(t, "denied.db")
		now := recordsTestNow()
		// 伪造 worker_id：拒绝。
		if _, err := authenticateWorker(store.db, "forged-worker", "secret", now); err == nil {
			t.Error("forged worker_id not denied")
		}
		// 未认证（错误凭据）：拒绝。
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		tx.Commit()
		if _, err := authenticateWorker(store.db, worker.WorkerID, "wrong-secret", now); err == nil {
			t.Error("wrong credential not denied")
		}
		// 过期：拒绝。
		tx, _ = store.db.Begin()
		expired, _ := registerWorker(tx, RegisterWorkerRequest{
			Role: "old", SupportedKinds: []WorkUnitKind{WorkUnitModel}, Credential: "old-secret",
			ExpiresAt: now - 1000, RegisteredBy: "admin", // 已过期
		}, now)
		tx.Commit()
		if _, err := authenticateWorker(store.db, expired.WorkerID, "old-secret", now); err == nil {
			t.Error("expired worker not denied")
		}
		// 撤销：拒绝。
		tx, _ = store.db.Begin()
		revoked, _ := registerWorker(tx, workerRegisterRequest("revoked", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := revokeWorker(tx, revoked.WorkerID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if _, err := authenticateWorker(store.db, revoked.WorkerID, "secret-revoked", now); err == nil {
			t.Error("revoked worker not denied")
		}
	})

	t.Run("worker_credential_not_for_user_settings_or_connections", func(t *testing.T) {
		store := setupWorkersStore(t, "scope.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作资料"), now)
		tx.Commit()
		// 单元类型越权：拒绝（delivery 只支持 model，不支持 action）。
		if err := ensureWorkerScope(worker, WorkUnitAction, "工作资料"); err == nil {
			t.Error("worker allowed unsupported unit kind (单元类型越权)")
		}
		// 对象范围越权：拒绝（allowed_scope=工作资料，不能访问"资产管理"）。
		if err := ensureWorkerScope(worker, WorkUnitModel, "资产管理"); err == nil {
			t.Error("worker allowed out-of-scope target (工作者凭据不能调用任意资产管理入口)")
		}
		// 范围内：允许。
		if err := ensureWorkerScope(worker, WorkUnitModel, "工作资料"); err != nil {
			t.Errorf("worker denied in-scope access: %v", err)
		}
	})

	t.Run("wrong_scope_label_denied", func(t *testing.T) {
		store := setupWorkersStore(t, "wrongscope.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("investigate", []WorkUnitKind{WorkUnitDeterministic}, "网络运维"), now)
		tx.Commit()
		// 用户设置范围：拒绝。
		if err := ensureWorkerScope(worker, WorkUnitDeterministic, "用户设置"); err == nil {
			t.Error("worker allowed user settings scope (凭据不能调用用户设置入口)")
		}
		// 连接授权范围：拒绝。
		if err := ensureWorkerScope(worker, WorkUnitDeterministic, "连接授权"); err == nil {
			t.Error("worker allowed connection auth scope (凭据不能调用连接授权入口)")
		}
	})
}

// TestWorkerCredentialRevoked 凭据撤销与轮换。
func TestWorkerCredentialRevoked(t *testing.T) {
	t.Run("revoked_token_cannot_claim", func(t *testing.T) {
		store := setupWorkersStore(t, "tokenrevoke.db")
		now := recordsTestNow()
		// 注册并签发令牌。
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		token, plainToken, err := issueToken(tx, worker.WorkerID, "unit-1", 1, 100000, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 有效令牌：可核对。
		if _, err := validateToken(store.db, token.TokenID, plainToken, now); err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
		// 撤销 worker：旧令牌标记 revoked，不能继续领取。
		tx, _ = store.db.Begin()
		if err := revokeWorker(tx, worker.WorkerID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if _, err := validateToken(store.db, token.TokenID, plainToken, now); err == nil {
			t.Error("revoked token still valid (身份轮换不使旧令牌继续领取)")
		}
	})

	t.Run("new_claim_not_reuse_old_token", func(t *testing.T) {
		store := setupWorkersStore(t, "newtoken.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		// 旧令牌（先签发，再在事务内标记 renewed）。
		oldToken, plainOld, _ := issueToken(tx, worker.WorkerID, "unit-1", 1, 100000, now)
		if _, err := tx.Exec("UPDATE worker_tokens SET state='renewed' WHERE token_id=?", oldToken.TokenID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 新领取签发新令牌：不复用旧令牌。
		tx, _ = store.db.Begin()
		newToken, _, err := issueToken(tx, worker.WorkerID, "unit-1", 2, 100000, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if newToken.TokenID == oldToken.TokenID {
			t.Error("new claim reused old token (新领取不复用旧令牌)")
		}
		// 旧令牌（renewed 状态）不能继续。
		if _, err := validateToken(store.db, oldToken.TokenID, plainOld, now); err == nil {
			t.Error("renewed old token still accepted")
		}
	})

	t.Run("credential_digest_only_no_plaintext", func(t *testing.T) {
		store := setupWorkersStore(t, "digest.db")
		now := recordsTestNow()
		plainSecret := "super-secret-credential"
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, RegisterWorkerRequest{
			Role: "delivery", SupportedKinds: []WorkUnitKind{WorkUnitModel}, Credential: plainSecret,
			ExpiresAt: now + 100000, RegisteredBy: "admin",
		}, now)
		tx.Commit()
		// 数据库中只有摘要，无明文秘密（秘密不进入日志/Manifest/回执）。
		var digest string
		if err := store.db.QueryRow("SELECT credential_digest FROM worker_instances WHERE worker_id=?", worker.WorkerID).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if digest == plainSecret || strings.Contains(digest, "super-secret") {
			t.Errorf("plaintext secret stored: %s (秘密不落库)", digest)
		}
		// 摘要可用于认证（不能反推明文）。
		if _, err := authenticateWorker(store.db, worker.WorkerID, plainSecret, now); err != nil {
			t.Errorf("authentication with correct credential failed: %v", err)
		}
	})

	t.Run("revoke_records_audit", func(t *testing.T) {
		store := setupWorkersStore(t, "audit.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		tx.Commit()
		tx, _ = store.db.Begin()
		if err := revokeWorker(tx, worker.WorkerID, "admin", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 撤销已审计（统一撤权管理）。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='worker_revoked'").Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("revocation not audited: count=%d (受统一撤权管理)", auditCount)
		}
	})

	t.Run("touch_updates_last_contact", func(t *testing.T) {
		store := setupWorkersStore(t, "touch.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		worker, _ := registerWorker(tx, workerRegisterRequest("delivery", []WorkUnitKind{WorkUnitModel}, "工作"), now)
		tx.Commit()
		// 领取/续租/回报时更新最近联系。
		later := now + 5000
		tx, _ = store.db.Begin()
		if err := touchWorker(tx, worker.WorkerID, later); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var lastContact int64
		if err := store.db.QueryRow("SELECT last_contact_at FROM worker_instances WHERE worker_id=?", worker.WorkerID).Scan(&lastContact); err != nil {
			t.Fatal(err)
		}
		if lastContact != later {
			t.Errorf("last contact = %d, want %d", lastContact, later)
		}
	})
}

// 确保 sql/strings 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
)
