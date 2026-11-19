package work

// P2-02：原子领取、续租与过期回收。
//
// 本文件实现同一执行单元同一时刻只有一个有效推进者，失联可恢复：
//   - Claim 在短事务核对 Task/Step、合同与控制修订、依赖、工作者范围和额度，
//     条件更新领取并增加 lease_generation；
//   - 签发绑定 unit_id、worker_id、版本和期限的不可伪造令牌；令牌不包含外部凭据；
//   - Renew 以控制面时间验证原租约、代际与身份，过期令牌不能续租；新领取
//     不复用旧令牌；
//   - 区分 Task 的 assignment_generation 和每个单元的 lease_generation，避免
//     一个并行 Step 重领就无故废止其他独立 Step；
//   - 失联回收先判断是否有已派发操作；未知外部效果进入核对，不能自动重新发出。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 租约类型
// ---------------------------------------------------------------------------

// Lease 是 work_leases 的一行。
type Lease struct {
	LeaseID         string
	UnitID          string
	TaskID          string
	LeaseGeneration int
	WorkerID        string
	TokenID         string
	IssuedAt        int64
	LeaseUntil      int64
	State           string // active/expired/returned/reclaimed
	ReclaimReason   string
	ReclaimedAt     int64
	DispatchedOps   int
}

// ---------------------------------------------------------------------------
// Claim（短事务条件更新领取）
// ---------------------------------------------------------------------------

// ClaimRequest 是领取请求。
type ClaimRequest struct {
	WorkerID   string
	Credential string
	Kind       WorkUnitKind
	Scope      string
	LeaseTTL   int64 // 租约期限（毫秒）
	// QuotaAvailable 是可用额度（由调用方核对，P2-04 接入持久配额）。
	QuotaAvailable bool
}

// ClaimResult 是领取结果。
type ClaimResult struct {
	Lease      Lease
	Token      WorkerToken
	PlainToken string // 明文令牌（只返回给调用方，不落库）
	Unit       WorkUnit
}

// claimWorkUnit 原子领取一个就绪单元（P2-02.1）。
// 短事务：核对工作者范围/额度 → 条件更新单元状态 → 增加 lease_generation →
// 签发令牌。两个工作者竞争同一单元仅一个领取成功；不同单元可分别领取。
func claimWorkUnit(tx *sql.Tx, req ClaimRequest, now int64) (ClaimResult, error) {
	// 1. 认证与范围核对。
	worker, err := authenticateWorker(tx, req.WorkerID, req.Credential, now)
	if err != nil {
		return ClaimResult{}, err
	}
	if err := ensureWorkerScope(worker, req.Kind, req.Scope); err != nil {
		return ClaimResult{}, err
	}
	if !req.QuotaAvailable {
		return ClaimResult{}, fmt.Errorf("%w：无可用额度", ErrQuotaExhausted)
	}
	// 2. Filter against the persisted task, step and worker scope before claiming.
	units, err := selectReadyUnits(tx, req.Kind, req.Scope, now, 1)
	if err != nil {
		return ClaimResult{}, err
	}
	if len(units) == 0 {
		return ClaimResult{}, fmt.Errorf("%w：无就绪单元", sql.ErrNoRows)
	}
	unit := units[0]
	// 3. 核对 Task/Step 状态与合同（条件更新领取）。
	newGeneration := unit.LeaseGeneration + 1
	res, err := tx.Exec(`UPDATE work_units SET state='claimed', lease_generation=?, updated_at=?
WHERE unit_id=? AND state='ready' AND lease_generation=?`,
		newGeneration, now, unit.UnitID, unit.LeaseGeneration)
	if err != nil {
		return ClaimResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// 竞争失败：另一工作者已取得（同一单元同一时刻只有一个有效推进者）。
		return ClaimResult{}, fmt.Errorf("%w：单元已被其他工作者领取", ErrLeaseConflict)
	}
	// 4. 签发令牌（绑定单元与代际，不含外部凭据）。
	token, plainToken, err := issueToken(tx, req.WorkerID, unit.UnitID, newGeneration, req.LeaseTTL, now)
	if err != nil {
		return ClaimResult{}, err
	}
	// 5. 记录租约。
	lease := Lease{
		LeaseID:         id(),
		UnitID:          unit.UnitID,
		TaskID:          unit.TaskID,
		LeaseGeneration: newGeneration,
		WorkerID:        req.WorkerID,
		TokenID:         token.TokenID,
		IssuedAt:        now,
		LeaseUntil:      now + req.LeaseTTL,
		State:           "active",
	}
	if _, err := tx.Exec(`INSERT INTO work_leases(lease_id,unit_id,task_id,lease_generation,worker_id,token_id,issued_at,lease_until,state)
VALUES(?,?,?,?,?,?,?,?,?)`,
		lease.LeaseID, lease.UnitID, lease.TaskID, lease.LeaseGeneration, lease.WorkerID,
		lease.TokenID, lease.IssuedAt, lease.LeaseUntil, lease.State); err != nil {
		return ClaimResult{}, err
	}
	// 6. 更新最近联系与审计。
	if err := touchWorker(tx, req.WorkerID, now); err != nil {
		return ClaimResult{}, err
	}
	if err := recordAudit(tx, AuditEvent{
		TaskID: unit.TaskID, UnitID: unit.UnitID, Event: "lease_claimed", Actor: req.WorkerID,
		Summary: fmt.Sprintf("领取单元（代际 %d）", newGeneration), OccurredAt: now,
	}); err != nil {
		return ClaimResult{}, err
	}
	unit.LeaseGeneration = newGeneration
	unit.State = "claimed"
	return ClaimResult{Lease: lease, Token: token, PlainToken: plainToken, Unit: unit}, nil
}

// ErrLeaseConflict 是领取竞争失败的错误。
var ErrLeaseConflict = errors.New("租约竞争失败")

// ErrQuotaExhausted 是额度耗尽的错误（P2-04 接入持久配额）。
var ErrQuotaExhausted = errors.New("额度耗尽")

// ---------------------------------------------------------------------------
// Renew（控制面时间验证原租约、代际与身份）
// ---------------------------------------------------------------------------

// renewLease 续租（P2-02.3）。
// 以控制面时间验证原租约、代际与身份：过期令牌不能续租；重复 Renew 不创造
// 第二份额度；新领取不复用旧令牌。
func renewLease(tx *sql.Tx, tokenID, plainToken string, now int64) (Lease, error) {
	// 核对令牌（过期不能续租）。
	token, err := validateToken(tx, tokenID, plainToken, now)
	if err != nil {
		return Lease{}, err
	}
	// 读取租约（核对代际与身份）。
	var lease Lease
	err = tx.QueryRow(`SELECT lease_id,unit_id,task_id,lease_generation,worker_id,token_id,issued_at,lease_until,state
FROM work_leases WHERE token_id=? AND state='active'`, tokenID).
		Scan(&lease.LeaseID, &lease.UnitID, &lease.TaskID, &lease.LeaseGeneration,
			&lease.WorkerID, &lease.TokenID, &lease.IssuedAt, &lease.LeaseUntil, &lease.State)
	if err != nil {
		return Lease{}, fmt.Errorf("%w：租约不存在或已失效（过期令牌不能续租）", ErrLeaseConflict)
	}
	// 代际与身份核对。
	if lease.LeaseGeneration != token.LeaseGeneration || lease.WorkerID != token.WorkerID {
		return Lease{}, fmt.Errorf("%w：令牌与租约代际/身份不匹配", ErrLeaseConflict)
	}
	// 续租（控制面时间裁决，延长 lease_until；不创造第二份额度）。
	newUntil := now + (lease.LeaseUntil - lease.IssuedAt)
	res, err := tx.Exec(`UPDATE work_leases SET lease_until=? WHERE lease_id=? AND state='active' AND lease_until>?`,
		newUntil, lease.LeaseID, now)
	if err != nil {
		return Lease{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Lease{}, fmt.Errorf("%w：租约已过期不能续租", ErrLeaseConflict)
	}
	lease.LeaseUntil = newUntil
	return lease, nil
}

// ---------------------------------------------------------------------------
// 过期回收（失联先判断已派发操作）
// ---------------------------------------------------------------------------

// ReclaimResult 是回收结果。
type ReclaimResult struct {
	Reclaimed int
	// PendingOps 是回收时发现的已派发未核对操作数（未知外部效果进入核对）。
	PendingOps int
	// Reason 是回收原因。
	Reason string
}

// reclaimExpiredLeases 回收过期租约（P2-02.5）。
// 失联回收先判断是否有已派发操作；未知外部效果进入核对，不能自动重新发出；
// 控制面重启后根据持久租约恢复，不能依赖内存 map 判断所有权。
func reclaimExpiredLeases(tx *sql.Tx, now int64, limit int) (ReclaimResult, error) {
	// 读取过期 active 租约（分批）。
	rows, err := tx.Query(`SELECT lease_id,unit_id,task_id,worker_id FROM work_leases
WHERE state='active' AND lease_until<=? LIMIT ?`, now, limit)
	if err != nil {
		return ReclaimResult{}, err
	}
	var leases []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.LeaseID, &l.UnitID, &l.TaskID, &l.WorkerID); err != nil {
			rows.Close()
			return ReclaimResult{}, err
		}
		leases = append(leases, l)
	}
	rows.Close()
	var result ReclaimResult
	for _, lease := range leases {
		// 判断是否有已派发操作（未知外部效果进入核对，不自动重新发出）。
		var dispatchedOps int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM work_units WHERE unit_id=? AND state='running'`, lease.UnitID).Scan(&dispatchedOps); err != nil {
			return result, err
		}
		// 回收租约：标记 reclaimed，单元回到 ready（可重新核对领取）。
		res, err := tx.Exec(`UPDATE work_leases SET state='reclaimed', reclaimed_at=?, reclaim_reason=?, dispatched_ops=?
WHERE lease_id=? AND state='active' AND lease_until<=?`,
			now, "租约过期失联", dispatchedOps, lease.LeaseID, now)
		if err != nil {
			return result, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue // 并发回收：另一回收者已取得
		}
		// 单元回到 ready（可重新核对领取；已派发操作的效果另行核对）。
		if _, err := tx.Exec(`UPDATE work_units SET state='ready', updated_at=? WHERE unit_id=? AND state IN ('claimed','running')`,
			now, lease.UnitID); err != nil {
			return result, err
		}
		// 审计：失联回收与已派发操作数（未知外部效果进入核对）。
		if err := recordAudit(tx, AuditEvent{
			TaskID: lease.TaskID, UnitID: lease.UnitID, Event: "lease_reclaimed", Actor: "system",
			Summary: fmt.Sprintf("回收过期租约（已派发操作 %d，进入核对）", dispatchedOps), OccurredAt: now,
		}); err != nil {
			return result, err
		}
		result.Reclaimed++
		result.PendingOps += dispatchedOps
	}
	result.Reason = "租约过期失联回收"
	return result, nil
}

// ---------------------------------------------------------------------------
// 查询（重启后按持久租约恢复）
// ---------------------------------------------------------------------------

// activeLeaseForUnit 查询一个单元的当前有效租约（P2-02 反例：重启后按持久
// 租约恢复，不依赖内存 map 判断所有权）。
func activeLeaseForUnit(q recordsQuerier, unitID string, now int64) (*Lease, error) {
	var l Lease
	err := q.QueryRow(`SELECT lease_id,unit_id,task_id,lease_generation,worker_id,token_id,issued_at,lease_until,state
FROM work_leases WHERE unit_id=? AND state='active' AND lease_until>? ORDER BY lease_generation DESC LIMIT 1`,
		unitID, now).
		Scan(&l.LeaseID, &l.UnitID, &l.TaskID, &l.LeaseGeneration, &l.WorkerID, &l.TokenID, &l.IssuedAt, &l.LeaseUntil, &l.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// 确保 context 引用保留。
var _ = context.Background
