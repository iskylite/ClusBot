package work

import "context"

// StopConversation targets a specific ordinary Run. Replaying the request never
// controls a newer Run, and completed Runs retain their actual terminal outcome.
func (s *Store) StopConversation(ctx context.Context, conversationID, runID string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=? AND conversation_id=? AND task_id IS NULL", runID, conversationID))
	if err != nil {
		return Run{}, err
	}
	if run.Status != "queued" && run.Status != "running" {
		return run, nil
	}
	// Once control is committed, delta rejects further fragments. Save the
	// already committed fragment in the same transaction, without editing it.
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET control='stop' WHERE id=?", runID); err != nil {
		return Run{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(conversation_id,run_id,role,text)
SELECT conversation_id,id,'assistant',response FROM runs WHERE id=? AND response<>''
AND NOT EXISTS(SELECT 1 FROM messages WHERE run_id=? AND role='assistant')`, runID, runID); err != nil {
		return Run{}, err
	}
	if run.Status == "queued" {
		if _, err = tx.ExecContext(ctx, "UPDATE run_reads SET status='not_run',detail='已停止' WHERE run_id=? AND status='queued'", runID); err != nil {
			return Run{}, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE runs SET status='cancelled',error='已停止',error_code='stop',usage_tracked=1 WHERE id=?", runID); err != nil {
			return Run{}, err
		}
		if err = addEvent(tx, runID, "run.state_changed", map[string]string{"status": "cancelled", "error_code": "stop"}); err != nil {
			return Run{}, err
		}
	}
	if err = cancelConversationInputs(tx, conversationID, runID); err != nil {
		return Run{}, err
	}
	run, err = scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=?", runID))
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}
