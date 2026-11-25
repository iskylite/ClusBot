package work

import (
	"context"
	"database/sql"
	"errors"
)

// TaskListActions is a conservative snapshot. Mutations still recheck the
// task revision and all execution guards when the user confirms an action.
type TaskListActions struct {
	CanRename    bool   `json:"can_rename"`
	CanCancel    bool   `json:"can_cancel"`
	StatusAction string `json:"status_action,omitempty"`
}

func (s *Store) TaskListActions(ctx context.Context, task Task) (TaskListActions, error) {
	var actions TaskListActions
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return actions, err
	}
	defer tx.Rollback()
	ordinary := requireOrdinaryTaskFollowup(tx, task.ID) == nil
	clear := ensureNoPublication(tx, task.ID) == nil && ensureReadConfirmed(tx, task.ID) == nil
	actions.CanRename = ordinary && clear && conversationIdle(tx, task.ConversationID) == nil

	var runID, runStatus, control string
	var incomplete, hasArtifact bool
	err = tx.QueryRowContext(ctx, `SELECT id,status,control,
EXISTS(SELECT 1 FROM run_reads d WHERE d.run_id=runs.id AND d.status NOT IN ('completed','reused')),
EXISTS(SELECT 1 FROM artifacts a WHERE a.run_id=runs.id)
FROM runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1`, task.ID).Scan(&runID, &runStatus, &control, &incomplete, &hasArtifact)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return actions, err
	}
	actions.CanCancel = ordinary && clear && runID != "" && control == "" &&
		(task.Status == "active" || task.Status == "waiting_user" || task.Status == "interrupted") &&
		task.StatusReason != "pause_requested" && task.StatusReason != "stop_requested"
	switch {
	case ordinary && clear && task.Status == "active" && runID != "" &&
		(runStatus == "queued" || runStatus == "running") && control == "" && task.StatusReason != "pause_requested" && task.StatusReason != "stop_requested":
		actions.StatusAction = "pause"
	case ordinary && clear && task.Status == "waiting_user" && task.StatusReason == "acceptance_required" && runStatus == "completed" && hasArtifact:
		// Acceptance opens the full work view, where the current artifact and
		// any delivery checks can be reviewed before confirmation.
		actions.StatusAction = "accept"
	case ordinary && clear && (task.Status == "interrupted" || task.Status == "cancelled") &&
		conversationIdle(tx, task.ConversationID) == nil:
		if task.Status == "cancelled" || task.RequirementsChanged {
			actions.StatusAction = "resume"
		} else if runID != "" {
			checkpoint, checkpointErr := readResumeStatus(tx, runID)
			if checkpointErr != nil {
				return actions, checkpointErr
			}
			switch {
			case checkpoint == "retry":
				actions.StatusAction = "retry"
			case checkpoint == "ready" || checkpoint == "" && !incomplete:
				actions.StatusAction = "resume"
			}
		}
	case task.Status == "completed":
		if _, _, err := completedTaskRerunInputs(ctx, tx, task.ID, false); err == nil {
			actions.StatusAction = "rerun"
		}
	}
	return actions, nil
}
