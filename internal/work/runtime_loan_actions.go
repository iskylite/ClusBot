package work

import (
	"context"
	"fmt"
)

// LendAsset 发起并确认一次借用（冻结、核对冲突写、建立唯一有效 loan、转借出）。
func (s *Store) LendAsset(ctx context.Context, objectRef, lender, borrower string, dueAt int64, readOnlyScope string) (Loan, error) {
	if objectRef == "" || lender == "" || borrower == "" || dueAt <= 0 {
		return Loan{}, fmt.Errorf("借用需要对象、借出方、借入方与到期时间")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Loan{}, err
	}
	defer tx.Rollback()
	// Like benchmark claim, decide against durable ownership in the same
	// transaction as the asset write. A missing provider reply, expired local
	// execution lease or restart does not make a held asset lendable.
	held, err := targetHasLocalExclusiveHold(tx, objectRef)
	if err != nil {
		return Loan{}, err
	}
	if held {
		return Loan{}, fmt.Errorf("%w：资产仍被运行中或待核对的操作占用，不能借出", ErrConflict)
	}
	h, err := initiateLoanHandover(tx, LoanHandover{
		ObjectRef: objectRef, Intent: "lend", Lender: lender, Borrower: borrower,
		DueAt: dueAt, ReadOnlyScope: readOnlyScope,
		Occupancy: "待核对", Responsibility: "借用期间由借入方承担；只读监测保留",
	}, now())
	if err != nil {
		return Loan{}, err
	}
	reconciled, pending, err := reconcileConflictWrites(tx, h.HandoverID, objectRef, now())
	if err != nil {
		return Loan{}, err
	}
	if !reconciled {
		return Loan{}, fmt.Errorf("借用前存在未核对的冲突写：%v", pending)
	}
	loan, err := confirmLoanHandover(tx, h.HandoverID, lender, borrower, dueAt, readOnlyScope, now())
	if err != nil {
		return Loan{}, err
	}
	return loan, tx.Commit()
}

// ReturnAsset 完成真实归还核对并结束有效借用。
func (s *Store) ReturnAsset(ctx context.Context, objectRef string, v ReturnVerification) error {
	if objectRef == "" {
		return fmt.Errorf("归还缺少对象")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := returnLoan(tx, objectRef, v, now()); err != nil {
		return err
	}
	return tx.Commit()
}
