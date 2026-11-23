package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestMessageTaskRefSurvivesRecoveryWithoutRoutingOrModelContext(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "message-task-ref.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	create := func(key string) Run {
		t.Helper()
		r, e := s.Submit(ctx, key, key, "", "", true)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.claim(ctx); e != nil {
			t.Fatal(e)
		}
		if e = s.finish(r, "completed", "", runAccounting{}); e != nil {
			t.Fatal(e)
		}
		return r
	}
	target, other := create("original-task"), create("quoted-task")
	before, err := s.Detail(target.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	ref := Delivery{TaskRef: &MessageTaskRef{TaskID: target.TaskID}}
	const actor = "user:session"
	text := "普通说明；引用其他消息「任务 " + other.TaskID + "」不切换本条任务"
	if _, err := s.AdmitChat(ctx, "wrong-conversation", actor, other.ConversationID, text, "", false, ref, nil, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-conversation ref accepted: %v", err)
	}
	if _, err := s.AdmitChat(ctx, "wrong-actor", "other", target.ConversationID, text, "", false, ref, nil, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign actor ref accepted: %v", err)
	}
	const key = "persistent-reference"
	if receipt, err := s.AdmitChat(ctx, key, actor, target.ConversationID, text, "", false, ref, nil, false); err != nil || !receipt.ModelPending {
		t.Fatalf("could not durably accept reference: %+v %v", receipt, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	router := func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		t.Fatal("task label was sent to Router")
		return DelegationResult{}, nil
	}
	if err := s.ProcessChatAdmissions(ctx, router); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.ReadAdmission(ctx, key)
	if err != nil || receipt.RunID == "" || receipt.TaskID != "" {
		t.Fatalf("did not send plain chat after restart: %+v %v", receipt, err)
	}
	replay, err := s.SubmitRoutedChat(ctx, key, text, "", target.ConversationID, ref, router)
	if err != nil || replay.ID != receipt.RunID {
		t.Fatalf("replay changed run: %+v %v", replay, err)
	}
	if _, err := s.SubmitRoutedChat(ctx, key, text, "", target.ConversationID, Delivery{TaskRef: &MessageTaskRef{TaskID: other.TaskID}}, router); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed tag reused key: %v", err)
	}
	detail, err := s.Detail(target.TaskID, true)
	if err != nil || detail.Task.Revision != before.Task.Revision || detail.Task.Status != before.Task.Status {
		t.Fatalf("reference mutated task: %+v %v", detail, err)
	}
	page, err := s.MessageHistory(ctx, target.ConversationID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range page.Items {
		if m.TaskRef != nil {
			count++
			if m.TaskRef.TaskID != target.TaskID || m.TaskRef.Title != before.Task.Title || m.Text != text {
				t.Fatalf("text retargeted explicit tag: %+v", m)
			}
		}
	}
	if count != 1 {
		t.Fatalf("lost or duplicated history tag: %+v", page)
	}
	model, err := messages(s.db, target.ConversationID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range model {
		if m.TaskRef != nil {
			t.Fatal("display metadata leaked to model history")
		}
	}
	if _, err := s.db.Exec("UPDATE conversations SET archived=1 WHERE id=?", target.ConversationID); err != nil {
		t.Fatal(err)
	}
	archived, err := s.MessageHistory(ctx, target.ConversationID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range archived.Items {
		if m.TaskRef != nil {
			t.Fatal("archived conversation retained a live task reference")
		}
	}
}
