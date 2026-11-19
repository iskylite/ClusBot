package work

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
)

var ErrConversationEventCursorReset = errors.New("会话事件游标无法续读，请重新读取会话")

const conversationEventPageSize = 64

type ConversationEvent struct {
	ID     string `json:"id"`
	RunID  string `json:"run_id"`
	TaskID string `json:"task_id,omitempty"`
	Kind   string `json:"kind"`
	Status string `json:"status,omitempty"`
}

type ConversationEventPage struct {
	Events  []ConversationEvent `json:"events"`
	Cursor  string              `json:"cursor"`
	HasMore bool                `json:"has_more"`
}

type conversationEventCursor struct {
	Sequence int64  `json:"s"`
	EventID  string `json:"e"`
}

const conversationEventsQuery = `SELECT e.seq,r.id,
 COALESCE((SELECT t.id FROM tasks t WHERE t.id=r.task_id AND t.conversation_id=r.conversation_id), ''),
 e.kind, CASE WHEN e.kind='run.state_changed' AND json_valid(e.data)
  THEN COALESCE(json_extract(e.data,'$.status'),'') ELSE '' END
FROM runs r JOIN run_events e INDEXED BY events_by_run ON e.run_id=r.id
WHERE r.conversation_id=:conversation AND (
 e.kind='run.state_changed'
 OR (e.kind='benchmark.admission_suggested' AND EXISTS (
  SELECT 1 FROM inbox_events i JOIN submissions sub ON sub.request_key=i.event_id
  WHERE i.source='chat' AND i.run_id=r.id AND i.state='applied' AND i.principal=:actor AND sub.run_id=r.id
 ))
 OR (e.kind IN ('benchmark.revision_suggested','benchmark.revision_preview') AND EXISTS (
  SELECT 1 FROM benchmark_runs b WHERE b.actor=:actor
  AND b.task_id=CASE WHEN json_valid(e.data) THEN json_extract(e.data,'$.task_id') END
 ))
)`

func scanConversationEvent(row scanner, conversation string) (ConversationEvent, int64, error) {
	var e ConversationEvent
	var seq int64
	if err := row.Scan(&seq, &e.RunID, &e.TaskID, &e.Kind, &e.Status); err != nil {
		return ConversationEvent{}, 0, err
	}
	switch e.Status {
	case "queued", "running", "completed", "interrupted", "cancelled":
	default:
		e.Status = ""
	}
	// This proof is over the safe event projection, not raw errors, messages,
	// or material. It detects an unrelated anchor at the same cursor position.
	e.ID = digest(toJSON([]any{conversation, seq, e.RunID, e.Kind, e.Status}))
	return e, seq, nil
}

// ReadConversationEvents exposes a retained, allowlisted receipt history,
// not model streaming or a feed for operations outside this conversation.
// Existing conversations are owned by the authenticated local app principal.
func (s *Store) ReadConversationEvents(ctx context.Context, actor, conversation, cursor string) (ConversationEventPage, error) {
	if actor != "user:session" {
		return ConversationEventPage{}, ErrAuthorizationDenied
	}
	var anchor conversationEventCursor
	if cursor != "" {
		if len(cursor) > 192 {
			return ConversationEventPage{}, ErrConversationEventCursorReset
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &anchor) != nil || anchor.Sequence <= 0 || len(anchor.EventID) != 64 {
			return ConversationEventPage{}, ErrConversationEventCursorReset
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationEventPage{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM conversations WHERE id=?", conversation).Scan(&exists); err != nil {
		return ConversationEventPage{}, err
	}
	params := []any{sql.Named("conversation", conversation), sql.Named("actor", actor)}
	if anchor.Sequence != 0 {
		row := tx.QueryRowContext(ctx, conversationEventsQuery+" AND e.seq=:sequence", append(params, sql.Named("sequence", anchor.Sequence))...)
		event, _, err := scanConversationEvent(row, conversation)
		if errors.Is(err, sql.ErrNoRows) || err == nil && event.ID != anchor.EventID {
			return ConversationEventPage{}, ErrConversationEventCursorReset
		}
		if err != nil {
			return ConversationEventPage{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, conversationEventsQuery+" AND e.seq>:sequence ORDER BY e.seq LIMIT :limit",
		append(params, sql.Named("sequence", anchor.Sequence), sql.Named("limit", conversationEventPageSize+1))...)
	if err != nil {
		return ConversationEventPage{}, err
	}
	defer rows.Close()
	page := ConversationEventPage{Events: []ConversationEvent{}, Cursor: cursor}
	for rows.Next() {
		if len(page.Events) == conversationEventPageSize {
			page.HasMore = true
			break
		}
		event, seq, err := scanConversationEvent(rows, conversation)
		if err != nil {
			return ConversationEventPage{}, err
		}
		anchor = conversationEventCursor{Sequence: seq, EventID: event.ID}
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return ConversationEventPage{}, err
	}
	if err := rows.Close(); err != nil {
		return ConversationEventPage{}, err
	}
	if len(page.Events) != 0 {
		page.Cursor = base64.RawURLEncoding.EncodeToString([]byte(toJSON(anchor)))
	}
	return page, tx.Commit()
}
