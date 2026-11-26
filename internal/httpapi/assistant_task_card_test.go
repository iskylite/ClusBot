package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAssistantTaskCardScopeAndDurableRead(t *testing.T) {
	var calls atomic.Int32
	const privateTitle = "不能发给模型的内部任务标题"
	var outcome atomic.Value
	outcome.Store("show_progress")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(assistantTaskProgressAction)) || bytes.Contains(body, []byte(privateTitle)) ||
			bytes.Contains(body, []byte("browser-private-note")) || bytes.Contains(body, []byte("task_id")) {
			t.Error("model input was not limited to the declared read action")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", fmt.Sprintf(`{"next":%q}`, outcome.Load()))
	}))
	defer provider.Close()
	t.Setenv("CLUSBOT_TASK_CARD_FIXTURE_KEY", "test-local-model-key")
	fixture := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "fixture", KeyRef: "env:CLUSBOT_TASK_CARD_FIXTURE_KEY", Protocol: model.ProtocolResponses}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	source, err := fixture.store.Submit(t.Context(), "fixture-card-source", privateTitle, "私有范围", "", true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key string, data any, cookie *http.Cookie) *httptest.ResponseRecorder {
		body, _ := json.Marshal(data)
		r := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(body))
		if method != http.MethodGet {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "https://ops.example")
		}
		r.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if key != "" {
			r.Header.Set("X-ClusBot-Request-Key", key)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		fixture.account.handler.ServeHTTP(w, r)
		return w
	}
	var origin struct {
		Conversation string `json:"conversation_id"`
		Run          string `json:"run_id"`
	}
	start := request("POST", "/api/v1/conversations", "", map[string]string{"text": "列出我的任务？", "client_message_id": "fixture-task-card-turn"}, fixture.cookie)
	if start.Code != 202 {
		t.Fatalf("composer: %d %s", start.Code, start.Body)
	}
	accountJSON(t, start, &origin)
	base := "/api/v1/conversations/" + origin.Conversation + "/task-card/" + origin.Run
	event := func() map[string]any {
		w := request("GET", base, "", nil, fixture.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), privateTitle) {
			t.Fatalf("task view %d %s", w.Code, w.Body)
		}
		var card a2uiDiscoveryReply
		accountJSON(t, w, &card)
		return renderedA2UIEvent(t, card, assistantTaskProgressAction)
	}
	action := event()
	body := action["action"].(map[string]any)
	body["name"] = "pause_task"
	if w := request("POST", base+"/actions", "request-bad-name", action, fixture.cookie); w.Code != 400 {
		t.Fatalf("unknown action %d %s", w.Code, w.Body)
	}
	body["name"] = assistantTaskProgressAction
	body["userMessage"] = "browser-private-note"
	body["metadata"] = map[string]any{"extensions": map[string]any{"untrusted": "browser-private-note"}}
	choice := body["context"].(map[string]any)
	choice["task_id"] = strings.Repeat("1", 32)
	if w := request("POST", base+"/actions", "request-bad-task", action, fixture.cookie); w.Code != 409 {
		t.Fatalf("unknown target %d %s", w.Code, w.Body)
	}
	choice["task_id"] = source.TaskID
	freshSession := fixture.account.login("admin", "correct horse battery staple")
	if w := request("POST", base+"/actions", "request-new-session", action, freshSession); w.Code != 409 {
		t.Fatalf("stale session %d %s", w.Code, w.Body)
	}
	if w := request("POST", base+"/actions", "request-anonymous", action, nil); w.Code != 401 {
		t.Fatalf("anonymous %d %s", w.Code, w.Body)
	}
	created := fixture.account.request("POST", "/api/v1/admin/users", map[string]any{
		"username": "member", "password": "separate fixture password",
	}, fixture.cookie)
	if created.Code != 201 {
		t.Fatalf("member setup %d %s", created.Code, created.Body)
	}
	var member struct{ User work.Account }
	accountJSON(t, created, &member)
	memberCookie := fixture.account.login("member", "separate fixture password")
	other, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "other.sqlite"), fixture.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.BindAccountBusinessStore(t.Context(), member.User.PrincipalID); err != nil {
		t.Fatal(err)
	}
	s := fixture.server
	previous := s.PersonalAPI
	s.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor == member.User.PrincipalID {
			return s.PersonalHandler(other), nil
		}
		return previous(actor)
	}
	// Reissue the card with the new handler before testing another account.
	fixture.account.handler = s.Handler(nil)
	action = event()
	if w := request("POST", base+"/actions", "request-other-account", action, memberCookie); w.Code != 404 || strings.Contains(w.Body.String(), privateTitle) {
		t.Fatalf("foreign account got receipt: %d %s", w.Code, w.Body)
	}
	detail, err := fixture.store.Detail(origin.Conversation, false)
	if err != nil || len(detail.Messages) < 2 || detail.Messages[0].Text != "列出我的任务？" || !detail.Messages[len(detail.Messages)-1].AssistantTaskList {
		t.Fatalf("historical card marker missing: %+v %v", detail, err)
	}
	name := "fixture renamed"
	if _, err := fixture.store.ChangeConversation(t.Context(), origin.Conversation, detail.Conversation.Revision, &name, nil); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", base+"/actions", "request-old-revision", action, fixture.cookie); w.Code != 409 || calls.Load() != 0 {
		t.Fatalf("stale action reached the model: %d, calls=%d, %s", w.Code, calls.Load(), w.Body)
	}
	fresh := event()
	freshBody := fresh["action"].(map[string]any)
	freshBody["userMessage"] = "browser-private-note"
	freshBody["metadata"] = map[string]any{"extensions": map[string]any{"untrusted": "browser-private-note"}}
	outcome.Store("pause_task")
	if w := request("POST", base+"/actions", "request-invalid-model", fresh, fixture.cookie); w.Code != 502 {
		t.Fatalf("accepted arbitrary model action: %d %s", w.Code, w.Body)
	}
	outcome.Store("show_progress")
	before, err := fixture.store.Detail(source.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"request-valid-once", "request-replayed"} {
		w := request("POST", base+"/actions", key, fresh, fixture.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "核对时：任务处理中") || !strings.Contains(w.Body.String(), "receipt_"+source.TaskID) {
			t.Fatalf("query receipt %d %s", w.Code, w.Body)
		}
	}
	var updatedCard a2uiDiscoveryReply
	accountJSON(t, request("GET", base, "", nil, fixture.cookie), &updatedCard)
	unselected := renderedA2UIEvent(t, updatedCard, assistantTaskProgressAction)
	conflict := request("POST", base+"/actions", "request-replayed", unselected, fixture.cookie)
	if conflict.Code != 409 || calls.Load() != 2 {
		t.Fatalf("a cached-result key escaped its original target: status=%d model=%d %s", conflict.Code, calls.Load(), conflict.Body)
	}
	fixture.account.handler = s.Handler(nil) // new signer; no in-memory receipt survives
	for _, tc := range []struct {
		cookie *http.Cookie
		status int
	}{{fixture.cookie, http.StatusOK}, {memberCookie, http.StatusNotFound}, {nil, http.StatusUnauthorized}} {
		if w := request("GET", base+"/records/"+source.TaskID, "", nil, tc.cookie); w.Code != tc.status {
			t.Fatalf("steps and evidence were not account/focus scoped: %d %s", w.Code, w.Body)
		}
	}
	for range 2 {
		w := request("GET", base, "", nil, fixture.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "核对时：任务处理中") {
			t.Fatalf("restored receipt %d %s", w.Code, w.Body)
		}
	}
	after, err := fixture.store.Detail(source.TaskID, true)
	if err != nil || before.Task.Revision != after.Task.Revision || before.Run.ID != after.Run.ID || before.Run.Status != after.Run.Status ||
		calls.Load() != 2 {
		t.Fatalf("unexpected effects or repeated model calls: %v; model=%d", err, calls.Load())
	}
	open := func(cookie *http.Cookie, key string) (string, string, string) {
		t.Helper()
		w := request("POST", "/api/v1/conversations", "", map[string]string{"text": "查看我的任务", "client_message_id": key}, cookie)
		if w.Code != 202 {
			t.Fatalf("new card %d %s", w.Code, w.Body)
		}
		var opened struct {
			Conversation string `json:"conversation_id"`
			Run          string `json:"run_id"`
		}
		accountJSON(t, w, &opened)
		return "/api/v1/conversations/" + opened.Conversation + "/task-card/" + opened.Run, opened.Conversation, opened.Run
	}
	pendingBase, conversation, pendingRun := open(fixture.cookie, "ambiguous-read-turn")
	view := request("GET", pendingBase, "", nil, fixture.cookie)
	var pendingCard a2uiDiscoveryReply
	accountJSON(t, view, &pendingCard)
	started := renderedA2UIEvent(t, pendingCard, assistantTaskProgressAction)
	selected := started["action"].(map[string]any)["context"].(map[string]any)
	const oldKey = "pending-model-dispatch"
	completed, err := fixture.store.BeginAssistantTaskProgress(t.Context(), work.AssistantTaskProgressAction{
		Actor: fixture.actor, Conversation: conversation, Run: pendingRun, Target: selected["task_id"].(string), Request: oldKey, Revision: int64(selected["revision"].(float64)),
	})
	if err != nil || completed {
		t.Fatalf("could not reserve ambiguous request: %v", err)
	}
	fixture.account.handler = s.Handler(nil)
	view = request("GET", pendingBase, "", nil, fixture.cookie)
	accountJSON(t, view, &pendingCard)
	signedAgain := renderedA2UIEvent(t, pendingCard, assistantTaskProgressAction)
	for _, key := range []string{oldKey, "fresh-duplicate-pending"} {
		w := request("POST", pendingBase+"/actions", key, signedAgain, fixture.cookie)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "a2ui_task_progress_pending") || calls.Load() != 2 {
			t.Fatalf("ambiguous request re-dispatched: %d model=%d %s", w.Code, calls.Load(), w.Body)
		}
	}

	const memberTitle = "普通成员的本地任务"
	memberTask, err := other.Submit(t.Context(), "member-private-source", memberTitle, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	memberBase, _, _ := open(memberCookie, "member-personal-card-turn")
	view = request("GET", memberBase, "", nil, memberCookie)
	if view.Code != 200 || !strings.Contains(view.Body.String(), memberTitle) || strings.Contains(view.Body.String(), privateTitle) {
		t.Fatalf("member list not isolated: %d %s", view.Code, view.Body)
	}
	var memberCard a2uiDiscoveryReply
	accountJSON(t, view, &memberCard)
	chosen := renderedA2UIEvent(t, memberCard, assistantTaskProgressAction)
	if w := request("POST", memberBase+"/actions", "member-progress-request", chosen, memberCookie); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "receipt_"+memberTask.TaskID) || calls.Load() != 3 {
		t.Fatalf("ordinary member could not read own task or extra model calls occurred: %d model=%d %s", w.Code, calls.Load(), w.Body)
	}
}
