package work

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedReplyDoesNotConsumeOtherPendingInput(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "isolated-delivery.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.SaveModelProfile(ctx, ModelConnectionProfile{Name: "explicit", Settings: ModelSettings{ModelID: "model", Protocol: "responses", BaseURL: "https://fixture.invalid", KeyRef: "env:TEST_FIXTURE"}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Submit(ctx, "isolated-old-input", "PREVIOUS-PENDING", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context, string, int) (string, error) { return "fixture", nil }
	in := IsolatedReplyInput{Mode: IsolatedReplyMode, ConfirmCurrentTextEgress: true, ProfileID: p.ID, ProfileRevision: p.Settings.Revision,
		RequestID: "isolated-new-request", Text: "SEPARATE-ONLY"}
	run, err := s.SubmitIsolatedReply(ctx, "user:session", old.ConversationID, in, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := s.claim(ctx); err != nil || claim.ID != run.ID {
		t.Fatalf("claim %+v %v", claim, err)
	}
	var deliveries int
	if err := s.db.QueryRow("SELECT count(*) FROM input_deliveries WHERE run_id=?", run.ID).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("isolated run took unselected deliveries: %d %v", deliveries, err)
	}
	if _, err := s.StopConversation(ctx, old.ConversationID, run.ID); err != nil {
		t.Fatal(err)
	}
	var oldDisposition, selectedDisposition bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM input_dispositions d JOIN messages m ON m.seq=d.message_seq WHERE m.run_id=?),
EXISTS(SELECT 1 FROM input_dispositions d JOIN messages m ON m.seq=d.message_seq WHERE m.run_id=?)`, old.ID, run.ID).
		Scan(&oldDisposition, &selectedDisposition); err != nil || oldDisposition || !selectedDisposition {
		t.Fatalf("isolated stop touched an old pending input: old=%t isolated=%t %v", oldDisposition, selectedDisposition, err)
	}
	// Finalize the stopped reply as the production worker would, then check
	// default model assembly and Router history do not silently re-export it.
	if err := s.finish(run, "cancelled", "已停止", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	_, history, err := s.chatSubmissionContext(ctx, "next", "fixture", old.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.modelMessagesForRun(ctx, old.ConversationID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range [][]Message{history, input} {
		var foundOld bool
		for _, m := range stream {
			if strings.Contains(m.Text, "SEPARATE-ONLY") {
				t.Fatal("isolated text leaked into default path")
			}
			foundOld = foundOld || strings.Contains(m.Text, "PREVIOUS-PENDING")
		}
		if !foundOld {
			t.Fatal("ordinary history was inadvertently removed")
		}
	}
}
