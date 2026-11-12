package work

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestMemoryMutationRequiresCurrentRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.SaveMemory(ctx, "", Memory{Kind: "preference", Title: "周报格式", Content: "先写结论"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveMemory(ctx, "", m, 0); !errors.Is(err, ErrMemoryDuplicate) {
		t.Fatalf("duplicate memory: %v", err)
	}
	stale := m
	m.Content = "先写结论和风险"
	updated, err := s.SaveMemory(ctx, m.ID, m, 0)
	if err != nil || updated.Revision != m.Revision+1 {
		t.Fatalf("save revision: %+v, %v", updated, err)
	}
	if _, err = s.SaveMemory(ctx, stale.ID, stale, 0); !errors.Is(err, ErrMemoryRevision) {
		t.Fatalf("stale update: %v", err)
	}
	if err = s.DeleteMemory(ctx, stale.ID, stale.Revision); !errors.Is(err, ErrMemoryRevision) {
		t.Fatalf("stale delete: %v", err)
	}
	items, err := s.Memories(ctx)
	if err != nil || len(items) != 1 || items[0].Content != m.Content {
		t.Fatalf("latest memory: %+v, %v", items, err)
	}
}

func TestMemoryCommandPersistsInConversationOnce(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, text := range []string{"怎么让你记住这件事？", "不要记住我的地址", "记住了吗？", "记住了哪些内容", "记住的内容能删除吗", "「记住」是什么意思"} {
		if _, handled, err := s.SubmitMemoryCommand(ctx, "not-a-command", text, "", ""); handled || err != nil {
			t.Fatalf("ordinary chat handled as a save: %q, %v", text, err)
		}
	}
	if _, handled, err := s.SubmitMemoryCommand(ctx, "empty-command", "记住这件事", "", ""); !handled || !errors.Is(err, ErrMemoryCommandContent) {
		t.Fatalf("ambiguous save: handled=%v, %v", handled, err)
	}

	const request = "请记住：我的办公地址是上海"
	run, handled, err := s.SubmitMemoryCommand(ctx, "remember-address", request, "", "")
	if err != nil || !handled || run.Status != "completed" || run.TaskID != "" {
		t.Fatalf("local save: %+v, %v, %v", run, handled, err)
	}
	replay, _, err := s.SubmitMemoryCommand(ctx, "remember-address", request, "", "")
	if err != nil || replay.ID != run.ID {
		t.Fatalf("retry: %+v, %v", replay, err)
	}
	if _, _, err := s.SubmitMemoryCommand(ctx, "remember-address", "记住：我的办公地址是杭州", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if _, _, err := s.SubmitMemoryCommand(ctx, "conflicting-address", "记住：我的办公地址是杭州", "", run.ConversationID); !errors.Is(err, ErrMemoryDuplicate) {
		t.Fatalf("same title: %v", err)
	}

	detail, err := s.Detail(run.ConversationID, false)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[0].Text != request || detail.Messages[1].Text != "已保存到个人记忆。" {
		t.Fatalf("stored exchange: %+v, %v", detail, err)
	}
	items, err := s.Memories(ctx)
	if err != nil || len(items) != 1 || items[0].Kind != "fact" || items[0].Title != "办公地址" ||
		items[0].Content != "我的办公地址是上海" || items[0].Source == nil || items[0].Source.MessageID != detail.Messages[0].ID {
		t.Fatalf("stored memory: %+v, %v", items, err)
	}
	if len(matchingMemories(items, "办公地址在哪里？")) != 1 {
		t.Fatal("the saved fact could not be recalled by its subject")
	}
}

func TestAnswerMemoryReferencesRetainTheirOriginalVersion(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	saved, _, err := s.SubmitMemoryCommand(ctx, "remember-format", "记住，以后写周报先列风险", "", "")
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Memories(ctx)
	if err != nil || len(items) != 1 || items[0].Kind != "preference" {
		t.Fatalf("saved preference: %+v, %v", items, err)
	}
	first := items[0]
	answer := func(key string, task bool, conversation string) Run {
		t.Helper()
		run, err := s.Submit(ctx, key, "帮我写周报", "", conversation, task)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := s.claim(ctx)
		if err != nil || claimed.ID != run.ID {
			t.Fatalf("claim: %+v, %v", claimed, err)
		}
		if _, err = s.prepareModelRequest(ctx, run.ID, 0, "帮我写周报"); err != nil {
			t.Fatal(err)
		}
		if err = s.delta(ctx, run.ID, "周报内容"); err != nil {
			t.Fatal(err)
		}
		if err = s.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		return run
	}
	oldAnswer := answer("first-answer", false, saved.ConversationID)
	edited := first
	edited.Title = "周报格式"
	edited.Content = "先写进展"
	edited, err = s.SaveMemory(ctx, edited.ID, edited, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskAnswer := answer("second-answer", true, saved.ConversationID)
	local, _, err := s.SubmitMemoryCommand(ctx, "remember-in-task", "记住：周报截止日是周五", "", taskAnswer.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteMemory(ctx, edited.ID, edited.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Detail(taskAnswer.TaskID, true)
	if err != nil || detail.Run == nil || detail.Run.ID != taskAnswer.ID || detail.Task.Status != "waiting_user" {
		t.Fatalf("task execution changed after local save: %+v, %v", detail, err)
	}
	expected := map[string][]MemoryRef{
		oldAnswer.ID:  {{ID: first.ID, Revision: 1, Title: first.Title}},
		taskAnswer.ID: {{ID: first.ID, Revision: 2, Title: edited.Title}},
	}
	for _, message := range detail.Messages {
		if message.Role == "assistant" {
			if !slices.Equal(message.MemoryRefs, expected[message.RunID]) {
				t.Fatalf("wrong refs on answer %s: %+v", message.RunID, message.MemoryRefs)
			}
			delete(expected, message.RunID)
		}
	}
	if len(expected) != 0 || detail.Messages[len(detail.Messages)-1].RunID != local.ID {
		t.Fatalf("missing expected conversation answers: %+v", detail.Messages)
	}
	input, err := s.modelMessages(ctx, taskAnswer.ConversationID)
	if err != nil || len(input) != 4 || input[len(input)-1].Text != "周报内容" {
		t.Fatalf("local receipt leaked into execution history: %+v, %v", input, err)
	}
}
