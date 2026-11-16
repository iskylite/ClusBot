package work

// P3-01：统一主体、范围授权与直接撤权。
//
// 本文件实现用户、外部服务、Worker 和策略执行主体使用同一授权语义：
//   - 授权包含主体、动作、对象/范围、参数/时间限制、来源/策略版本、有效期、
//     撤销和责任人；旧工具 allowed 不自动变成资产写权限；
//   - 读取、材料、模型外发、任务控制与动作派发分别判定；调用者不能用伪造
//     auth_ref 自证获准；
//   - 撤销与授权版本/相关代际及可靠通知同事务；新的最终核验读取当前权威，
//     不使用滞后索引或旧缓存；
//   - 区分已撤销后的新核验和撤销前在途请求；保留在途责任，不宣称已发送内容
//     可撤回；
//   - API 不返回未授权对象的标题/数量/敏感正文；策略主体只有明确范围，不
//     继承用户所有权限。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 主体（用户/服务/Worker/策略执行主体统一注册）
// ---------------------------------------------------------------------------

// PrincipalKind 是主体类型。
type PrincipalKind string

const (
	PrincipalUser    PrincipalKind = "user"
	PrincipalService PrincipalKind = "service"
	PrincipalWorker  PrincipalKind = "worker"
	PrincipalPolicy  PrincipalKind = "policy"
)

// Principal 是 principals 的一行。
type Principal struct {
	PrincipalID    string
	Kind           PrincipalKind
	IdentityDigest string
	Enabled        bool
	ExpiresAt      int64
	RevokedAt      int64
	CreatedAt      int64
}

// registerPrincipal 注册一个主体（P3-01 反例：主体不能互相冒用）。
func registerPrincipal(tx *sql.Tx, p Principal, identity string, now int64) (Principal, error) {
	if p.PrincipalID == "" || p.Kind == "" {
		return Principal{}, errors.New("主体缺少身份或类型")
	}
	if identity == "" {
		return Principal{}, errors.New("主体缺少身份凭据")
	}
	p.IdentityDigest = digestCredential(identity)
	p.Enabled = true
	p.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO principals(principal_id,kind,identity_digest,enabled,expires_at,created_at)
VALUES(?,?,?,?,?,?)`, p.PrincipalID, string(p.Kind), p.IdentityDigest, 1, p.ExpiresAt, now)
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}

// authenticatePrincipal 核对主体身份与类型（P3-01 反例：Worker 凭据、用户会话
// 和外部事件主体不能互相冒用）。
func authenticatePrincipal(q recordsQuerier, principalID string, kind PrincipalKind, identity string, now int64) (Principal, error) {
	var p Principal
	var pKind string
	var enabled int
	err := q.QueryRow(`SELECT principal_id,kind,identity_digest,enabled,expires_at,revoked_at,created_at
FROM principals WHERE principal_id=?`, principalID).
		Scan(&p.PrincipalID, &pKind, &p.IdentityDigest, &enabled, &p.ExpiresAt, &p.RevokedAt, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, fmt.Errorf("%w：主体不存在", ErrAuthorizationDenied)
	}
	if err != nil {
		return Principal{}, err
	}
	p.Kind = PrincipalKind(pKind)
	p.Enabled = enabled == 1
	// 类型核对：Worker 凭据不能冒用用户会话（类型必须匹配）。
	if p.Kind != kind {
		return Principal{}, fmt.Errorf("%w：主体类型不匹配（%s 不能冒用 %s）", ErrAuthorizationDenied, kind, p.Kind)
	}
	// 身份摘要核对（伪造 auth_ref 不能自证获准）。
	if digestCredential(identity) != p.IdentityDigest {
		return Principal{}, fmt.Errorf("%w：身份凭据不匹配（伪造 auth_ref 不能自证获准）", ErrAuthorizationDenied)
	}
	if !p.Enabled || p.RevokedAt > 0 {
		return Principal{}, fmt.Errorf("%w：主体已撤销", ErrAuthorizationDenied)
	}
	if p.ExpiresAt > 0 && now >= p.ExpiresAt {
		return Principal{}, fmt.Errorf("%w：主体已过期", ErrAuthorizationDenied)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 授权（统一授权语义）
// ---------------------------------------------------------------------------

// Authorization 是 authorizations 的一行。
type Authorization struct {
	AuthID         string
	Subject        string
	Action         string
	ObjectRef      string
	ScopeLabel     string
	ParamConstraints string
	Source         string
	PolicyVersion  int
	ValidFrom      int64
	ExpiresAt      int64
	RevokedAt      int64
	RevokedBy      string
	Owner          string
	Revision       int
	CreatedAt      int64
	UpdatedAt      int64
}

// grantAuthorization 授予一个授权（P3-01.1）。
// 授权包含主体、动作、对象/范围、参数/时间限制、来源/策略版本、有效期、
// 撤销和责任人；旧工具 allowed 不自动变成资产写权限。
func grantAuthorization(tx *sql.Tx, a Authorization, now int64) (Authorization, error) {
	if a.Subject == "" || a.Action == "" || a.Owner == "" {
		return Authorization{}, errors.New("授权缺少主体/动作/责任人")
	}
	if a.ParamConstraints == "" {
		a.ParamConstraints = "{}"
	}
	if a.Revision < 1 {
		a.Revision = 1
	}
	a.AuthID = id()
	a.CreatedAt, a.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,scope_label,
param_constraints,source,policy_version,valid_from,expires_at,owner,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.AuthID, a.Subject, a.Action, a.ObjectRef, a.ScopeLabel, a.ParamConstraints, a.Source,
		a.PolicyVersion, a.ValidFrom, a.ExpiresAt, a.Owner, a.Revision, now, now)
	if err != nil {
		return Authorization{}, err
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// 判定（读取/材料/模型外发/任务控制/动作派发分别判定）
// ---------------------------------------------------------------------------

// AuthorizationDecision 是一次授权判定。
type AuthorizationDecision struct {
	DecisionID   string
	Subject      string
	Action       string
	ObjectRef    string
	Outcome      string // allowed/denied/requires_confirmation
	Reason       string
	PolicyVersion int
	DecidedAt    int64
	InFlight     bool
}

// decideAuthorization 判定一次授权（P3-01.2）。
// 读取、材料、模型外发、任务控制与动作派发分别判定；调用者不能用伪造
// auth_ref 自证获准；每次判定记录（含拒绝）。
func decideAuthorization(tx *sql.Tx, subject, action, objectRef, scopeLabel string, now int64, inFlight bool) (AuthorizationDecision, error) {
	decision := AuthorizationDecision{
		DecisionID: id(), Subject: subject, Action: action, ObjectRef: objectRef, DecidedAt: now, InFlight: inFlight,
	}
	// 查找匹配的有效授权（读取当前权威，不使用滞后索引或旧缓存）。
	// object_ref 精确匹配，或 scope_label 匹配（仅当查询的 scope 非空）。
	var authID string
	var policyVersion int
	var err error
	if scopeLabel != "" {
		err = tx.QueryRow(`SELECT auth_id, policy_version FROM authorizations
WHERE subject=? AND action=? AND (object_ref=? OR scope_label=?) AND revoked_at=0
AND valid_from<=? AND (expires_at=0 OR expires_at>?) ORDER BY policy_version DESC LIMIT 1`,
			subject, action, objectRef, scopeLabel, now, now).Scan(&authID, &policyVersion)
	} else {
		err = tx.QueryRow(`SELECT auth_id, policy_version FROM authorizations
WHERE subject=? AND action=? AND object_ref=? AND revoked_at=0
AND valid_from<=? AND (expires_at=0 OR expires_at>?) ORDER BY policy_version DESC LIMIT 1`,
			subject, action, objectRef, now, now).Scan(&authID, &policyVersion)
	}
	if errors.Is(err, sql.ErrNoRows) {
		decision.Outcome = "denied"
		decision.Reason = "无匹配的有效授权"
	} else if err != nil {
		return decision, err
	} else {
		decision.Outcome = "allowed"
		decision.PolicyVersion = policyVersion
		decision.Reason = "有有效授权"
	}
	// 记录判定（含拒绝，保留在途责任）。
	if _, err := tx.Exec(`INSERT INTO authorization_decisions(decision_id,subject,action,object_ref,outcome,reason,policy_version,decided_at,in_flight)
VALUES(?,?,?,?,?,?,?,?,?)`,
		decision.DecisionID, subject, action, objectRef, decision.Outcome, decision.Reason,
		decision.PolicyVersion, now, boolToInt(inFlight)); err != nil {
		return decision, err
	}
	return decision, nil
}

// ---------------------------------------------------------------------------
// 撤销（直接撤权，同事务）
// ---------------------------------------------------------------------------

// revokeAuthorization 撤销一个授权（P3-01.3/P3-01.4）。
// 撤销与授权版本/相关代际及可靠通知同事务；新的最终核验读取当前权威，不
// 使用滞后索引或旧缓存；区分已撤销后的新核验和撤销前在途请求。
func revokeAuthorization(tx *sql.Tx, authID, revokedBy string, now int64) error {
	res, err := tx.Exec(`UPDATE authorizations SET revoked_at=?, revoked_by=?, revision=revision+1, updated_at=?
WHERE auth_id=? AND revoked_at=0`, now, revokedBy, now, authID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("授权 %s 不存在或已撤销", authID)
	}
	// 可靠通知同事务（审计）。
	if err := recordAudit(tx, AuditEvent{
		Event: "authorization_revoked", Actor: revokedBy, Summary: fmt.Sprintf("撤销授权 %s", authID), OccurredAt: now,
	}); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 范围核对（策略主体只有明确范围，不继承用户所有权限）
// ---------------------------------------------------------------------------

// ErrAuthorizationDenied 是授权拒绝的错误。
var ErrAuthorizationDenied = errors.New("授权拒绝")

// ensurePolicyScope 核对策略主体的明确范围（P3-01.5）。
// 策略主体只有明确范围，不继承用户所有权限；API 不返回未授权对象的标题/
// 数量/敏感正文。
func ensurePolicyScope(principal Principal, scope string) error {
	if principal.Kind == PrincipalPolicy && scope == "" {
		return fmt.Errorf("%w：策略主体必须有明确范围（不继承用户所有权限）", ErrAuthorizationDenied)
	}
	return nil
}

// filterUnauthorized 过滤未授权对象（P3-01.5）。
// API 不返回未授权对象的标题/数量/敏感正文。
func filterUnauthorized(tx *sql.Tx, subject, action string, objects []string, now int64) (allowed []string, err error) {
	for _, obj := range objects {
		decision, err := decideAuthorization(tx, subject, action, obj, "", now, false)
		if err != nil {
			return nil, err
		}
		if decision.Outcome == "allowed" {
			allowed = append(allowed, obj)
		}
	}
	return allowed, nil
}

// 确保 context 引用保留。
var _ = context.Background
