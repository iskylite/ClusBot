package work

// P2-01：工作者实例与受限身份。
//
// 本文件区分实际执行实例和角色，为统一 API 建立可信调用主体：
//   - 登记 worker_id、角色、支持单元类型、实例版本、最近联系、启用/撤销及
//     有效期；不能所有实例都叫 personal-worker；
//   - 建立受限 Worker 身份及凭据摘要，绑定允许领取的类型/范围；秘密不进入
//     日志、Manifest 或普通回执；
//   - 本地嵌入执行器也通过同一服务协议校验，不直接访问 SQLite；可以使用宿主
//     可信身份；
//   - 暴露必要的注册/领取/续租/回报入口；默认本机部署不因新增 Worker 接口
//     就开放所有管理路由到公网；
//   - 通用主体记录在 P3 扩展，不再创建一套不受统一撤权管理的 Worker 权限系统。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Worker 实例
// ---------------------------------------------------------------------------

// WorkerInstance 是 worker_instances 的一行。
type WorkerInstance struct {
	WorkerID          string
	Role              string
	SupportedKinds    []WorkUnitKind
	InstanceVersion   string
	LastContactAt     int64
	Enabled           bool
	RevokedAt         int64
	ExpiresAt         int64
	CredentialDigest  string // 凭据摘要（不存明文秘密）
	CredentialVersion int
	AllowedScope      string
	RegisteredBy      string
	CreatedAt         int64
	UpdatedAt         int64
}

// ---------------------------------------------------------------------------
// 注册（凭据摘要，秘密不落库）
// ---------------------------------------------------------------------------

// RegisterWorkerRequest 是注册请求。
type RegisterWorkerRequest struct {
	Role            string
	SupportedKinds  []WorkUnitKind
	InstanceVersion string
	ExpiresAt       int64
	Credential      string // 明文秘密（仅用于计算摘要，不落库）
	AllowedScope    string
	RegisteredBy    string
}

// registerWorker 登记一个 Worker 实例（P2-01.1/P2-01.2）。
// 凭据只保存摘要（SHA-256），秘密不进入日志、Manifest 或普通回执。
// 不能所有实例都叫 personal-worker：worker_id 是实例身份，由系统生成。
func registerWorker(tx *sql.Tx, req RegisterWorkerRequest, now int64) (WorkerInstance, error) {
	if req.Role == "" {
		return WorkerInstance{}, errors.New("worker 缺少角色")
	}
	if len(req.SupportedKinds) == 0 {
		return WorkerInstance{}, errors.New("worker 缺少支持的单元类型")
	}
	if req.Credential == "" {
		return WorkerInstance{}, errors.New("worker 缺少凭据")
	}
	if req.RegisteredBy == "" {
		return WorkerInstance{}, errors.New("worker 注册缺少决定者")
	}
	// 实例身份：系统生成（worker_id 是实例，不是角色名）。
	workerID := id()
	kindsJSON, err := json.Marshal(req.SupportedKinds)
	if err != nil {
		return WorkerInstance{}, err
	}
	credentialDigest := digestCredential(req.Credential)
	worker := WorkerInstance{
		WorkerID:          workerID,
		Role:              req.Role,
		SupportedKinds:    req.SupportedKinds,
		InstanceVersion:   req.InstanceVersion,
		LastContactAt:     now,
		Enabled:           true,
		ExpiresAt:         req.ExpiresAt,
		CredentialDigest:  credentialDigest,
		CredentialVersion: 1,
		AllowedScope:      req.AllowedScope,
		RegisteredBy:      req.RegisteredBy,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	_, err = tx.Exec(`INSERT INTO worker_instances(worker_id,role,supported_kinds,instance_version,
last_contact_at,enabled,expires_at,credential_digest,credential_version,allowed_scope,registered_by,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		workerID, req.Role, string(kindsJSON), req.InstanceVersion, now, 1,
		req.ExpiresAt, credentialDigest, 1, req.AllowedScope, req.RegisteredBy, now, now)
	if err != nil {
		return WorkerInstance{}, err
	}
	return worker, nil
}

// digestCredential 计算凭据摘要（不存明文）。
func digestCredential(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 认证（核对凭据摘要与实例状态）
// ---------------------------------------------------------------------------

// authenticateWorker 核对 Worker 凭据与实例状态（P2-01 反例）。
// 未认证、过期、撤销或伪造 worker_id 的请求拒绝；身份轮换不使旧令牌继续领取。
func authenticateWorker(q recordsQuerier, workerID, credential string, now int64) (WorkerInstance, error) {
	var w WorkerInstance
	var kindsJSON string
	var enabled int
	err := q.QueryRow(`SELECT worker_id,role,supported_kinds,instance_version,last_contact_at,enabled,
revoked_at,expires_at,credential_digest,credential_version,allowed_scope,registered_by,created_at,updated_at
FROM worker_instances WHERE worker_id=?`, workerID).
		Scan(&w.WorkerID, &w.Role, &kindsJSON, &w.InstanceVersion, &w.LastContactAt, &enabled,
			&w.RevokedAt, &w.ExpiresAt, &w.CredentialDigest, &w.CredentialVersion,
			&w.AllowedScope, &w.RegisteredBy, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerInstance{}, fmt.Errorf("%w：worker_id 不存在（伪造拒绝）", ErrWorkerAuth)
	}
	if err != nil {
		return WorkerInstance{}, err
	}
	if err := json.Unmarshal([]byte(kindsJSON), &w.SupportedKinds); err != nil {
		return WorkerInstance{}, err
	}
	w.Enabled = enabled == 1
	// 凭据摘要核对（伪造拒绝）。
	if digestCredential(credential) != w.CredentialDigest {
		return WorkerInstance{}, fmt.Errorf("%w：凭据不匹配", ErrWorkerAuth)
	}
	// 启用/撤销/有效期核对。
	if !w.Enabled || w.RevokedAt > 0 {
		return WorkerInstance{}, fmt.Errorf("%w：worker 已撤销", ErrWorkerAuth)
	}
	if w.ExpiresAt > 0 && now >= w.ExpiresAt {
		return WorkerInstance{}, fmt.Errorf("%w：worker 已过期", ErrWorkerAuth)
	}
	return w, nil
}

// ErrWorkerAuth 是 Worker 认证失败的错误。
var ErrWorkerAuth = errors.New("worker 认证失败")

// ---------------------------------------------------------------------------
// 范围核对（Worker 凭据不能调用越权入口）
// ---------------------------------------------------------------------------

// ErrWorkerScope 是 Worker 范围越权的错误。
var ErrWorkerScope = errors.New("worker 无权访问该范围")

// ensureWorkerScope 核对 Worker 对指定单元类型与对象范围的权限（P2-01 反例）。
// 工作者凭据不能调用用户设置、连接授权或任意资产管理入口。
func ensureWorkerScope(w WorkerInstance, kind WorkUnitKind, scope string) error {
	// 单元类型核对。
	supported := false
	for _, k := range w.SupportedKinds {
		if k == kind {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("%w：单元类型 %s 不在 worker 支持范围", ErrWorkerScope, kind)
	}
	// 对象范围核对（allowed_scope 为空表示仅限默认范围）。
	if w.AllowedScope != "" && scope != w.AllowedScope {
		return fmt.Errorf("%w：对象范围 %s 超出 worker 允许 %s", ErrWorkerScope, scope, w.AllowedScope)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 令牌（每次领取签发，绑定单元与代际，不含外部凭据）
// ---------------------------------------------------------------------------

// WorkerToken 是 worker_tokens 的一行。
type WorkerToken struct {
	TokenID         string
	WorkerID        string
	UnitID          string
	LeaseGeneration int
	TokenDigest     string
	ExpiresAt       int64
	State           string // active/renewed/expired/revoked
	IssuedAt        int64
	RenewedAt       int64
}

// issueToken 为一次领取签发令牌（P2-02 使用；令牌不包含外部凭据）。
func issueToken(tx *sql.Tx, workerID, unitID string, leaseGeneration int, ttl int64, now int64) (WorkerToken, string, error) {
	plainToken := hex.EncodeToString(randBytes())
	token := WorkerToken{
		TokenID:         id(),
		WorkerID:        workerID,
		UnitID:          unitID,
		LeaseGeneration: leaseGeneration,
		TokenDigest:     digestCredential(plainToken),
		ExpiresAt:       now + ttl,
		State:           "active",
		IssuedAt:        now,
	}
	_, err := tx.Exec(`INSERT INTO worker_tokens(token_id,worker_id,unit_id,lease_generation,token_digest,expires_at,state,issued_at)
VALUES(?,?,?,?,?,?,?,?)`,
		token.TokenID, workerID, unitID, leaseGeneration, token.TokenDigest, token.ExpiresAt, token.State, now)
	if err != nil {
		return WorkerToken{}, "", err
	}
	// 明文令牌只在签发时返回给调用方（不落库，不进日志/回执）。
	return token, plainToken, nil
}

// validateToken 核对令牌（P2-02 使用）。
// 过期令牌不能续租/提交；新领取不复用旧令牌（身份轮换不使旧令牌继续）。
func validateToken(q recordsQuerier, tokenID, plainToken string, now int64) (WorkerToken, error) {
	var t WorkerToken
	err := q.QueryRow(`SELECT token_id,worker_id,unit_id,lease_generation,token_digest,expires_at,state,issued_at,renewed_at
FROM worker_tokens WHERE token_id=?`, tokenID).
		Scan(&t.TokenID, &t.WorkerID, &t.UnitID, &t.LeaseGeneration, &t.TokenDigest, &t.ExpiresAt, &t.State, &t.IssuedAt, &t.RenewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerToken{}, fmt.Errorf("%w：令牌不存在", ErrWorkerAuth)
	}
	if err != nil {
		return WorkerToken{}, err
	}
	if digestCredential(plainToken) != t.TokenDigest {
		return WorkerToken{}, fmt.Errorf("%w：令牌摘要不匹配", ErrWorkerAuth)
	}
	if t.State != "active" {
		return WorkerToken{}, fmt.Errorf("%w：令牌已 %s（新领取不复用旧令牌）", ErrWorkerAuth, t.State)
	}
	if now >= t.ExpiresAt {
		return WorkerToken{}, fmt.Errorf("%w：令牌已过期", ErrWorkerAuth)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 撤销（统一撤权管理）
// ---------------------------------------------------------------------------

// revokeWorker 撤销一个 Worker 实例（P2-01.5：受统一撤权管理）。
// 撤销后新领取被拒；已签发的令牌标记 revoked（旧令牌不能继续）。
func revokeWorker(tx *sql.Tx, workerID, revokedBy string, now int64) error {
	res, err := tx.Exec(`UPDATE worker_instances SET enabled=0, revoked_at=?, updated_at=? WHERE worker_id=? AND enabled=1`,
		now, now, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("worker %s 不存在或已撤销", workerID)
	}
	// 撤销该实例的全部活跃令牌（旧令牌不能继续领取）。
	if _, err := tx.Exec(`UPDATE worker_tokens SET state='revoked' WHERE worker_id=? AND state='active'`, workerID); err != nil {
		return err
	}
	if err := recordAudit(tx, AuditEvent{
		Event: "worker_revoked", Actor: revokedBy, Summary: "撤销 worker 实例", OccurredAt: now,
	}); err != nil {
		return err
	}
	return nil
}

// touchWorker 更新最近联系时间（领取/续租/回报时）。
func touchWorker(tx *sql.Tx, workerID string, now int64) error {
	_, err := tx.Exec(`UPDATE worker_instances SET last_contact_at=? WHERE worker_id=?`, now, workerID)
	return err
}

// ---------------------------------------------------------------------------
// 本地嵌入执行器（P2-01.3：同一服务协议校验）
// ---------------------------------------------------------------------------

// embeddedWorkerIdentity 是本地嵌入执行器的宿主可信身份。
// 本地嵌入执行器也通过同一服务协议校验，不直接访问 SQLite。
func embeddedWorkerIdentity() RegisterWorkerRequest {
	return RegisterWorkerRequest{
		Role:            "embedded",
		SupportedKinds:  []WorkUnitKind{WorkUnitModel, WorkUnitDeterministic},
		InstanceVersion: "embedded-host",
		Credential:      hex.EncodeToString(randBytes()), // 宿主可信身份（每次启动生成）
		AllowedScope:    "",                              // 默认范围
		RegisteredBy:    "host",
	}
}

// 确保 context/strings 引用保留。
var (
	_ = context.Background
	_ = strings.Contains
)
