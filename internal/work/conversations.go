package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

var (
	ErrConversationArchived = errors.New("这段交流已归档，请恢复后再发送")
	ErrConversationRevision = errors.New("会话已变更，请刷新后重试")
	ErrConversationTitle    = errors.New("请输入 1 至 60 字的会话标题")
	ErrReadUnconfirmed      = errors.New("请先确认读取结果尚未取得")
)

const conversationColumns = "id,title,archived,revision"

func (s *Store) ResumeConversation(ctx context.Context, conversationID, runID, requestKey string, revision int) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	fingerprint := digest("resume conversation:" + conversationID + ":" + runID)
	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil || previous.ID != "" {
		return previous, err
	}
	if err = checkConversationSubmission(tx, conversationID); err != nil {
		return Run{}, err
	}
	var currentRevision int
	var latestOrdinaryRun bool
	err = tx.QueryRowContext(ctx, `SELECT revision, NOT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=c.id)
AND coalesce((SELECT id=? AND task_id IS NULL FROM runs WHERE conversation_id=c.id AND memory_command=0 ORDER BY rowid DESC LIMIT 1),0)
FROM conversations c WHERE c.id=?`, runID, conversationID).Scan(&currentRevision, &latestOrdinaryRun)
	if err != nil {
		return Run{}, err
	}
	if revision != currentRevision || !latestOrdinaryRun {
		return Run{}, ErrConversationRevision
	}
	checkpoint, err := readResumeStatus(tx, runID)
	if err != nil {
		return Run{}, err
	}
	if checkpoint != "ready" {
		return Run{}, ErrReadCheckpoint
	}
	run, err := resumeCheckpointRun(tx, runID, requestKey, fingerprint)
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func scanConversation(row scanner) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.Title, &c.Archived, &c.Revision)
	return c, err
}

// A nil history means the conversation already belongs to a Task.
func (s *Store) chatSubmissionContext(ctx context.Context, requestKey, fingerprint, conversationID string) (Run, []Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, nil, err
	}
	defer tx.Rollback()
	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil || previous.ID != "" {
		return previous, nil, err
	}
	history := make([]Message, 0, 6)
	if conversationID == "" {
		return Run{}, history, nil
	}
	if err = checkConversationSubmission(tx, conversationID); err != nil {
		return Run{}, nil, err
	}
	var hasTask bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=?)", conversationID).Scan(&hasTask); err != nil || hasTask {
		return Run{}, nil, err
	}
	rows, err := tx.Query(`SELECT role,substr(text,1,1200) FROM (SELECT seq,role,text FROM messages
WHERE conversation_id=? ORDER BY seq DESC LIMIT 6) ORDER BY seq`, conversationID)
	if err != nil {
		return Run{}, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var message Message
		if err = rows.Scan(&message.Role, &message.Text); err != nil {
			return Run{}, nil, err
		}
		history = append(history, message)
	}
	return Run{}, history, rows.Err()
}

func conversationIdle(tx *sql.Tx, id string) error {
	var active bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM runs WHERE conversation_id=? AND status IN ('queued','running'))", id).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrActive
	}
	var taskID string
	err := tx.QueryRow("SELECT id FROM tasks WHERE conversation_id=?", id).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = ensureNoPublication(tx, taskID); err != nil {
		return err
	}
	return ensureReadConfirmed(tx, taskID)
}

func (s *Store) ChangeConversation(ctx context.Context, id string, revision int, title *string, archived *bool) (Conversation, error) {
	if title != nil {
		name := strings.TrimSpace(*title)
		if name == "" || utf8.RuneCountInString(name) > 60 {
			return Conversation{}, ErrConversationTitle
		}
		title = &name
	}
	if revision < 1 || (title == nil && archived == nil) {
		return Conversation{}, ErrConversationRevision
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	current, err := scanConversation(tx.QueryRow("SELECT "+conversationColumns+" FROM conversations WHERE id=?", id))
	if err != nil {
		return current, err
	}
	if current.Revision != revision {
		return current, ErrConversationRevision
	}
	if archived != nil && *archived && !current.Archived {
		if err = conversationIdle(tx, id); err != nil {
			return current, err
		}
	}
	if _, err = tx.Exec("UPDATE conversations SET title=coalesce(?,title),archived=coalesce(?,archived),revision=revision+1 WHERE id=?", title, archived, id); err != nil {
		return current, err
	}
	updated, err := scanConversation(tx.QueryRow("SELECT "+conversationColumns+" FROM conversations WHERE id=?", id))
	if err != nil {
		return updated, err
	}
	return updated, tx.Commit()
}

func (s *Store) DeleteConversation(ctx context.Context, id string, revision int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := scanConversation(tx.QueryRow("SELECT "+conversationColumns+" FROM conversations WHERE id=?", id))
	if err != nil {
		return err
	}
	if c.Revision != revision {
		return ErrConversationRevision
	}
	if err = conversationIdle(tx, id); err != nil {
		return err
	}
	var linked bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=?)", id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		if _, err = tx.Exec("UPDATE conversations SET archived=1,revision=revision+1 WHERE id=?", id); err != nil {
			return err
		}
		return tx.Commit()
	}
	for _, query := range []string{
		"UPDATE memories SET source_deleted=1,revision=revision+1 WHERE source_message IN (SELECT seq FROM messages WHERE conversation_id=?)",
		"DELETE FROM message_materials WHERE message_seq IN (SELECT seq FROM messages WHERE conversation_id=?)",
		"DELETE FROM messages WHERE conversation_id=?",
		"DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE conversation_id=?)",
		"INSERT OR IGNORE INTO deleted_submissions SELECT request_key FROM submissions WHERE run_id IN (SELECT id FROM runs WHERE conversation_id=?)",
		"DELETE FROM submissions WHERE run_id IN (SELECT id FROM runs WHERE conversation_id=?)",
		"DELETE FROM runs WHERE conversation_id=?",
		"DELETE FROM conversations WHERE id=?",
	} {
		if _, err = tx.Exec(query, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func unknownReadRun(tx *sql.Tx, taskID string) (string, error) {
	var runID string
	err := tx.QueryRow(`SELECT r.id FROM runs r JOIN run_reads d ON d.run_id=r.id
WHERE r.task_id=? AND d.status='unknown' AND d.acknowledged_at=0 ORDER BY r.rowid DESC LIMIT 1`, taskID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return runID, err
}

func ensureReadConfirmed(tx *sql.Tx, taskID string) error {
	id, err := unknownReadRun(tx, taskID)
	if err == nil && id != "" {
		return ErrReadUnconfirmed
	}
	return err
}

func (s *Store) AcknowledgeRead(ctx context.Context, taskID, runID string, revision int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
	if err != nil {
		return err
	}
	if task.Revision != revision || task.Status == "active" {
		return ErrState
	}
	res, err := tx.Exec(`UPDATE run_reads SET acknowledged_at=? WHERE run_id=? AND status='unknown' AND acknowledged_at=0
AND EXISTS(SELECT 1 FROM runs WHERE id=? AND task_id=?)`, now(), runID, runID, taskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	remaining, err := unknownReadRun(tx, taskID)
	if err != nil {
		return err
	}
	if remaining == "" && task.StatusReason == "reconciliation_required" {
		var control string
		var checkpointed bool
		if err = tx.QueryRow(`SELECT control,EXISTS(SELECT 1 FROM run_read_checkpoints WHERE run_id=runs.id) FROM runs WHERE id=?`, runID).
			Scan(&control, &checkpointed); err != nil {
			return err
		}
		if control == "stop" && checkpointed {
			if _, err = tx.Exec("UPDATE runs SET status='cancelled',error='已停止',error_code='stop' WHERE id=?", runID); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE tasks SET status='cancelled',status_reason='stopped',reason='已停止',revision=revision+1,updated_at=? WHERE id=?", now(), taskID); err != nil {
				return err
			}
			if err = addEvent(tx, runID, "run.state_changed", map[string]string{"status": "cancelled", "error_code": "stop"}); err != nil {
				return err
			}
		} else if _, err = tx.Exec("UPDATE tasks SET status_reason='read_unavailable',reason='已记录读取结果未取得',revision=revision+1,updated_at=? WHERE id=?", now(), taskID); err != nil {
			return err
		}
	}
	if err = addEvent(tx, runID, "run_read.acknowledged", map[string]string{"status": "unknown"}); err != nil {
		return err
	}
	return tx.Commit()
}
