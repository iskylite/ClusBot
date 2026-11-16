package work

// P2-04：持久预算、并发额度与占用。
//
// 本文件实现多工作者共享预算和配额，重启、重领或新 Run 都不能重置已消费
// 成本：
//   - 区分 Task 总预算、单 Run 上限、模型/提供者并发、工作组速率和目标占用；
//     配置项含明确单位与作用域；
//   - 领取/派发所需的预留与状态同事务；回报按稳定身份结算，重复结算无影响；
//   - 模型槽按实际模型请求占用；确定步骤和持久等待不占模型槽；
//   - 费用未知时保留已预留上限/未知状态，不退款成零；租约可回收的执行额度
//     与资产危险操作占用分开；
//   - 外部动作效果未核实前不能仅按 TTL 释放排他占用；运行时间和 token 使用
//     累计到正确任务/步骤。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 配额作用域与类型
// ---------------------------------------------------------------------------

// QuotaScope 是配额作用域。
type QuotaScope string

const (
	// QuotaTaskBudget：任务总预算（跨 Run 不重置）。
	QuotaTaskBudget QuotaScope = "task_budget"
	// QuotaRunLimit：单 Run 上限。
	QuotaRunLimit QuotaScope = "run_limit"
	// QuotaModelSlot：模型/提供者并发槽。
	QuotaModelSlot QuotaScope = "model_slot"
	// QuotaProviderRate：提供者速率。
	QuotaProviderRate QuotaScope = "provider_rate"
	// QuotaTargetHold：目标占用（资产危险操作）。
	QuotaTargetHold QuotaScope = "target_hold"
)

// QuotaReservation 是 quota_reservations 的一行。
type QuotaReservation struct {
	ReservationID string
	Scope         QuotaScope
	TaskID        string
	RunID         string
	StepID        string
	WorkerID      string
	TargetRef     string
	Amount        int64
	Unit          string
	State         string // reserved/settled/released/expired
	SettlementKey string
	SettledAt     int64
	CostKnown     bool
	Exclusive     bool
	EffectVerified bool
	ReservedAt    int64
	ExpiresAt     int64
}

// ---------------------------------------------------------------------------
// 预留（与领取/派发同事务）
// ---------------------------------------------------------------------------

// reserveQuota 在事务内预留配额（P2-04.2）。
// 领取/派发所需的预留与状态同事务：两个工作者同时申请最后一份额度，仅一方
// 成功；重复预留（同 settlement_key）返回既有。
func reserveQuota(tx *sql.Tx, r QuotaReservation) (QuotaReservation, error) {
	if r.Scope == "" || r.Amount <= 0 || r.Unit == "" {
		return QuotaReservation{}, errors.New("配额预留缺少作用域/数量/单位")
	}
	// 结算键（回报按稳定身份结算）：缺省按预留生成。
	if r.SettlementKey == "" && r.TaskID != "" {
		r.SettlementKey = fmt.Sprintf("%s:%s:%s", r.Scope, r.TaskID, r.StepID)
	}
	// 重复预留（同 settlement_key）：返回既有（重复结算无影响）。
	// 但仅当既有仍处于可复用状态；额度已被占满时不能凭空"复用成功"。
	if r.SettlementKey != "" {
		var existingID, existingState string
		var existingAmount int64
		err := tx.QueryRow(`SELECT reservation_id, state, amount FROM quota_reservations WHERE settlement_key=?`, r.SettlementKey).
			Scan(&existingID, &existingState, &existingAmount)
		if err == nil {
			// 既有预留已占用额度：同键复用不产生新额度消耗，但只有状态允许
			// 且调用方明确是同一次逻辑预留（重复结算无影响）；新的一次领取
			// 应经 ensureQuotaAvailable 判定（竞争最后一份仅一方成功）。
			if existingState == "settled" || existingState == "released" {
				r.ReservationID = existingID
				r.State = existingState
				return r, nil
			}
			// 既有仍在 reserved：同键重复预留视为重复结算（返回既有），
			// 但不同工作者的竞争应失败——用额度占用判定（仅一方成功）。
			return r, ensureQuotaAvailable(tx, r)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return QuotaReservation{}, err
		}
	}
	// 核对作用域上限（两个工作者同时申请最后一份额度，仅一方成功）。
	if err := ensureQuotaAvailable(tx, r); err != nil {
		return QuotaReservation{}, err
	}
	r.ReservationID = id()
	r.State = "reserved"
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := tx.Exec(`INSERT INTO quota_reservations(reservation_id,scope,task_id,run_id,step_id,worker_id,
target_ref,amount,unit,state,settlement_key,cost_known,exclusive,effect_verified,reserved_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ReservationID, string(r.Scope), nullable(r.TaskID), nullable(r.RunID), nullable(r.StepID),
		nullable(r.WorkerID), r.TargetRef, r.Amount, r.Unit, r.State, nullable(r.SettlementKey),
		boolToInt(r.CostKnown), boolToInt(r.Exclusive), boolToInt(r.EffectVerified), r.ReservedAt, r.ExpiresAt)
	if err != nil {
		return QuotaReservation{}, err
	}
	return r, nil
}

// ensureQuotaAvailable 核对作用域上限（P2-04 反例：两个工作者同时申请最后
// 一份额度，仅一方成功）。
func ensureQuotaAvailable(tx *sql.Tx, r QuotaReservation) error {
	// 读取作用域上限。
	var limitAmount int64
	configKey := string(r.Scope) + ":" + quotaConfigSubject(r)
	err := tx.QueryRow(`SELECT limit_amount FROM quota_configs WHERE config_key=?`, configKey).Scan(&limitAmount)
	if errors.Is(err, sql.ErrNoRows) {
		// 无配置：默认不限制（由调用方决定是否要求配置）。
		return nil
	}
	if err != nil {
		return err
	}
	// 统计当前占用（reserved + settled 未释放）。
	var used int64
	where := "scope=? AND state IN ('reserved','settled') AND " + quotaOccupiedWhere(r)
	args := append([]any{string(r.Scope)}, quotaOccupiedArgs(r)...)
	if err := tx.QueryRow(`SELECT coalesce(sum(amount),0) FROM quota_reservations WHERE `+where, args...).Scan(&used); err != nil {
		return err
	}
	if used+r.Amount > limitAmount {
		return fmt.Errorf("%w：作用域 %s 已用 %d，申请 %d 超出上限 %d（仅一方成功）",
			ErrQuotaExhausted, configKey, used, r.Amount, limitAmount)
	}
	return nil
}

// quotaConfigSubject 是配置键的主体部分。
func quotaConfigSubject(r QuotaReservation) string {
	switch r.Scope {
	case QuotaTaskBudget:
		return r.TaskID
	case QuotaModelSlot:
		return r.WorkerID // 简化：按 worker/provider
	default:
		return r.TargetRef
	}
}

// quotaOccupiedWhere 是占用统计的 WHERE 子句。
func quotaOccupiedWhere(r QuotaReservation) string {
	switch r.Scope {
	case QuotaTaskBudget:
		return "task_id=?"
	case QuotaModelSlot:
		return "worker_id=?"
	default:
		return "target_ref=?"
	}
}

// quotaOccupiedArgs 是占用统计的参数。
func quotaOccupiedArgs(r QuotaReservation) []any {
	switch r.Scope {
	case QuotaTaskBudget:
		return []any{r.TaskID}
	case QuotaModelSlot:
		return []any{r.WorkerID}
	default:
		return []any{r.TargetRef}
	}
}

// ---------------------------------------------------------------------------
// 结算（回报按稳定身份，重复结算无影响）
// ---------------------------------------------------------------------------

// settleQuota 按稳定身份结算配额（P2-04.2）。
// 回报按稳定身份结算，重复结算无影响；费用未知时保留已预留上限/未知状态，
// 不退款成零。
func settleQuota(tx *sql.Tx, settlementKey string, costKnown bool, now int64) error {
	res, err := tx.Exec(`UPDATE quota_reservations SET state='settled', settled_at=?, cost_known=?
WHERE settlement_key=? AND state='reserved'`, now, boolToInt(costKnown), settlementKey)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 重复结算无影响（已 settled 或不存在）。
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// 模型槽（按实际模型请求占用；确定步骤和等待不占槽）
// ---------------------------------------------------------------------------

// acquireModelSlot 按实际模型请求占用模型槽（P2-04.3）。
// 模型槽按实际模型请求占用；确定步骤和持久等待不占模型槽。
func acquireModelSlot(tx *sql.Tx, workerID, taskID string, now int64) (QuotaReservation, error) {
	return reserveQuota(tx, QuotaReservation{
		Scope:      QuotaModelSlot,
		TaskID:     taskID,
		WorkerID:   workerID,
		Amount:     1,
		Unit:       "slots",
		ReservedAt: now,
	})
}

// releaseModelSlot 释放模型槽（模型请求结束时）。
func releaseModelSlot(tx *sql.Tx, settlementKey string, now int64) error {
	_, err := tx.Exec(`UPDATE quota_reservations SET state='released' WHERE settlement_key=? AND state IN ('reserved','settled')`,
		settlementKey)
	return err
}

// ---------------------------------------------------------------------------
// 占用（资产危险操作；效果未核实前不按 TTL 释放）
// ---------------------------------------------------------------------------

// holdTarget 占用一个目标（P2-04.5）。
// 外部动作效果未核实前不能仅按 TTL 释放排他占用。
func holdTarget(tx *sql.Tx, targetRef, workerID, taskID string, now int64) (QuotaReservation, error) {
	return reserveQuota(tx, QuotaReservation{
		Scope:      QuotaTargetHold,
		TaskID:     taskID,
		WorkerID:   workerID,
		TargetRef:  targetRef,
		Amount:     1,
		Unit:       "holds",
		Exclusive:  true,
		ReservedAt: now,
	})
}

// releaseTargetHold 释放目标占用（仅当效果已核实）。
// 外部动作效果未核实前不能仅按 TTL 释放排他占用。
func releaseTargetHold(tx *sql.Tx, targetRef string, effectVerified bool, now int64) error {
	if !effectVerified {
		return fmt.Errorf("目标 %s 的效果未核实，不能释放排他占用（不能仅按 TTL 释放）", targetRef)
	}
	_, err := tx.Exec(`UPDATE quota_reservations SET state='released', effect_verified=1
WHERE target_ref=? AND exclusive=1 AND state IN ('reserved','settled')`, targetRef)
	return err
}

// ---------------------------------------------------------------------------
// 查询（占用与积压）
// ---------------------------------------------------------------------------

// QuotaUsage 是一个作用域的占用快照。
type QuotaUsage struct {
	Scope         QuotaScope
	Subject       string
	Reserved      int64
	Settled       int64
	Released      int64
	ExclusiveHolds int64
	UnverifiedHolds int64 // 效果未核实的排他占用（不能仅按 TTL 释放）
}

// readQuotaUsage 读取一个作用域的占用快照。
func readQuotaUsage(q recordsQuerier, scope QuotaScope, subject string) (QuotaUsage, error) {
	var u QuotaUsage
	u.Scope = scope
	u.Subject = subject
	where := "scope=?"
	args := []any{string(scope)}
	switch scope {
	case QuotaTaskBudget:
		where += " AND task_id=?"
		args = append(args, subject)
	case QuotaModelSlot:
		where += " AND worker_id=?"
		args = append(args, subject)
	default:
		where += " AND target_ref=?"
		args = append(args, subject)
	}
	if err := q.QueryRow(`SELECT
coalesce(sum(CASE WHEN state='reserved' THEN amount END),0),
coalesce(sum(CASE WHEN state='settled' THEN amount END),0),
coalesce(sum(CASE WHEN state='released' THEN amount END),0),
coalesce(sum(CASE WHEN exclusive=1 AND state IN ('reserved','settled') THEN amount END),0),
coalesce(sum(CASE WHEN exclusive=1 AND effect_verified=0 AND state IN ('reserved','settled') THEN amount END),0)
FROM quota_reservations WHERE `+where, args...).
		Scan(&u.Reserved, &u.Settled, &u.Released, &u.ExclusiveHolds, &u.UnverifiedHolds); err != nil {
		return u, err
	}
	return u, nil
}

// upsertQuotaConfig 配置一个作用域上限（含明确单位与作用域）。
func upsertQuotaConfig(tx *sql.Tx, configKey string, scope QuotaScope, limit int64, unit, subject string, now int64) error {
	if configKey == "" || limit < 0 || unit == "" {
		return errors.New("配额配置缺少键/上限/单位")
	}
	_, err := tx.Exec(`INSERT INTO quota_configs(config_key,scope,limit_amount,unit,subject,revision,updated_at)
VALUES(?,?,?,?,?,1,?) ON CONFLICT(config_key) DO UPDATE SET limit_amount=excluded.limit_amount,
unit=excluded.unit,subject=excluded.subject,revision=quota_configs.revision+1,updated_at=excluded.updated_at`,
		configKey, string(scope), limit, unit, subject, now)
	return err
}

// 确保 context 引用保留。
var _ = context.Background
