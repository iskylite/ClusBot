package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestMCPOnboardingChatToTrustedForm(t *testing.T) {
	const secret = "private-startup-arg"
	var calls atomic.Int32
	var outcome atomic.Value
	outcome.Store("open_trusted_form")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		for _, s := range []string{secret, "/opt/private", "/private-mcp-endpoint", "sensitive-note", "FAKE_MCP_ENV", "fixture-real-user-message"} {
			if bytes.Contains(raw, []byte(s)) {
				t.Errorf("model saw private field %q", s)
			}
		}
		if !bytes.Contains(raw, []byte(mcpChooseTransport)) || !bytes.Contains(raw, []byte("selection")) {
			t.Error("missing model event")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", fmt.Sprintf(`{"next":%q}`, outcome.Load().(string)))
	}))
	defer provider.Close()
	t.Setenv("MCP_SETUP_FIXTURE_KEY", "fixture/model-key")
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		s := &model.Settings{Store: store}
		_, err := s.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "fixture", KeyRef: "env:MCP_SETUP_FIXTURE_KEY", Protocol: model.ProtocolResponses})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
	post := func(path, req string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
		return f.request("POST", path, req, body, cookie)
	}
	open := func(path, key string) (string, string) {
		w := post(path, "", map[string]string{"text": "我要接入一个 MCP，stdio /opt/private --arg=" + secret, "client_request_id": key}, f.cookie)
		if w.Code != http.StatusAccepted {
			t.Fatalf("chat submit: %d %s", w.Code, w.Body)
		}
		var out map[string]string
		accountJSON(t, w, &out)
		return out["conversation_id"], out["run_id"]
	}
	conv, run := open("/api/v1/conversations", "chat-mcp-new-request")
	if other, otherRun := open("/api/v1/conversations", "chat-mcp-new-request"); other != conv || otherRun != run {
		t.Fatal("replayed origin created a different turn")
	}
	detail, err := f.store.Detail(conv, false)
	if err != nil || len(detail.Messages) != 2 || !detail.Messages[1].MCPOnboarding {
		t.Fatalf("history lacks typed card: %+v %v", detail.Messages, err)
	}
	for _, m := range detail.Messages {
		if strings.Contains(m.Text, secret) || strings.Contains(m.Text, "/opt/private") {
			t.Fatal("stored raw instructions")
		}
	}
	view := func(c, r string) (string, map[string]any) {
		base := "/api/v1/conversations/" + c + "/mcp-setup/" + r
		w := f.request("GET", base, "", nil, f.cookie)
		var reply a2uiDiscoveryReply
		accountJSON(t, w, &reply)
		if w.Code != 200 || reply.Conversation != c || reply.Run != r || reply.State != "choose_transport" {
			t.Fatalf("no scoped surface %d %s", w.Code, w.Body)
		}
		msg := renderedA2UIEvent(t, reply, mcpChooseTransport)
		return base, msg
	}
	base, event := view(conv, run)
	a := event["action"].(map[string]any)
	a["userMessage"] = "fixture-real-user-message"
	a["metadata"] = map[string]any{"extensions": map[string]any{"x": "sensitive-note"}}
	if calls.Load() != 0 {
		t.Fatal("opening surface called model")
	}
	connCount := func(n int) {
		t.Helper()
		conns, e := f.instance.Connections(t.Context())
		if e != nil || len(conns) != n {
			t.Fatalf("connections=%d want %d (%v)", len(conns), n, e)
		}
	}
	connCount(0)
	admin2 := f.account.login("admin", "correct horse battery staple")
	if w := post(base+"/actions", "model-choice-1", event, admin2); w.Code != 409 {
		t.Fatalf("cross-session action %d %s", w.Code, w.Body)
	}
	_, otherRun := open("/api/v1/conversations/"+conv+"/messages", "chat-mcp-next-request")
	otherBase, _ := view(conv, otherRun)
	if w := post(otherBase+"/actions", "model-choice-1", event, f.cookie); w.Code != 409 {
		t.Fatalf("cross-run action: %d %s", w.Code, w.Body)
	}
	ctx := a["context"].(map[string]any)
	ctx["command"] = secret
	if w := post(base+"/actions", "model-choice-1", event, f.cookie); w.Code != 400 {
		t.Fatalf("unknown field %d", w.Code)
	}
	delete(ctx, "command")
	key := "model-choice-1"
	var proposal map[string]any
	for i := 0; i < 2; i++ {
		w := post(base+"/actions", key, event, f.cookie)
		accountJSON(t, w, &proposal)
		if w.Code != 200 || proposal["state"] != "open_trusted_form" || proposal["transport"] != "stdio" {
			t.Fatalf("model decision %d %s", w.Code, w.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	connCount(0)
	config := map[string]any{"transport": "stdio", "command": "/opt/private", "args": []string{secret}, "env": []string{"FAKE_MCP_ENV"}}
	args := map[string]any{"request_key": key, "surface_id": a["surfaceId"], "form_proof": proposal["form_proof"], "revision": proposal["revision"], "name": "fixture stdio", "config": config}
	if w := post(otherBase+"/connection", "", args, f.cookie); w.Code != 409 {
		t.Fatalf("cross-run form %d %s", w.Code, w.Body)
	}
	if w := post(base+"/connection", "", args, admin2); w.Code != 409 {
		t.Fatalf("cross-session form %d", w.Code)
	}
	config["transport"] = "http"
	if w := post(base+"/connection", "", args, f.cookie); w.Code != 409 {
		t.Fatalf("protocol confusion %d", w.Code)
	}
	config["transport"] = "stdio"
	connCount(0)
	if w := post(base+"/connection", "", args, f.cookie); w.Code != 409 || !strings.Contains(w.Body.String(), "mcp_preview_required") {
		t.Fatalf("connection bypassed local preflight: %d %s", w.Code, w.Body)
	}
	if w := post(otherBase+"/preview", "", args, f.cookie); w.Code != 409 {
		t.Fatalf("cross-run preflight: %d %s", w.Code, w.Body)
	}
	if w := post(base+"/preview", "", args, admin2); w.Code != 409 {
		t.Fatalf("cross-session preflight: %d %s", w.Code, w.Body)
	}
	config["command"] = "relative-or-unspecified"
	if w := post(base+"/preview", "", args, f.cookie); w.Code != 400 {
		t.Fatalf("bad local program path passed structural validation: %d %s", w.Code, w.Body)
	}
	config["command"] = "/opt/private"
	first := post(base+"/preview", "", args, f.cookie)
	var review map[string]any
	accountJSON(t, first, &review)
	if first.Code != 200 || first.Header().Get("Cache-Control") != "no-store" ||
		review["state"] != "configuration_reviewed" || review["target"] != "/opt/private" || review["argument_count"] != float64(1) ||
		review["credential_reference_count"] != float64(1) || strings.Contains(first.Body.String(), secret) || strings.Contains(first.Body.String(), "FAKE_MCP_ENV") {
		t.Fatalf("wrong or overbroad preflight: %d %s", first.Code, first.Body)
	}
	firstProof := review["preview_id"]
	if w := f.request("GET", base, "", nil, f.cookie); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "choose_transport") || strings.Contains(w.Body.String(), "preview_id") ||
		strings.Contains(w.Body.String(), "/opt/private") || calls.Load() != 1 {
		t.Fatalf("history restored a private preview or re-invoked the model: %d %s", w.Code, w.Body)
	}
	args["preview_id"] = firstProof
	config["args"] = []string{"revision-argument"}
	if w := post(base+"/connection", "", args, f.cookie); w.Code != 409 {
		t.Fatalf("modified argument retained old approval: %d %s", w.Code, w.Body)
	}
	delete(args, "preview_id")
	w := post(base+"/preview", "", args, f.cookie)
	accountJSON(t, w, &review)
	if w.Code != 200 || review["preview_id"] == firstProof {
		t.Fatalf("edited draft did not invalidate prior approval: %d %s", w.Code, w.Body)
	}
	args["preview_id"] = firstProof
	config["args"] = []string{secret}
	if w := post(base+"/connection", "", args, f.cookie); w.Code != 409 {
		t.Fatalf("superseded preview was reusable: %d %s", w.Code, w.Body)
	}
	config["args"] = []string{"revision-argument"}
	args["preview_id"] = review["preview_id"]
	connCount(0)
	if calls.Load() != 1 {
		t.Fatalf("local preflight unexpectedly called model: %d", calls.Load())
	}
	for i := 0; i < 2; i++ {
		w := post(base+"/connection", "", args, f.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"untested"`) || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("save %d %s", w.Code, w.Body)
		}
	}
	connCount(1)
	if w := f.request("GET", base, "", nil, f.cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"saved"`) {
		t.Fatalf("readback %d %s", w.Code, w.Body)
	}
	args["name"] = "attempt-change"
	if w := post(base+"/connection", "", args, f.cookie); w.Code != 409 {
		t.Errorf("edited replay %d", w.Code)
	}
	connCount(1)
	if w := post(base+"/actions", "reopen-obsolete-choice", event, f.cookie); w.Code != 409 {
		t.Fatalf("obsolete choose surface: %d", w.Code)
	}
	_, next := view(conv, otherRun)
	const duplicateKey = "duplicate-name-preflight"
	w = post(otherBase+"/actions", duplicateKey, next, f.cookie)
	if w.Code != 200 {
		t.Fatalf("could not open the duplicate-name fixture: %d %s", w.Code, w.Body)
	}
	var dupForm map[string]any
	accountJSON(t, w, &dupForm)
	duplicateDraft := map[string]any{
		"request_key": duplicateKey, "surface_id": next["action"].(map[string]any)["surfaceId"],
		"form_proof": dupForm["form_proof"], "revision": dupForm["revision"], "name": "fixture stdio", "config": config,
	}
	if w := post(otherBase+"/preview", "", duplicateDraft, f.cookie); w.Code != 409 || strings.Contains(w.Body.String(), "preview_id") {
		t.Fatalf("preflight allowed a registered name: %d %s", w.Code, w.Body)
	}
	connCount(1)
	outcome.Store("ask_for_details")
	w = post(otherBase+"/actions", "non-mutating-response", next, f.cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ask_for_details"`) || strings.Contains(w.Body.String(), "form_proof") {
		t.Errorf("ask decision %d %s", w.Code, w.Body)
	}
	outcome.Store("execute")
	w = post(otherBase+"/actions", "bad-model-enum", next, f.cookie)
	if w.Code != 502 {
		t.Errorf("forbidden model decision %d %s", w.Code, w.Body)
	}
	archived := true
	d, e := f.store.Detail(conv, false)
	if e != nil {
		t.Fatal(e)
	}
	title := "renamed conversation"
	updated, e := f.store.ChangeConversation(t.Context(), conv, d.Conversation.Revision, &title, nil)
	if e != nil {
		t.Fatal(e)
	}
	if w := post(otherBase+"/actions", "stale-after-rename", next, f.cookie); w.Code != 409 {
		t.Fatalf("stale conversation action: %d", w.Code)
	}
	if w := post(otherBase+"/preview", "", duplicateDraft, f.cookie); w.Code != 409 || !strings.Contains(w.Body.String(), "mcp_form_stale") {
		t.Fatalf("stale form after conversation change: %d %s", w.Code, w.Body)
	}
	if _, e = f.store.ChangeConversation(t.Context(), conv, updated.Revision, nil, &archived); e != nil {
		t.Fatal(e)
	}
	if w := post(otherBase+"/actions", "after-archive", next, f.cookie); w.Code < 400 {
		t.Fatal("archived action reached model")
	}
	connCount(1)
	if calls.Load() != 4 { // transport, duplicate-name form, clarification, and malformed model decision
		t.Errorf("unexpected number of model calls %d", calls.Load())
	}
}

func TestMCPOnboardingRejectsUnboundActorAndSensitiveQuestions(t *testing.T) {
	f := newDiscoveryA2UIFixture(t, nil)
	req := func(cookie *http.Cookie, text string) *httptest.ResponseRecorder {
		return f.request("POST", "/api/v1/conversations", "", map[string]string{"text": text, "client_request_id": "origin-auth-check"}, cookie)
	}
	if w := req(nil, "接入 MCP"); w.Code != 401 {
		t.Fatalf("anonymous %d", w.Code)
	}
	if w := req(f.cookie, "如何接入 MCP： https://private.example/private-mcp-endpoint"); w.Code != 400 || strings.Contains(w.Body.String(), "private.example") {
		t.Fatalf("sensitive question %d %s", w.Code, w.Body)
	}
	var auth work.Account
	created := f.account.request("POST", "/api/v1/admin/users", map[string]string{"username": "member", "password": "a different sufficiently long pass"}, f.cookie)
	if created.Code != 201 {
		t.Fatal(created.Body)
	}
	member := f.account.login("member", "a different sufficiently long pass")
	change := f.account.request("POST", "/api/v1/me/password", map[string]string{"current_password": "a different sufficiently long pass", "new_password": "a different replaced long pass"}, member)
	if change.Code != 204 {
		t.Fatal(change.Body)
	}
	member = f.account.login("member", "a different replaced long pass")
	var result struct{ User work.Account }
	accountJSON(t, f.account.request("GET", "/api/v1/session", nil, member), &result)
	auth = result.User
	// Route the member to a genuinely separate personal DB via the normal handler.
	memberStore, err := work.OpenAccountBusinessStore(t.TempDir()+"/member.db", f.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer memberStore.Close()
	if err := memberStore.BindAccountBusinessStore(t.Context(), auth.PrincipalID); err != nil {
		t.Fatal(err)
	}
	old := f.server.PersonalAPI
	f.server.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor == auth.PrincipalID {
			return f.server.PersonalHandler(memberStore), nil
		}
		return old(actor)
	}
	f.account.handler = f.server.Handler(nil)
	if w := req(member, "接入 MCP"); w.Code != 403 {
		t.Fatalf("member start %d %s", w.Code, w.Body)
	}
}

func TestMCPOnboardingIntentAndPrivateConfiguration(t *testing.T) {
	for _, text := range []string{"我有一个 MCP 要接入", "帮我接入 MCP 用于查询资产", "/mcp", `/assistant {"tool":"configure_mcp_connection"}`} {
		if !mcpSetupIntent(text) {
			t.Errorf("missed %q", text)
		}
	}
	for _, text := range []string{"查看已接入的 MCP", "如何接入 MCP", "不要接入 MCP", "查询 MCP 添加时间", `/assistant {"tool":"configure_mcp_connection","body":{"command":"/private"}}`} {
		if mcpSetupIntent(text) {
			t.Errorf("unexpected intent %q", text)
		}
	}
	for _, text := range []string{"MCP 密钥是 xxx", "MCP password: private", "MCP service https://private.invalid", "MCP bearer test_token"} {
		if !mcpSetupPayload(text) {
			t.Errorf("private payload allowed for %q", text)
		}
	}
}
