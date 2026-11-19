package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestConversationSkillSelectionPinsOnlyMatchingFutureRun(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "conversation-skill.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, scope = "user:session", "reports"
	draft := func(body string, revision int) SkillVersion {
		t.Helper()
		out, err := s.DraftSkill(ctx, scope, body, revision)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	activate := func(v SkillVersion, revision int) {
		t.Helper()
		if err := s.ActivateSkill(ctx, v.SkillID, v.Version, revision, v.Digest); err != nil {
			t.Fatal(err)
		}
	}
	conversation := func(key string) string {
		t.Helper()
		// A completed no-task chat turn, with no published benchmark or provider.
		chat, handled, err := s.SubmitBenchmarkCommand(ctx, key, actor, "", "跑 POD7 全套基准", "", nil)
		if err != nil || !handled || chat.ConversationID == "" {
			t.Fatalf("unable to create chat: %+v %t %v", chat, handled, err)
		}
		return chat.ConversationID
	}
	conversationID := conversation("chosen-chat")
	first := draft("First version", 0)
	bind := ConversationSkillBindingInput{Action: "bind_skill", SkillID: first.SkillID, Version: first.Version}
	clear := ConversationSkillBindingInput{Action: "clear_skill"}
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, bind); !errors.Is(err, ErrState) {
		t.Fatalf("unapproved draft became usable: %v", err)
	}
	activate(first, 0)
	initial, err := s.SetConversationSkillSelection(ctx, actor, conversationID, bind)
	if err != nil || initial.Binding == nil || initial.Binding.Validity != "valid" || initial.Binding.Version != first.Version {
		t.Fatalf("expected explicitly selected approved version: %+v %v", initial, err)
	}
	if initial.BindingRevision != 1 {
		t.Fatalf("wrong initial binding revision: %+v", initial)
	}
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear); !errors.Is(err, ErrConversationBindingConflict) {
		t.Fatalf("legacy omission overwrote a selected binding: %v", err)
	}
	clear.ExpectedBindingRevision = &initial.BindingRevision
	gone, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear)
	if err != nil || gone.Binding != nil || gone.BindingRevision != 2 {
		t.Fatalf("lost cleared revision: %+v %v", gone, err)
	}
	bind.ExpectedBindingRevision = &gone.BindingRevision
	renewed, err := s.SetConversationSkillSelection(ctx, actor, conversationID, bind)
	if err != nil || renewed.BindingRevision != 3 {
		t.Fatalf("same target rebind reused revision: %+v %v", renewed, err)
	}
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear); !errors.Is(err, ErrConversationBindingConflict) {
		t.Fatalf("older window cleared same target after ABA: %v", err)
	}
	clear.ExpectedBindingRevision = &renewed.BindingRevision
	if _, err := s.ReadConversationSkillSelection(ctx, "other", conversationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign principal could read local selection: %v", err)
	}
	if _, err := s.SetConversationSkillSelection(ctx, "other", conversationID, clear); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign principal could clear local selection: %v", err)
	}
	second := draft("Second version", 1)
	activate(second, 1)
	bind.ExpectedBindingRevision = &renewed.BindingRevision
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, bind); !errors.Is(err, ErrState) {
		t.Fatalf("current binding revision accepted stale source version: %v", err)
	}
	claim := func(key, conv, selectedScope string) Run {
		t.Helper()
		if _, err := s.Submit(ctx, key, "Organize this report", selectedScope, conv, true); err != nil {
			t.Fatal(err)
		}
		run, err := s.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	run := claim("chosen-run", conversationID, scope)
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear); !errors.Is(err, ErrActive) {
		t.Fatalf("running conversation binding was changed: %v", err)
	}
	pinned, err := s.skillForRun(ctx, run)
	if err != nil || pinned == nil || pinned.Version != first.Version || pinned.Instructions != "First version" {
		t.Fatalf("real runtime ignored or upgraded chosen version: %+v %v", pinned, err)
	}
	projected, err := s.ReadRunSkillBinding(ctx, actor, run.ID)
	if err != nil || projected.Binding == nil || projected.Binding.SelectionSource != "conversation_selection" || projected.Binding.Version != first.Version {
		t.Fatalf("run binding missing provenance: %+v %v", projected, err)
	}

	otherConversation := conversation("unmatched-chat")
	if _, err := s.SetConversationSkillSelection(ctx, actor, otherConversation, ConversationSkillBindingInput{Action: "bind_skill", SkillID: second.SkillID, Version: second.Version}); err != nil {
		t.Fatal(err)
	}
	mismatch := claim("different-purpose", otherConversation, "unrelated")
	if v, err := s.skillForRun(ctx, mismatch); err != nil || v != nil {
		t.Fatalf("selected Skill applied outside purpose: %+v %v", v, err)
	}

	if err := s.DisableSkill(ctx, first.SkillID, 2); err != nil {
		t.Fatal(err)
	}
	selected, err := s.ReadConversationSkillSelection(ctx, actor, conversationID)
	if err != nil || selected.Binding == nil || selected.Binding.Validity != "revoked" {
		t.Fatalf("disabled skill still selected as valid: %+v %v", selected, err)
	}
	if _, err := s.skillForRun(ctx, run); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("bound run used disabled selection: %v", err)
	}
	if _, err := s.db.Exec("UPDATE runs SET status='interrupted' WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear); err != nil || got.Binding != nil {
		t.Fatalf("clear selected skill when idle: %+v %v", got, err)
	}
	if _, err := s.db.Exec("UPDATE conversations SET archived=1 WHERE id=?", conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConversationSkillSelection(ctx, actor, conversationID, clear); !errors.Is(err, ErrConversationArchived) {
		t.Fatalf("archived binding changed: %v", err)
	}
	if old, err := s.ReadRunSkillBinding(ctx, actor, run.ID); err != nil || old.Binding == nil || old.Binding.Version != first.Version {
		t.Fatalf("clearing future choice rewrote a run history: %+v %v", old, err)
	}
}
