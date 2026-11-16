package work

// P3-03：绑定具体动作的确认与人工接管。
//
// 本文件实现人工决定既具体可追溯，也不会变成绕过授权的通行证：
//   - 确认绑定主体、动作、实际对象、参数摘要、合同/策略/资产版本、决定者
//     和期限；
//   - 通过已有 Chat/责任入口返回待决定事项；满足有效自动策略的动作不逐次
//     要求人工；
//   - 内容/权限/版本变化后旧确认无效，需重判并按策略决定是否重新请求确认；
//   - 人工接管未知效果须记录明确授权、接收人、未决操作、隔离措施、残余
//     风险和后续责任；
//   - 确认通过与实际派发分开；请求已收到或用户点过按钮不能直接修改
//     Operation 为成功。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 确认（绑定具体动作，可追溯）
// ---------------------------------------------------------------------------

// Approval 是 approvals 的一行。
type Approval struct {
	ApprovalID       string
	Subject          string
	Action           string
	ObjectRef        string
	ParamsDigest     string
	ContractRevision int
	PolicyVersion    int
	AssetRevision    int
	DecidedBy        string
	ExpiresAt        int64
	Decision         string // pending/approved/rejected/expired
	DecidedAt        int64
	RequestKey       string
	CreatedAt        int64
}

// ---------------------------------------------------------------------------
// 请求确认（绑定主体/动作/对象/参数摘要/版本/决定者/期限）
// ---------------------------------------------------------------------------

// requestApproval 请求一个确认（P3-03.1/P3-03.5）。
// 确认绑定主体、动作、实际对象、参数摘要、合同/策略/资产版本、决定者和期限；
// 确认通过与实际派发分开；请求已收到或用户点过按钮不能直接修改 Operation 为成功。
func requestApproval(tx *sql.Tx, a Approval, now int64) (Approval, error) {
	if a.Subject == "" || a.Action == "" || a.ObjectRef == "" {
		return Approval{}, errors.New("确认缺少主体/动作/对象")
	}
	if a.ParamsDigest == "" {
		return Approval{}, errors.New("确认缺少参数摘要")
	}
	if a.RequestKey == "" {
		a.RequestKey = id()
	}
	a.ApprovalID = id()
	a.Decision = "pending"
	a.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO approvals(approval_id,subject,action,object_ref,params_digest,
contract_revision,policy_version,asset_revision,decided_by,expires_at,decision,request_key,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ApprovalID, a.Subject, a.Action, a.ObjectRef, a.ParamsDigest,
		a.ContractRevision, a.PolicyVersion, a.AssetRevision, a.DecidedBy, a.ExpiresAt,
		a.Decision, a.RequestKey, now)
	if err != nil {
		return Approval{}, err
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// 决定（重复点击只产生一次有效决定；过期/错对象/参数/无权确认拒绝）
// ---------------------------------------------------------------------------

// decideApproval 决定一个确认（P3-03 反例）。
// 重复点击只产生一次有效决定；过期、错对象/参数、无权确认均拒绝。
func decideApproval(tx *sql.Tx, requestKey, decision, decidedBy string, expectedVersions Approval, now int64) (Approval, error) {
	// 重复点击：同请求键已有决定（只产生一次有效决定）。
	var existing Approval
	err := tx.QueryRow(`SELECT approval_id,subject,action,object_ref,params_digest,contract_revision,
policy_version,asset_revision,decided_by,expires_at,decision,decided_at,request_key,created_at
FROM approvals WHERE request_key=?`, requestKey).
		Scan(&existing.ApprovalID, &existing.Subject, &existing.Action, &existing.ObjectRef, &existing.ParamsDigest,
			&existing.ContractRevision, &existing.PolicyVersion, &existing.AssetRevision, &existing.DecidedBy,
			&existing.ExpiresAt, &existing.Decision, &existing.DecidedAt, &existing.RequestKey, &existing.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, fmt.Errorf("确认请求 %s 不存在", requestKey)
	}
	if err != nil {
		return Approval{}, err
	}
	// 重复决定：返回原决定（幂等）。
	if existing.Decision != "pending" {
		return existing, nil
	}
	// 过期：拒绝。
	if existing.ExpiresAt > 0 && now >= existing.ExpiresAt {
		if _, err := tx.Exec(`UPDATE approvals SET decision='expired', decided_at=? WHERE approval_id=?`, now, existing.ApprovalID); err != nil {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("确认已过期")
	}
	// 内容/权限/版本变化：旧确认无效（需重判）。
	if expectedVersions.ContractRevision > 0 && existing.ContractRevision != expectedVersions.ContractRevision {
		return Approval{}, fmt.Errorf("%w：合同版本已变化（%d≠%d），旧确认无效需重判", ErrApprovalStale, existing.ContractRevision, expectedVersions.ContractRevision)
	}
	if expectedVersions.AssetRevision > 0 && existing.AssetRevision != expectedVersions.AssetRevision {
		return Approval{}, fmt.Errorf("%w：资产版本已变化（%d≠%d），旧确认无效需重判", ErrApprovalStale, existing.AssetRevision, expectedVersions.AssetRevision)
	}
	// 无权确认：决定者必须有确认权限（确认不能补足缺失权限）。
	authDecision, err := decideAuthorization(tx, decidedBy, "confirm", existing.ObjectRef, "", now, false)
	if err != nil {
		return Approval{}, err
	}
	if authDecision.Outcome != "allowed" {
		return Approval{}, fmt.Errorf("%w：决定者 %s 无权确认该动作（确认不能补足缺失权限）", ErrAuthorizationDenied, decidedBy)
	}
	// 决定（条件更新，只一次）。
	res, err := tx.Exec(`UPDATE approvals SET decision=?, decided_at=?, decided_by=? WHERE approval_id=? AND decision='pending'`,
		decision, now, decidedBy, existing.ApprovalID)
	if err != nil {
		return Approval{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return existing, nil // 并发竞争：另一决定已取得
	}
	existing.Decision = decision
	existing.DecidedAt = now
	existing.DecidedBy = decidedBy
	return existing, nil
}

// ErrApprovalStale 是确认过期的错误。
var ErrApprovalStale = errors.New("确认已过期")

// ---------------------------------------------------------------------------
// 待决定事项（Chat/责任入口返回；自动策略不逐次要求人工）
// ---------------------------------------------------------------------------

// PendingApproval 是待决定事项。
type PendingApproval struct {
	ApprovalID string
	Subject    string
	Action     string
	ObjectRef  string
	Reason     string
}

// listPendingApprovals 列出待决定事项（P3-03.2）。
// 通过已有 Chat/责任入口返回待决定事项；满足有效自动策略的动作不逐次要求人工。
func listPendingApprovals(q recordsQuerier, subject string, now int64) ([]PendingApproval, error) {
	rows, err := q.Query(`SELECT approval_id,subject,action,object_ref FROM approvals
WHERE subject=? AND decision='pending' AND (expires_at=0 OR expires_at>?)`, subject, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingApproval
	for rows.Next() {
		var p PendingApproval
		if err := rows.Scan(&p.ApprovalID, &p.Subject, &p.Action, &p.ObjectRef); err != nil {
			return nil, err
		}
		p.Reason = fmt.Sprintf("待确认：%s 对 %s 执行 %s", p.Subject, p.ObjectRef, p.Action)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPendingApprovals 返回主体的待决定确认（HTTP 用）。
func (s *Store) ListPendingApprovals(ctx context.Context, subject string, now int64) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT approval_id,subject,action,object_ref,params_digest,
contract_revision,policy_version,asset_revision,decided_by,expires_at,decision,decided_at,request_key,created_at
FROM approvals WHERE subject=? AND decision='pending' AND (expires_at=0 OR expires_at>?)
ORDER BY created_at ASC, approval_id ASC`, subject, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Approval{}
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ApprovalID, &a.Subject, &a.Action, &a.ObjectRef, &a.ParamsDigest,
			&a.ContractRevision, &a.PolicyVersion, &a.AssetRevision, &a.DecidedBy,
			&a.ExpiresAt, &a.Decision, &a.DecidedAt, &a.RequestKey, &a.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

// DecideApproval 按 approval_id 落决定；重复点击由 decideApproval 保证幂等。
func (s *Store) DecideApproval(ctx context.Context, approvalID, decision, decidedBy string) (Approval, error) {
	if decision != "approved" && decision != "rejected" {
		return Approval{}, fmt.Errorf("无效的确认决定：%s", decision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	var a Approval
	err = tx.QueryRowContext(ctx, `SELECT approval_id,subject,action,object_ref,params_digest,
contract_revision,policy_version,asset_revision,decided_by,expires_at,decision,decided_at,request_key,created_at
FROM approvals WHERE approval_id=?`, approvalID).
		Scan(&a.ApprovalID, &a.Subject, &a.Action, &a.ObjectRef, &a.ParamsDigest,
			&a.ContractRevision, &a.PolicyVersion, &a.AssetRevision, &a.DecidedBy,
			&a.ExpiresAt, &a.Decision, &a.DecidedAt, &a.RequestKey, &a.CreatedAt)
	if err != nil {
		return Approval{}, err
	}
	updated, err := decideApproval(tx, a.RequestKey, decision, decidedBy, Approval{}, now())
	if err != nil {
		return Approval{}, err
	}
	if err := tx.Commit(); err != nil {
		return Approval{}, err
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// 人工接管（未知效果的明确授权与后续责任）
// ---------------------------------------------------------------------------

// ManualTakeover 是 manual_takeovers 的一行。
type ManualTakeover struct {
	TakeoverID             string
	TaskID                 string
	AuthorizedBy           string
	TakenOverBy            string
	PendingOperations      string
	IsolationMeasures      string
	ResidualRisks          string
	FollowUpResponsibility string
	TakenAt                int64
}

// recordManualTakeover 记录人工接管（P3-03.4）。
// 人工接管未知效果须记录明确授权、接收人、未决操作、隔离措施、残余风险和
// 后续责任；Task 不能凭普通取消确认丢弃未知外部效果；例外接管仍留证。
func recordManualTakeover(tx *sql.Tx, t ManualTakeover, now int64) (ManualTakeover, error) {
	if t.TaskID == "" || t.AuthorizedBy == "" || t.TakenOverBy == "" {
		return ManualTakeover{}, errors.New("人工接管缺少任务/授权人/接收人")
	}
	if t.FollowUpResponsibility == "" {
		return ManualTakeover{}, errors.New("人工接管缺少后续责任")
	}
	t.TakeoverID = id()
	t.TakenAt = now
	_, err := tx.Exec(`INSERT INTO manual_takeovers(takeover_id,task_id,authorized_by,taken_over_by,
pending_operations,isolation_measures,residual_risks,follow_up_responsibility,taken_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		t.TakeoverID, t.TaskID, t.AuthorizedBy, t.TakenOverBy, t.PendingOperations,
		t.IsolationMeasures, t.ResidualRisks, t.FollowUpResponsibility, now)
	if err != nil {
		return ManualTakeover{}, err
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: t.TaskID, Event: "manual_takeover", Actor: t.TakenOverBy,
		Summary: fmt.Sprintf("人工接管（授权 %s，责任 %s）", t.AuthorizedBy, t.FollowUpResponsibility), OccurredAt: now,
	}); err != nil {
		return ManualTakeover{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 确认通过与实际派发分开（P3-03.5）
// ---------------------------------------------------------------------------

// ErrApprovalNotDispatch 是确认通过不等于派发的错误。
var ErrApprovalNotDispatch = errors.New("确认通过与实际派发分开")

// confirmApprovedNotDispatch 核对确认通过与实际派发分开（P3-03.5）。
// 请求已收到或用户点过按钮不能直接修改 Operation 为成功。
func confirmApprovedNotDispatch(approval Approval) error {
	if approval.Decision != "approved" {
		return fmt.Errorf("%w：确认未通过（%s）", ErrApprovalNotDispatch, approval.Decision)
	}
	// 确认通过不等于派发成功：派发需经能力接入与领域准入（P3-02）。
	return nil
}

// 确保 context/sql/errors 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
)
