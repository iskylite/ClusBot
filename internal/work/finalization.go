package work

import (
	"context"
	"database/sql"
	"errors"
)

// Save the provider outcome separately from the business transaction. The
// response deltas are already committed to runs; this receipt records that the
// stream ended, including its question/error and uncommitted usage.
func (s *Store) persistFinalization(ctx context.Context, run Run, status, failure string, accounting runAccounting) error {
	var tokens TokenUsage
	if accounting.tokens != nil {
		tokens = *accounting.tokens
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_finalizations
(run_id,execution_generation,status,failure,question,active_ms,error_code,input_tokens,output_tokens,plan)
SELECT id,execution_generation,?,?,?,?,?,?,?,? FROM runs
WHERE id=? AND status='running' AND execution_generation=?
ON CONFLICT(run_id,execution_generation) DO NOTHING`,
		status, failure, accounting.question, accounting.activeMS, accounting.errorCode,
		tokens.InputTokens, tokens.OutputTokens, accounting.plan, run.ID, run.Generation)
	if err != nil {
		return err
	}
	err = s.finishContext(ctx, run, status, failure, accounting)
	if errors.Is(err, sql.ErrNoRows) {
		// A control/recovery transition superseded this generation. Discard only
		// its receipt; never let a late outcome advance the current execution.
		if _, deleteErr := s.db.ExecContext(ctx, "DELETE FROM run_finalizations WHERE run_id=? AND execution_generation=?", run.ID, run.Generation); deleteErr != nil {
			return deleteErr
		}
	}
	return err
}

func (s *Store) recoverFinalizations() error {
	rows, err := s.db.Query(`SELECT f.run_id,coalesce(r.task_id,''),r.conversation_id,
f.execution_generation,f.status,f.failure,f.question,f.active_ms,f.error_code,f.input_tokens,f.output_tokens,f.plan
FROM run_finalizations f JOIN runs r ON r.id=f.run_id ORDER BY f.rowid`)
	if err != nil {
		return err
	}
	var pending []runFinalization
	for rows.Next() {
		var outcome runFinalization
		var tokens TokenUsage
		if err = rows.Scan(&outcome.run.ID, &outcome.run.TaskID, &outcome.run.ConversationID,
			&outcome.run.Generation, &outcome.status, &outcome.failure, &outcome.accounting.question,
			&outcome.accounting.activeMS, &outcome.accounting.errorCode, &tokens.InputTokens, &tokens.OutputTokens, &outcome.accounting.plan); err != nil {
			rows.Close()
			return err
		}
		outcome.accounting.tokens = &tokens
		pending = append(pending, outcome)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Close the cursor before writing: Store uses a single SQLite connection.
	for _, outcome := range pending {
		err = s.persistFinalization(context.Background(), outcome.run, outcome.status, outcome.failure, outcome.accounting)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}
