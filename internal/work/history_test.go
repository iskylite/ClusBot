package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
)

func TestConversationHistoryStableBoundaries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var expected []string
	for index := 0; index < 105; index++ {
		key := id()
		expected = append(expected, key)
		if _, err := s.db.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", key, fmt.Sprint(index), 1000); err != nil {
			t.Fatal(err)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(expected)))
	first, err := s.ConversationsPage(ctx, false, "")
	if err != nil || len(first.Items) != 100 || first.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	for index, item := range first.Items {
		if item.ID != expected[index] {
			t.Fatal("equal-timestamp conversations did not have stable order")
		}
	}
	// Deleting the exact cursor object does not invalidate its immutable boundary.
	if _, err := s.db.Exec("DELETE FROM conversations WHERE id=?", first.Items[99].ID); err != nil {
		t.Fatal(err)
	}
	last, err := s.ConversationsPage(ctx, false, first.NextCursor)
	if err != nil || len(last.Items) != 5 || last.NextCursor != "" {
		t.Fatalf("last page after boundary deletion: %+v, %v", last, err)
	}
	for index, item := range last.Items {
		if item.ID != expected[index+100] {
			t.Fatal("history has duplicate or missing conversations")
		}
	}
	if _, err := s.ConversationsPage(ctx, true, first.NextCursor); !errors.Is(err, ErrHistoryCursor) {
		t.Fatalf("cursor crossed archived scope: %v", err)
	}
	if _, err := s.ConversationsPage(ctx, false, "invalid"); !errors.Is(err, ErrHistoryCursor) {
		t.Fatalf("invalid cursor accepted: %v", err)
	}
}

func TestMessageHistoryEdgesAndTaskSources(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	run, err := s.Submit(ctx, "history-task", "原始事项消息", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE conversations SET archived=1 WHERE id=?", run.ConversationID); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < 80; index++ {
		if _, err := s.db.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)", run.ConversationID, run.ID, fmt.Sprint(index)); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := s.MessageHistory(ctx, run.ConversationID, 0, 0, 0)
	if err != nil || len(latest.Items) != 50 || latest.BeforeCursor == 0 || latest.AfterCursor != 0 {
		t.Fatalf("latest: %+v, %v", latest, err)
	}
	older, err := s.MessageHistory(ctx, run.ConversationID, latest.BeforeCursor, 0, 0)
	if err != nil || len(older.Items) != 30 || older.BeforeCursor != 0 || older.AfterCursor == 0 {
		t.Fatalf("older: %+v, %v", older, err)
	}
	around, err := s.MessageHistory(ctx, run.ConversationID, 0, 0, older.Items[0].ID)
	if err != nil || len(around.Items) != 26 || around.Items[0].Text != "原始事项消息" || around.AfterCursor == 0 {
		t.Fatalf("archived Task source: %+v, %v", around, err)
	}
	seen := map[int64]bool{}
	for {
		for _, message := range around.Items {
			if seen[message.ID] {
				t.Fatal("message duplicated between pages")
			}
			seen[message.ID] = true
		}
		if around.AfterCursor == 0 {
			break
		}
		around, err = s.MessageHistory(ctx, run.ConversationID, 0, around.AfterCursor, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 80 {
		t.Fatalf("missing messages: %d", len(seen))
	}
	other, err := s.Submit(ctx, "history-other", "其他会话", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []int64{older.Items[0].ID, 999999} {
		if _, err := s.MessageHistory(ctx, other.ConversationID, 0, 0, anchor); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("foreign or missing source accepted: %v", err)
		}
	}
	if _, err := s.MessageHistory(ctx, run.ConversationID, 1, 2, 0); !errors.Is(err, ErrHistoryCursor) {
		t.Fatalf("ambiguous cursor accepted: %v", err)
	}
}
