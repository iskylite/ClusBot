package work

import (
	"database/sql"
	"errors"
)

func deferTaskFollowup(tx *sql.Tx, task Task, key, fingerprint, text string) (Run, error) {
	run, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE task_id=? AND status IN ('queued','running')", task.ID))
	if err != nil {
		return Run{}, err
	}
	var control string
	if err = tx.QueryRow("SELECT control FROM runs WHERE id=?", run.ID).Scan(&control); err != nil {
		return Run{}, err
	}
	if control == "stop" {
		return Run{}, ErrState
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM task_followups WHERE task_id=?
AND dispatched_run_id IS NULL AND cancelled_at IS NULL`, task.ID).Scan(&count); err != nil {
		return Run{}, err
	}
	if count >= 16 {
		return Run{}, ErrActive
	}
	if _, err = tx.Exec("INSERT INTO submissions VALUES(?,?,?)", key, fingerprint, run.ID); err != nil {
		return Run{}, err
	}
	if _, err = tx.Exec("INSERT INTO task_followups(request_key,task_id,text,accepted_at) VALUES(?,?,?,?)",
		key, task.ID, text, now()); err != nil {
		return Run{}, err
	}
	run.FollowupQueued = true
	return run, nil
}

// Promote one accepted follow-up at the decision boundary. Inserting the next
// Run in the finishing transaction prevents a restart from losing the handoff.
func dispatchTaskFollowup(tx *sql.Tx, previous Run) error {
	var key, text string
	var acceptedAt int64
	err := tx.QueryRow(`SELECT request_key,text,accepted_at FROM task_followups WHERE task_id=?
AND dispatched_run_id IS NULL AND cancelled_at IS NULL ORDER BY accepted_at,rowid LIMIT 1`,
		previous.TaskID).Scan(&key, &text, &acceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	next := Run{ID: id(), TaskID: previous.TaskID, ConversationID: previous.ConversationID}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,'queued',?)",
		next.ID, next.ConversationID, next.TaskID, now()); err != nil {
		return err
	}
	if err = reuseTaskRead(tx, next.TaskID, next.ID, false); err != nil {
		return err
	}
	res, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)", next.ConversationID, next.ID, text)
	if err != nil {
		return err
	}
	message, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO input_events VALUES(?,?,?)", message, key, acceptedAt); err != nil {
		return err
	}
	if err = attachDeferredClassification(tx, key, next.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE task_followups SET dispatched_run_id=? WHERE request_key=?", next.ID, key); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='active',status_reason='queued',reason='补充要求已受理',
completed_at=NULL,revision=revision+1,updated_at=? WHERE id=?`, now(), next.TaskID); err != nil {
		return err
	}
	return addEvent(tx, next.ID, "run.state_changed", map[string]string{"status": "queued"})
}

func attachDeferredClassification(tx *sql.Tx, key, runID string) error {
	classification := submissionClassification{DelegationResult: DelegationResult{Task: true}}
	var tokens TokenUsage
	err := tx.QueryRow(`SELECT requests,active_ms,output_used,input_tokens,output_tokens
FROM submission_classifications WHERE request_key=? AND state='completed'`, key).Scan(
		&classification.Requests, &classification.activeMS, &classification.outputUsed, &tokens.InputTokens, &tokens.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	classification.Usage = &tokens
	return attachClassification(tx, runID, key, classification)
}
