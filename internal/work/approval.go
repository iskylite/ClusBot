package work

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

var ErrApprovalExpired = errors.New("审批已过期，请重新检查目标并发起审批")

func (s *Store) ExpireApprovals(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now()
	rows, err := tx.QueryContext(ctx, "SELECT "+publicationColumns+" FROM publications WHERE status='pending' AND expires_at<=?", stamp)
	if err != nil {
		return err
	}
	var due []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return err
		}
		due = append(due, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range due {
		if err = savePublication(tx, expiredPublication(p)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StartApprovalWorker(ctx context.Context) *sync.WaitGroup {
	var worker sync.WaitGroup
	worker.Go(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.ExpireApprovals(ctx); err != nil && ctx.Err() == nil {
					slog.Error("expire approvals", "error", err)
				}
			}
		}
	})
	return &worker
}
