package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestStopConversationPreservesFragmentAndLaterRuns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	run, err := s.Submit(ctx, "stop-running", "普通问题", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.delta(ctx, run.ID, "已提交片段"); err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 2; count++ {
		stopping, err := s.StopConversation(ctx, run.ConversationID, run.ID)
		if err != nil || stopping.Status != "running" {
			t.Fatalf("premature terminal state: %+v, %v", stopping, err)
		}
	}
	if err := s.delta(ctx, run.ID, "不应追加"); err == nil {
		t.Fatal("stop intent allowed more output")
	}
	if err := s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Detail(run.ConversationID, false)
	if err != nil || detail.Run.Status != "cancelled" || len(detail.Messages) != 2 || !detail.Messages[1].Incomplete || detail.Messages[1].Text != "已提交片段" {
		t.Fatalf("stopped fragment: %+v, %v", detail, err)
	}
	next, err := s.Submit(ctx, "stop-next", "后续消息", "", run.ConversationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopConversation(ctx, run.ConversationID, run.ID); err != nil {
		t.Fatal(err)
	}
	detail, err = s.Detail(run.ConversationID, false)
	if err != nil || detail.Run.ID != next.ID || detail.Run.Status != "queued" {
		t.Fatalf("late stop affected later run: %+v, %v", detail, err)
	}
	for count := 0; count < 2; count++ {
		stopped, err := s.StopConversation(ctx, next.ConversationID, next.ID)
		if err != nil || stopped.Status != "cancelled" {
			t.Fatalf("queued stop: %+v, %v", stopped, err)
		}
	}
	if _, err := s.claim(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cancelled queue was claimed: %v", err)
	}
	completed, err := s.Submit(ctx, "stop-already-completed", "正常结束的问题", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.delta(ctx, completed.ID, "完整答复"); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(completed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	if terminal, err := s.StopConversation(ctx, completed.ConversationID, completed.ID); err != nil || terminal.Status != "completed" {
		t.Fatalf("completed outcome changed: %+v, %v", terminal, err)
	}
	detail, err = s.Detail(completed.ConversationID, false)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[1].Incomplete {
		t.Fatalf("completed reply duplicated or marked incomplete: %+v, %v", detail, err)
	}
	if _, err := s.StopConversation(ctx, run.ConversationID, completed.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stop crossed conversation scope: %v", err)
	}
	task, err := s.Submit(ctx, "stop-task-scope", "任务", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopConversation(ctx, task.ConversationID, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ordinary stop controlled a Task: %v", err)
	}
}

func TestStopConversationRacingCompletionDoesNotDuplicate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stop-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for index := 0; index < 16; index++ {
		run, err := s.Submit(ctx, fmt.Sprint("stop-race-", index), "普通问题", "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.claim(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.delta(ctx, run.ID, "实际已提交内容"); err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		errors := make(chan error, 2)
		group.Add(2)
		go func() {
			defer group.Done()
			_, err := s.StopConversation(ctx, run.ConversationID, run.ID)
			errors <- err
		}()
		go func() { defer group.Done(); errors <- s.finish(run, "completed", "", runAccounting{}) }()
		group.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		detail, err := s.Detail(run.ConversationID, false)
		if err != nil || len(detail.Messages) != 2 || (detail.Run.Status != "completed" && detail.Run.Status != "cancelled") {
			t.Fatalf("racing completion: %+v, %v", detail, err)
		}
		before := detail.Run.Status
		if _, err := s.StopConversation(ctx, run.ConversationID, run.ID); err != nil {
			t.Fatal(err)
		}
		detail, err = s.Detail(run.ConversationID, false)
		if err != nil || detail.Run.Status != before || len(detail.Messages) != 2 {
			t.Fatalf("late stop changed terminal outcome or duplicated message: %+v, %v", detail, err)
		}
	}
}
