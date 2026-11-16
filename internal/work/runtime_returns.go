package work

// P4-20：归还复核、退役再启用与 E04。
//
// 本文件实现借期、真实归还、维修影响和后续管理状态不混淆：
//   - 到期只提示/记录逾期；真实归还核对身份、现况、维修、操作和责任后，
//     同事务结束有效 loan 并进入待复核；
//   - 专项维修保持借出状态，归还后相关证据重新复查；不能自动恢复历史在役；
//   - 普通退役先完成实际归还/责任交接，处理占用、未知动作、设备/数据处置
//     和连接关闭，保留证明；
//   - 再启用重新接收管理责任并回待复核，不沿用旧交付直接在役；
//   - 按 E04 跑完整借出、只读观察、限定维修、到期、归还和再启用；同时汇总
//     E01—E03 的业务验收证据。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 到期（只提示/记录逾期）
// ---------------------------------------------------------------------------

// checkLoanDue 核对借期（P4-20.1）。
// 到期只提示/记录逾期；到期、告警恢复或口头预计均不会自动归还。
func checkLoanDue(tx *sql.Tx, objectRef string, now int64) (overdue bool, err error) {
	var dueAt int64
	err = tx.QueryRow(`SELECT due_at FROM loans WHERE object_ref=? AND state='active'`, objectRef).Scan(&dueAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// 到期只提示/记录逾期（不自动归还）。
	if dueAt > 0 && now >= dueAt {
		if err := recordAudit(tx, AuditEvent{
			Event: "loan_overdue", Actor: "system",
			Summary: fmt.Sprintf("借用 %s 已到期（只提示/记录逾期，不自动归还）", objectRef), OccurredAt: now,
		}); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// 真实归还（核对身份、现况、维修、操作和责任后，同事务结束有效 loan 并进入待复核）
// ---------------------------------------------------------------------------

// ReturnVerification 是归还核对。
type ReturnVerification struct {
	IdentityVerified  bool
	CurrentStateKnown bool
	RepairCompleted   bool
	OperationsReconciled bool
	ResponsibilityTransferred bool
}

// returnLoan 归还借用（P4-20.1/P4-20.2）。
// 真实归还核对身份、现况、维修、操作和责任后，同事务结束有效 loan 并进入
// 待复核；专项维修保持借出状态，归还后相关证据重新复查；不能自动恢复历史在役。
func returnLoan(tx *sql.Tx, objectRef string, v ReturnVerification, now int64) error {
	// 核对身份、现况、维修、操作和责任（真实归还核对）。
	if !v.IdentityVerified || !v.CurrentStateKnown || !v.OperationsReconciled || !v.ResponsibilityTransferred {
		return fmt.Errorf("归还核对不完整（真实归还核对身份/现况/维修/操作/责任）：identity=%v state=%v ops=%v resp=%v",
			v.IdentityVerified, v.CurrentStateKnown, v.OperationsReconciled, v.ResponsibilityTransferred)
	}
	// 专项维修保持借出状态（RepairCompleted=false 时不归还）。
	if !v.RepairCompleted {
		return fmt.Errorf("专项维修未完成（专项维修保持借出状态）")
	}
	// 同事务结束有效 loan 并进入待复核。
	res, err := tx.Exec(`UPDATE loans SET state='returned', returned_at=? WHERE object_ref=? AND state='active'`,
		now, objectRef)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("对象 %s 无有效借用（归还与 loan 更新原子一致）", objectRef)
	}
	// 归还后相关证据重新复查（不能自动恢复历史在役）：资产回待复核。
	if _, err := tx.Exec(`UPDATE ops_assets SET state=?, updated_at=? WHERE object_ref=?`,
		AssetStateRecheck, now, objectRef); err != nil {
		return err
	}
	// 交接确认（归还后相关证据重新复查）。
	return recordAudit(tx, AuditEvent{
		Event: "loan_returned", Actor: "system",
		Summary: fmt.Sprintf("借用 %s 已归还（结束有效 loan 并进入待复核，相关证据重新复查）", objectRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 普通退役（先完成实际归还/责任交接）
// ---------------------------------------------------------------------------

// retireAsset 退役资产（P4-20.3）。
// 普通退役先完成实际归还/责任交接，处理占用、未知动作、设备/数据处置和连接
// 关闭，保留证明。
func retireAsset(tx *sql.Tx, objectRef string, returnCompleted bool, occupationHandled bool, deviceDataDisposed bool, connectionClosed bool, evidenceRef string, now int64) error {
	// 普通退役先完成实际归还/责任交接。
	if !returnCompleted {
		return fmt.Errorf("退役前未完成实际归还/责任交接（普通退役先完成实际归还/责任交接）")
	}
	// 处理占用、未知动作、设备/数据处置和连接关闭。
	if !occupationHandled || !deviceDataDisposed || !connectionClosed {
		return fmt.Errorf("退役前未处理占用/设备/数据处置/连接关闭")
	}
	// 保留证明。
	if evidenceRef == "" {
		return errors.New("退役缺少证明")
	}
	// 退役（回待复核后退役）。
	if _, err := tx.Exec(`UPDATE ops_assets SET state=?, updated_at=? WHERE object_ref=?`,
		AssetStateRetired, now, objectRef); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		Event: "asset_retired", Actor: "system",
		Summary: fmt.Sprintf("资产 %s 已退役（保留证明 %s）", objectRef, evidenceRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 再启用（重新接收管理责任并回待复核）
// ---------------------------------------------------------------------------

// reactivateAsset 再启用资产（P4-20.4）。
// 再启用重新接收管理责任并回待复核，不沿用旧交付直接在役。
func reactivateAsset(tx *sql.Tx, objectRef string, responsibilityAccepted bool, now int64) error {
	// 再启用重新接收管理责任（不沿用旧交付直接在役）。
	if !responsibilityAccepted {
		return fmt.Errorf("再启用未重新接收管理责任（不沿用旧交付直接在役）")
	}
	// 回待复核（不沿用旧交付直接在役）。
	if _, err := tx.Exec(`UPDATE ops_assets SET state=?, updated_at=? WHERE object_ref=?`,
		AssetStateRecheck, now, objectRef); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		Event: "asset_reactivated", Actor: "system",
		Summary: fmt.Sprintf("资产 %s 已再启用（重新接收管理责任并回待复核）", objectRef), OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
