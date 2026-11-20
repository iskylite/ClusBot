package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestConversationReplyRoleNoWiderCapabilityOrData(t *testing.T) {
	const ref = "env:CLUSBOT_INTENT_TEST_KEY"
	t.Setenv("CLUSBOT_INTENT_TEST_KEY", "reply-fixture")
	traffic := make(chan responseRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		traffic <- req
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", "fixture")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer srv.Close()
	store, settings := classificationSettings(t, srv.URL)
	ctx := t.Context()
	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, output := 4096, 160
	base.Attributes = &work.ModelAttributes{ContextTokens: &window, MaxOutputTokens: &output}
	if base, err = store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}

	legacy, legacyPin, err := settings.TextRunProvider(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	existingPin, _ := legacy.(*Provider).ReadCheckpointIdentity()
	if legacyPin != existingPin+"\nconversation/v1" {
		t.Fatal("legacy checkpoint changed")
	}
	routerBefore, _, err := settings.inheritedModelRoleProvider(ctx, work.GeneralRouterModelRole, errGeneralRouterBinding)
	if err != nil {
		t.Fatal(err)
	}
	in := []work.Message{
		{Role: "user", Text: "Earlier message"},
		{Role: "assistant", Text: "Earlier answer"},
		{Role: "user", Text: "Current question", RequiredInput: true,
			Materials: []work.Material{{Path: "notes.txt", Content: "permitted-material"}},
			Memories:  []work.Memory{{Kind: "preference", Title: "format", Content: "matching-memory"}}},
	}
	emit := func(string) error { return nil }
	if _, err = legacy.Stream(ctx, in, false, output, emit); err != nil {
		t.Fatal(err)
	}
	previous := <-traffic

	yes := true
	big := 99000
	p, err := settings.SaveProfile(ctx, "", ProfileInput{Name: "Reply", Protocol: ProtocolResponses, BaseURL: srv.URL, ModelID: "reply", KeyRef: ref,
		Attributes: &work.ModelAttributes{Vision: &yes, ContextTokens: &big, MaxOutputTokens: &big}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := settings.ReadConversationReplyRole(ctx)
	if err != nil || v.Source != "legacy_default" || len(v.EgressFields) != 5 {
		t.Fatalf("missing upfront disclosure: %+v %v", v, err)
	}
	bind := ConversationReplyRoleInput{Action: "bind_profile", ProfileID: p.ID, ProfileRevision: p.Revision}
	if _, err := settings.SetConversationReplyRole(ctx, bind); err == nil {
		t.Fatal("bound without conversation-context consent")
	}
	bind.ConfirmConversationContextEgress = true
	if v, err = settings.SetConversationReplyRole(ctx, bind); err != nil || v.Status != "locally_configured" {
		t.Fatalf("bind: %+v %v", v, err)
	}
	if _, err = settings.SetConversationReplyRole(ctx, bind); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("accepted stale role: %v", err)
	}
	reply, changedPin, err := settings.TextRunProvider(ctx, false)
	if err != nil || changedPin == legacyPin {
		t.Fatalf("missing reply generation: %s %v", changedPin, err)
	}
	if _, isToolProvider := reply.(*Provider); isToolProvider {
		t.Fatal("ordinary reply exposed native task provider")
	}
	if _, err := reply.Stream(ctx, in, true, output, emit); !errors.Is(err, work.ErrConversationReplyRoleUnavailable) {
		t.Fatal("reply profile could run a task")
	}
	withImage := append([]work.Message(nil), in...)
	withImage[len(withImage)-1].Materials = []work.Material{{Path: "images/local.png", Content: "data:image/png;base64,fixture"}}
	if _, err := reply.Stream(ctx, withImage, false, output, emit); !errors.Is(err, work.ErrImageUnsupported) {
		t.Fatalf("new role widened image egress: %v", err)
	}
	if len(traffic) != 0 {
		t.Fatal("rejected call reached provider")
	}
	if _, err = reply.Stream(ctx, in, false, output, emit); err != nil {
		t.Fatal(err)
	}
	req := <-traffic
	if req.Model != "reply" || len(req.Tools) != 0 || req.MaxOutputTokens != previous.MaxOutputTokens ||
		req.Instructions != previous.Instructions || !reflect.DeepEqual(req.Input, previous.Input) {
		t.Fatalf("reply changed data or privilege: %+v vs %+v", req, previous)
	}
	task, taskPin, err := settings.TextRunProvider(ctx, true)
	if err != nil || task.(*Provider).model != "fixture" || !strings.HasSuffix(taskPin, "\ntask/v1") {
		t.Fatalf("reply role hijacked Worker: %+v %s %v", task, taskPin, err)
	}
	routerAfter, _, err := settings.inheritedModelRoleProvider(ctx, work.GeneralRouterModelRole, errGeneralRouterBinding)
	if err != nil || routerAfter.model != routerBefore.model {
		t.Fatalf("reply role hijacked Router: %+v %v", routerAfter, err)
	}
	cred, err := settings.IsolatedReplyCredential(ctx, p.ID, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := settings.IsolatedReplyProvider(ctx, work.IsolatedReplyPin{ProfileID: p.ID, ProfileRevision: p.Revision, CredentialBinding: cred})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = isolated.Stream(ctx, []work.Message{{Role: "user", ID: 1, Text: "isolated-only"}}, false, output, emit); err != nil {
		t.Fatal(err)
	}
	isolatedReq := <-traffic
	if compact, _ := json.Marshal(isolatedReq.Input); strings.Contains(string(compact), "matching-memory") || !strings.Contains(string(compact), "isolated-only") {
		t.Fatalf("isolated reply mixed context: %+v", isolatedReq.Input)
	}

	window -= 512
	if _, err = store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, newPin, err := settings.TextRunProvider(ctx, false); err != nil || newPin == changedPin {
		t.Fatalf("configuration caps changed without pin difference: %v", err)
	}
	base, err = store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.BaseURL = srv.URL + "/changed"
	if _, err = store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := settings.TextRunProvider(ctx, false); !errors.Is(err, work.ErrConversationReplyRoleUnavailable) {
		t.Fatalf("changed endpoint fell through: %v", err)
	}
}

func TestConversationReplyRoleKeepsQueuedSelectionAndRejectsCheckpointRebind(t *testing.T) {
	t.Setenv("CLUSBOT_INTENT_TEST_KEY", "reply-fixture")
	started := make(chan responseRequest, 3)
	unblock := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input responseRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		started <- input
		select {
		case <-unblock:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		// Deliberately no terminal event. This creates a genuine recoverable run.
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "reply-restart.db")
	open := func() (*work.Store, *Settings) {
		t.Helper()
		st, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.SetConnectionCredentialKey("stable-fixture-key-across-restart")
		return st, &Settings{Store: st}
	}
	store, s := open()
	defer func() { _ = store.Close() }()
	if _, err := s.Initialize(t.Context(), work.ModelSettings{BaseURL: srv.URL, ModelID: "default", Protocol: ProtocolResponses, KeyRef: "env:CLUSBOT_INTENT_TEST_KEY"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.Submit(t.Context(), "queued-pre-bind", "普通问题", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SaveProfile(t.Context(), "", ProfileInput{Name: "Reply", Protocol: ProtocolResponses, BaseURL: srv.URL, ModelID: "reply", KeyRef: "env:CLUSBOT_INTENT_TEST_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	bind := ConversationReplyRoleInput{Action: "bind_profile", ProfileID: p.ID, ProfileRevision: p.Revision, ConfirmConversationContextEgress: true}
	if _, err := s.SetConversationReplyRole(t.Context(), bind); err != nil {
		t.Fatal(err)
	}
	start := func(st *work.Store, settings *Settings) func() {
		ctx, cancel := context.WithCancel(t.Context())
		workers := st.Start(ctx, settings, 1, work.RunLimits{ActiveTime: 30 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 128})
		return func() { cancel(); workers.Wait() }
	}
	stop := start(store, s)
	defer stop()
	select {
	case request := <-started:
		if request.Model != "reply" || len(request.Tools) != 0 {
			t.Fatalf("wrong first execution: %+v", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending run did not execute")
	}
	if _, err := s.SetConversationReplyRole(t.Context(), ConversationReplyRoleInput{Action: "use_existing_default", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	bind.Revision = 2
	if _, err := s.SetConversationReplyRole(t.Context(), bind); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	first := waitConversationReplyRun(t, store, run.ID)
	if first.Status != "interrupted" || first.ErrorCode != "model_error" {
		t.Fatalf("old in-flight stream was not finalized against its original selection: %+v", first)
	}
	stop()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, s = open()
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	defer start(store, s)()
	conversations, err := store.Conversations(false)
	if err != nil || len(conversations) != 1 {
		t.Fatalf("get resume revision: %+v %v", conversations, err)
	}
	resumed, err := store.ResumeConversation(t.Context(), run.ConversationID, run.ID, "resume-old-profile", conversations[0].Revision)
	if err != nil || resumed.Generation != 1 {
		t.Fatalf("resume fixture: %+v %v", resumed, err)
	}
	final := waitConversationReplyRun(t, store, run.ID)
	if final.Generation != 1 || final.ErrorCode != "read_checkpoint_unavailable" || calls.Load() != 1 {
		t.Fatalf("new role generation replayed old checkpoint: %+v calls=%d", final, calls.Load())
	}
	if err := s.DeleteProfile(t.Context(), p.ID, p.Revision); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Submit(t.Context(), "stale-profile", "新问题", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := waitConversationReplyRun(t, store, fresh.ID)
	if unavailable.ErrorCode != "conversation_reply_role_unavailable" || !strings.Contains(unavailable.Error, "角色设置") ||
		strings.Contains(unavailable.Error, srv.URL) || calls.Load() != 1 {
		t.Fatalf("unavailable role made a request or leaked config: %+v calls=%d", unavailable, calls.Load())
	}
}

func waitConversationReplyRun(t *testing.T, s *work.Store, id string) work.Run {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		_, run, err := s.Events(id, 0)
		if err != nil {
			t.Fatal(err)
		}
		switch run.Status {
		case "completed", "failed", "interrupted":
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for run", id)
	return work.Run{}
}
