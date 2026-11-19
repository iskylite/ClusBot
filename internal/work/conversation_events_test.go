package work

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationEventPagingIsolationAndRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "conversation-events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	left, err := s.Submit(ctx, "reconnect-task-left", "left private material", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.Submit(ctx, "reconnect-task-right", "right private material", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "user:session"
	write := func(run, kind string, data any) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := addEvent(tx, run, kind, data); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < conversationEventPageSize; i++ {
		write(left.ID, "run.state_changed", map[string]string{"status": "running", "error": "private error"})
		write(right.ID, "run.state_changed", map[string]string{"status": "interrupted"})
	}
	write(left.ID, "message.delta", map[string]string{"delta": "private token"})
	write(left.ID, "benchmark.revision_preview", map[string]string{"task_id": right.TaskID})
	page := func(who, conversation, cursor string) (ConversationEventPage, error) {
		return s.ReadConversationEvents(ctx, who, conversation, cursor)
	}
	one, err := page(actor, left.ConversationID, "")
	if err != nil || !one.HasMore || len(one.Events) != conversationEventPageSize || one.Cursor == "" {
		t.Fatalf("first page is not bounded: %+v %v", one, err)
	}
	if _, err := page("user:anonymous", left.ConversationID, ""); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("anonymous access to feed: %v", err)
	}
	if _, err := page(actor, right.ConversationID, one.Cursor); !errors.Is(err, ErrConversationEventCursorReset) {
		t.Fatalf("another conversation reused cursor: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	two, err := page(actor, left.ConversationID, one.Cursor)
	if err != nil || len(two.Events) != 1 || two.HasMore || two.Cursor == one.Cursor {
		t.Fatalf("page after restart: %+v %v", two, err)
	}
	for _, event := range append(one.Events, two.Events...) {
		if event.RunID != left.ID || event.TaskID != left.TaskID || event.Kind != "run.state_changed" {
			t.Fatalf("wrong event escaped scope or type filter: %+v", event)
		}
	}
	data, _ := json.Marshal(two)
	if strings.Contains(string(data), "private") {
		t.Fatalf("raw error leaked: %s", data)
	}
	empty, err := page(actor, left.ConversationID, two.Cursor)
	if err != nil || empty.HasMore || len(empty.Events) != 0 || empty.Cursor != two.Cursor {
		t.Fatalf("empty poll lost cursor: %+v %v", empty, err)
	}
	write(left.ID, "run.state_changed", map[string]string{"status": "completed"})
	final, err := page(actor, left.ConversationID, empty.Cursor)
	if err != nil || len(final.Events) != 1 || final.Events[0].Status != "completed" {
		t.Fatalf("new terminal event lost: %+v %v", final, err)
	}
	if _, err := s.db.Exec("DELETE FROM run_events WHERE run_id=?", left.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := page(actor, left.ConversationID, final.Cursor); !errors.Is(err, ErrConversationEventCursorReset) {
		t.Fatalf("lost anchor did not request a fresh snapshot: %v", err)
	}
}
