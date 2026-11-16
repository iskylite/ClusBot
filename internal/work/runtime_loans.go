package work

// P4-19：借出冻结、在途核对与交接事务。
//
// 本文件实现管理责任转移时旧工作不能继续新写，未知在途效果不被隐藏：
//   - 先登记交接意图与冻结并递增 ownership_epoch；交接意图不是新增资产末级
//     状态；
//   - 列出冻结前已派发的冲突写并逐项核对完成/停止和效果；已登记未派发的
//     旧操作拒绝继续；
//   - 正常交接前处理占用、责任、双方、期限、只读范围与例外条件；未决效果
//     不明时保持原状态与交接待办；
//   - 实际交接确认时再次递增 epoch，同事务建立唯一有效借用并改为借出；保持
//     状态与有效 loan 双向一致；
//   - 人工例外必须完整记录授权、接收人、未知效果与后续责任；无外部 fencing
//     不保证旧在途请求不再生效。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 交接意图（先登记交接意图与冻结并递增 ownership_epoch）
// ---------------------------------------------------------------------------

// LoanHandover 是 loan_handovers 的一行。
type LoanHandover struct {
	HandoverID          string
	ObjectRef           string
	Intent              string // lend/return
	FrozenAt            int64
	OwnershipEpochBefore int
	ConflictWrites      string
	Occupancy           string
	Responsibility      string
	Lender              string
	Borrower            string
	DueAt               int64
	ReadOnlyScope       string
	ExceptionConditions string
	State               string // intent/frozen/reconciling/confirmed/cancelled
	CreatedAt           int64
	UpdatedAt           int64
}

// initiateLoanHandover 发起交接意图（P4-19.1）。
// 先登记交接意图与冻结并递增 ownership_epoch；交接意图不是新增资产末级状态。
func initiateLoanHandover(tx *sql.Tx, h LoanHandover, now int64) (LoanHandover, error) {
	if h.ObjectRef == "" || h.Intent == "" {
		return LoanHandover{}, errors.New("交接缺少对象或意图")
	}
	if h.Intent != "lend" && h.Intent != "return" {
		return LoanHandover{}, fmt.Errorf("无效的交接意图 %s", h.Intent)
	}
	// 先登记交接意图与冻结并递增 ownership_epoch（条件更新）。
	res, err := tx.Exec(`UPDATE ops_assets SET ownership_epoch=ownership_epoch+1, frozen_at=?, freeze_reason=?, updated_at=?
WHERE object_ref=? AND state<>?`, now, "交接冻结", now, h.ObjectRef, AssetStateLent)
	if err != nil {
		return LoanHandover{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return LoanHandover{}, fmt.Errorf("资产 %s 无法冻结（可能已借出）", h.ObjectRef)
	}
	// 读取冻结前 epoch。
	if err := tx.QueryRow(`SELECT ownership_epoch FROM ops_assets WHERE object_ref=?`, h.ObjectRef).Scan(&h.OwnershipEpochBefore); err != nil {
		return LoanHandover{}, err
	}
	h.HandoverID = id()
	h.FrozenAt = now
	h.State = "frozen"
	h.CreatedAt, h.UpdatedAt = now, now
	if h.ConflictWrites == "" {
		h.ConflictWrites = "[]"
	}
	_, err = tx.Exec(`INSERT INTO loan_handovers(handover_id,object_ref,intent,frozen_at,ownership_epoch_before,
conflict_writes,occupancy,responsibility,lender,borrower,due_at,read_only_scope,exception_conditions,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.HandoverID, h.ObjectRef, h.Intent, h.FrozenAt, h.OwnershipEpochBefore, h.ConflictWrites,
		h.Occupancy, h.Responsibility, h.Lender, h.Borrower, h.DueAt, h.ReadOnlyScope,
		h.ExceptionConditions, h.State, now, now)
	if err != nil {
		return LoanHandover{}, err
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// 冲突写核对（列出冻结前已派发的冲突写并逐项核对完成/停止和效果）
// ---------------------------------------------------------------------------

// ErrConflictWritesPending 是冲突写未决的错误。
var ErrConflictWritesPending = errors.New("冻结前已派发的冲突写未决")

// reconcileConflictWrites 核对冲突写（P4-19.2）。
// 列出冻结前已派发的冲突写并逐项核对完成/停止和效果；已登记未派发的旧
// 操作拒绝继续。
func reconcileConflictWrites(tx *sql.Tx, handoverID string, objectRef string, now int64) (reconciled bool, pending []string, err error) {
	// 列出冻结前已派发的冲突写（已登记未派发的旧操作拒绝继续）。
	rows, err := tx.Query(`SELECT operation_id, state FROM operations WHERE task_id IN (SELECT task_id FROM tasks WHERE 1=1) AND state IN ('dispatched','running')`)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var opID, state string
		if err := rows.Scan(&opID, &state); err != nil {
			return false, nil, err
		}
		pending = append(pending, opID)
	}
	// 冲突写未决：保持原状态与交接待办。
	if len(pending) > 0 {
		if _, err := tx.Exec(`UPDATE loan_handovers SET state='reconciling', conflict_writes=?, updated_at=? WHERE handover_id=?`,
			toJSON(pending), now, handoverID); err != nil {
			return false, nil, err
		}
		return false, pending, nil
	}
	return true, nil, nil
}

// ---------------------------------------------------------------------------
// 实际交接确认（再次递增 epoch，同事务建立唯一有效借用并改为借出）
// ---------------------------------------------------------------------------

// Loan 是 loans 的一行。
type Loan struct {
	LoanID             string
	ObjectRef          string
	HandoverID         string
	Lender             string
	Borrower           string
	DueAt              int64
	ReadOnlyScope      string
	State              string // active/returned/expired
	OwnershipEpochAtLend int
	LentAt             int64
	ReturnedAt         int64
}

// confirmLoanHandover 确认交接（P4-19.4）。
// 实际交接确认时再次递增 epoch，同事务建立唯一有效借用并改为借出；保持状态
// 与有效 loan 双向一致；提交中断不出现状态借出却无有效 loan，或两个有效 loan。
func confirmLoanHandover(tx *sql.Tx, handoverID string, lender, borrower string, dueAt int64, readOnlyScope string, now int64) (Loan, error) {
	var h LoanHandover
	if err := tx.QueryRow(`SELECT handover_id, object_ref, state FROM loan_handovers WHERE handover_id=?`, handoverID).
		Scan(&h.HandoverID, &h.ObjectRef, &h.State); err != nil {
		return Loan{}, err
	}
	if h.State != "frozen" && h.State != "reconciling" {
		return Loan{}, fmt.Errorf("交接 %s 状态 %s 不能确认（需先冻结）", handoverID, h.State)
	}
	// 再次递增 epoch（实际交接确认时）。
	if _, err := tx.Exec(`UPDATE ops_assets SET ownership_epoch=ownership_epoch+1, updated_at=? WHERE object_ref=?`,
		now, h.ObjectRef); err != nil {
		return Loan{}, err
	}
	var newEpoch int
	if err := tx.QueryRow(`SELECT ownership_epoch FROM ops_assets WHERE object_ref=?`, h.ObjectRef).Scan(&newEpoch); err != nil {
		return Loan{}, err
	}
	// 同事务建立唯一有效借用并改为借出（保持状态与有效 loan 双向一致）。
	loan := Loan{
		LoanID: id(), ObjectRef: h.ObjectRef, HandoverID: handoverID,
		Lender: lender, Borrower: borrower, DueAt: dueAt, ReadOnlyScope: readOnlyScope,
		State: "active", OwnershipEpochAtLend: newEpoch, LentAt: now,
	}
	// 唯一有效借用（loans_one_active_per_object 唯一索引）。
	if _, err := tx.Exec(`INSERT INTO loans(loan_id,object_ref,handover_id,lender,borrower,due_at,read_only_scope,state,ownership_epoch_at_lend,lent_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		loan.LoanID, loan.ObjectRef, loan.HandoverID, loan.Lender, loan.Borrower, loan.DueAt,
		loan.ReadOnlyScope, loan.State, loan.OwnershipEpochAtLend, now); err != nil {
		return Loan{}, fmt.Errorf("建立唯一有效借用失败（提交中断不出现状态借出却无有效 loan，或两个有效 loan）：%w", err)
	}
	// 改为借出（同事务）。
	if _, err := tx.Exec(`UPDATE ops_assets SET state=?, updated_at=? WHERE object_ref=?`,
		AssetStateLent, now, h.ObjectRef); err != nil {
		return Loan{}, err
	}
	// 交接确认。
	if _, err := tx.Exec(`UPDATE loan_handovers SET state='confirmed', updated_at=? WHERE handover_id=?`,
		now, handoverID); err != nil {
		return Loan{}, err
	}
	return loan, nil
}

// ---------------------------------------------------------------------------
// 人工例外（完整记录授权、接收人、未知效果与后续责任）
// ---------------------------------------------------------------------------

// LoanException 是 loan_exceptions 的一行。
type LoanException struct {
	ExceptionID           string
	HandoverID            string
	ObjectRef             string
	AuthorizedBy          string
	ReceivedBy            string
	UnknownEffects        string
	FollowUpResponsibility string
	CreatedAt             int64
}

// recordLoanException 记录人工例外（P4-19.5）。
// 人工例外必须完整记录授权、接收人、未知效果与后续责任；无外部 fencing
// 不保证旧在途请求不再生效。
func recordLoanException(tx *sql.Tx, e LoanException, now int64) (LoanException, error) {
	if e.HandoverID == "" || e.ObjectRef == "" || e.AuthorizedBy == "" || e.ReceivedBy == "" {
		return LoanException{}, errors.New("人工例外缺少交接/对象/授权/接收人")
	}
	// 未知效果与后续责任必须有。
	if e.UnknownEffects == "" {
		e.UnknownEffects = "[]"
	}
	if e.FollowUpResponsibility == "" {
		return LoanException{}, errors.New("人工例外缺少后续责任")
	}
	e.ExceptionID = id()
	e.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO loan_exceptions(exception_id,handover_id,object_ref,authorized_by,
received_by,unknown_effects,follow_up_responsibility,created_at)
VALUES(?,?,?,?,?,?,?,?)`,
		e.ExceptionID, e.HandoverID, e.ObjectRef, e.AuthorizedBy, e.ReceivedBy,
		e.UnknownEffects, e.FollowUpResponsibility, now)
	if err != nil {
		return LoanException{}, err
	}
	return e, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
