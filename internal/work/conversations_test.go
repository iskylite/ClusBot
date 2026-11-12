package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestOrdinaryChatDeletionPreservesMemoryAndBlocksReplay(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chat, err := s.Submit(ctx, "delete-chat-key", "保留偏好", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteConversation(ctx, chat.ConversationID, 1); !errors.Is(err, ErrActive) {
		t.Fatalf("active chat deleted: %v", err)
	}
	if _, err = s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(chat, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(chat.ConversationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveMemory(ctx, "", Memory{Kind: "preference", Title: "文字风格", Content: "先说结论"}, d.Messages[0].ID); err != nil {
		t.Fatal(err)
	}
	archived := true
	title := "整理偏好"
	c, err := s.ChangeConversation(ctx, chat.ConversationID, d.Conversation.Revision, &title, &archived)
	if err != nil || !c.Archived || c.Title != title {
		t.Fatalf("archive: %+v, %v", c, err)
	}
	if _, err = s.Submit(ctx, "archive-followup", "继续", "", c.ID, false); !errors.Is(err, ErrConversationArchived) {
		t.Fatalf("archived chat accepted input: %v", err)
	}
	if err = s.DeleteConversation(ctx, c.ID, c.Revision-1); !errors.Is(err, ErrConversationRevision) {
		t.Fatalf("stale delete: %v", err)
	}
	if err = s.DeleteConversation(ctx, c.ID, c.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Detail(c.ID, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted chat still exists: %v", err)
	}
	if _, err = s.Submit(ctx, "delete-chat-key", "保留偏好", "", "", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted request recreated a chat: %v", err)
	}
	memories, err := s.Memories(ctx)
	if err != nil || len(memories) != 1 || memories[0].Source != nil || !memories[0].SourceDeleted || memories[0].Content != "先说结论" {
		t.Fatalf("memory lost after chat deletion: %+v, %v", memories, err)
	}
}

func TestDeletingTaskConversationOnlyArchivesEvidence(t *testing.T) {
	s, _, _, before := publicationFixture(t)
	ctx := context.Background()
	p := requestWrite(t, s, before, "纪要.md", "conversation-approval")
	if err := s.DeleteConversation(ctx, before.Conversation.ID, before.Conversation.Revision); !errors.Is(err, ErrPublicationOpen) {
		t.Fatalf("deletion bypassed pending approval: %v", err)
	}
	if _, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); err != nil {
		t.Fatal(err)
	}
	before, err := s.Detail(before.Task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteConversation(ctx, before.Conversation.ID, before.Conversation.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := s.Detail(before.Task.ID, true)
	if err != nil || !after.Conversation.Archived || after.Conversation.ID != before.Conversation.ID ||
		after.Task.Revision != before.Task.Revision || after.Run.ID != before.Run.ID || after.Artifact.ID != before.Artifact.ID ||
		after.Publication.ID != p.ID || len(after.Messages) != len(before.Messages) {
		t.Fatalf("archiving altered task evidence: %+v, %v", after, err)
	}
}
