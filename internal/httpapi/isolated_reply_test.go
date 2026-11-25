package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestIsolatedReplyPinnedAcrossRestartAndNoHistoryEgress(t *testing.T) {
	ctx := t.Context()
	const env = "CLUSBOT_TEST_ISOLATED_KEY"
	t.Setenv(env, "fixture-profile-credential")
	var isolatedCalls, oldCalls atomic.Int32
	started := make(chan struct{}, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		text := string(body)
		if r.Header.Get("Authorization") == "Bearer fixture-profile-credential" {
			isolatedCalls.Add(1)
			if strings.Contains(text, "PRIVATE-HISTORY") || strings.Contains(text, "PRIVATE-MEMORY") ||
				strings.Contains(text, `"tools"`) || !strings.Contains(text, `"model":"isolated-model"`) {
				t.Errorf("isolated input escapes approved scope: %s", text)
			}
			var payload struct {
				Input []json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || len(payload.Input) != 1 {
				t.Errorf("expected exactly one caller message, got %d (%v)", len(payload.Input), err)
			}

			if strings.Contains(text, "INTERRUPT-NEW-TEXT") {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				w.(http.Flusher).Flush()
				started <- struct{}{}
				<-r.Context().Done()
				return
			}
			if !strings.Contains(text, "APPROVED-NEW-TEXT") {
				t.Errorf("new message lost: %s", text)
			}
		} else {
			oldCalls.Add(1)
			if !strings.Contains(text, "PRIVATE-HISTORY") {
				t.Error("default model received an isolated reply")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"vendor/version\"}}\n\n")
	}))
	defer fake.Close()
	path := filepath.Join(t.TempDir(), "isolated-reply.sqlite")
	s, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	businessFixturePrincipal(t, s)
	s.SetConnectionCredentialKey("fixture-connection-key")
	settings := &model.Settings{Store: s}
	if _, err := settings.Initialize(ctx, work.ModelSettings{ModelID: "default-model", Protocol: model.ProtocolResponses, BaseURL: fake.URL, KeyRef: "env:CLUSBOT_UNUSED_DEFAULT_FIXTURE"}); err != nil {
		t.Fatal(err)
	}
	v, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: "Test isolated", Protocol: model.ProtocolResponses, BaseURL: fake.URL,
		ModelID: "isolated-model", KeyRef: "env:" + env})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Submit(ctx, "legacy-reply-key", "PRIVATE-HISTORY", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	launch := func() func() {
		t.Helper()
		runCtx, cancel := context.WithCancel(ctx)
		w := s.Start(runCtx, settings, 1, work.RunLimits{ActiveTime: 4 * time.Second, ModelTimeout: 3 * time.Second, OutputTokens: 128})
		return func() { cancel(); w.Wait() }
	}
	wait := func(id, want string) work.RunRecord {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			out, err := s.RunRecord(ctx, id, "")
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "running" && out.Status != "queued" {
				if out.Status != want {
					t.Fatalf("run %s: %s %s", id, out.Status, out.Error)
				}
				return out
			}
			select {
			case <-timer.C:
				t.Fatalf("run %s did not settle", id)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	stop := launch()
	wait(legacy.ID, "completed")
	stop()
	if _, handled, err := s.SubmitMemoryCommand(ctx, "legacy-mem-key", "记住 PRIVATE-MEMORY", "", legacy.ConversationID); !handled || err != nil {
		t.Fatalf("memory fixture %t %v", handled, err)
	}
	historical, err := s.SubmitWithDelivery(ctx, "old-task-history", "PRIVATE-HISTORY-TASK", "", legacy.ConversationID, true, work.Delivery{})
	if err != nil || historical.TaskID == "" {
		t.Fatalf("historical task setup: %+v %v", historical, err)
	}
	before, err := s.Detail(historical.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Control(ctx, historical.TaskID, "stop", before.Task.Revision); err != nil {
		t.Fatal(err)
	}
	stableTask, err := s.Detail(historical.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}

	serverFor := func() *a1Server {
		h := Server{Store: s, Settings: settings, Hosts: []string{"127.0.0.1"}}
		a := &a1Server{store: s, ts: httptest.NewServer(businessFixtureHandler(t, h))}
		a.cookie = a.login()
		t.Cleanup(a.ts.Close)
		return a
	}
	a := serverFor()
	route := "/api/v1/conversations/" + legacy.ConversationID + "/isolated-replies"
	input := work.IsolatedReplyInput{Mode: work.IsolatedReplyMode, ConfirmCurrentTextEgress: true, ProfileID: v.ID, ProfileRevision: v.Revision, RequestID: "one-shot-new", Text: "APPROVED-NEW-TEXT"}
	post := func(in work.IsolatedReplyInput) (int, map[string]any) {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return a.post(route, string(data))
	}
	bad := input
	bad.ConfirmCurrentTextEgress = false
	if code, _ := post(bad); code != 400 {
		t.Fatalf("missing consent status %d", code)
	}
	if code, _ := a.post(route, `{"attachment_ids":["bad"]}`); code != 400 {
		t.Fatalf("unsupported input status %d", code)
	}
	bad = input
	bad.Text = "/benchmark run pod"
	if code, _ := post(bad); code != 400 {
		t.Fatalf("slash status %d", code)
	}
	code, receipt := post(input)
	if code != 202 || receipt["run_id"] == "" || receipt["task_id"] != "" {
		t.Fatalf("admission %d %+v", code, receipt)
	}
	id := receipt["run_id"].(string)
	if isolatedCalls.Load() != 0 || oldCalls.Load() != 1 {
		t.Fatal("router or selected provider ran while admitting")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetConnectionCredentialKey("fixture-connection-key")
	settings = &model.Settings{Store: s}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	a = serverFor()
	stop = launch()
	defer stop()
	done := wait(id, "completed")
	stop()
	if len(done.Contexts) != 1 || done.Contexts[0].ModelReceipt == nil || done.Contexts[0].ModelReceipt.ReportedModel != "vendor/version" {
		t.Fatalf("missing normal model receipt: %+v", done.Contexts)
	}
	rotate := input
	rotate.RequestID = "rotate-before-send"
	code, out := post(rotate)
	if code != 202 {
		t.Fatalf("rotation fixture %d %+v", code, out)
	}
	t.Setenv(env, "credential-rotated")
	stop = launch()
	wait(out["run_id"].(string), "failed")
	stop()
	if code, out := post(input); code != 202 || out["run_id"] != id {
		t.Fatalf("receipt replay after credential rotation %d %+v", code, out)
	}
	if isolatedCalls.Load() != 1 || oldCalls.Load() != 1 {
		t.Fatal("credential drift retried or fell back")
	}
	t.Setenv(env, "fixture-profile-credential")
	cut := input
	cut.RequestID = "cut-after-sent"
	cut.Text = "INTERRUPT-NEW-TEXT"
	code, out = post(cut)
	if code != 202 {
		t.Fatalf("interruption fixture %d %+v", code, out)
	}
	stop = launch()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no outbound isolated request")
	}
	stop()
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	interrupted := wait(out["run_id"].(string), "interrupted")
	if len(interrupted.Contexts) != 1 || interrupted.Contexts[0].ModelReceipt != nil {
		t.Fatalf("interrupted attempt falsely reported completion: %+v", interrupted.Contexts)
	}
	if code, replay := post(cut); code != 202 || replay["run_id"] != out["run_id"] || replay["status"] != "interrupted" {
		t.Fatalf("interrupted replay was not stable: %d %+v", code, replay)
	}
	stale := input
	stale.RequestID = "profile-version-before-send"
	code, out = post(stale)
	if code != 202 {
		t.Fatalf("stale profile fixture not queued: %d %+v", code, out)
	}
	if _, err := settings.SaveProfile(ctx, v.ID, model.ProfileInput{Name: v.Name, Protocol: v.Protocol, BaseURL: v.BaseURL,
		ModelID: "replacement-model", KeyRef: "env:" + env, Revision: v.Revision}); err != nil {
		t.Fatal(err)
	}
	stop = launch()
	wait(out["run_id"].(string), "failed")
	stop()
	archived := true
	if _, err := s.ChangeConversation(ctx, legacy.ConversationID, 1, nil, &archived); err != nil {
		t.Fatal(err)
	}
	if code, replay := post(input); code != 202 || replay["run_id"] != id {
		t.Fatalf("committed replay after archive/profile change: %d %+v", code, replay)
	}
	mismatch := input
	mismatch.Text = "modified"
	if code, _ := post(mismatch); code != 409 {
		t.Fatalf("changed same key status %d", code)
	}

	after, err := s.Detail(historical.TaskID, true)
	if err != nil || after.Task.Status != stableTask.Task.Status || after.Task.Revision != stableTask.Task.Revision ||
		after.Run.ID != stableTask.Run.ID {
		t.Fatalf("isolated reply moved a historical task: %+v %v", after.Task, err)
	}

	if isolatedCalls.Load() != 2 || oldCalls.Load() != 1 {
		t.Fatal("interrupted request was resent")
	}
}
