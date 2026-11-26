package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const approvalPreviewObject = "我的审批目标节点"
const approvalPreviewRecord = "local-pending-approval-fixture"
const approvalPreviewOnly = "未批准、拒绝或派发。"

type approvalPreviewFixture struct {
	*discoveryA2UIFixture
	db             *sql.DB
	calls          atomic.Int32
	authorizations int
}

func makeApprovalPreviewFixture(t *testing.T, answer func(*http.Request) string) *approvalPreviewFixture {
	t.Helper()
	f := &approvalPreviewFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16384))
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(raw, &request) != nil ||
			len(request.Input) != 1 || request.Input[0].Content != `{"requested_action":"preview_pending_approval"}` {
			t.Error("model did not receive exactly the bounded approval-preview action")
			w.WriteHeader(400)
			return
		}
		for _, secret := range []string{approvalPreviewObject, approvalPreviewRecord, "fixture-secret-parameters", "attention_slot", "fixture-ui-untrusted-text"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Error("approval details or browser content reached the model")
			}
		}
		f.calls.Add(1)
		out := `{"next":"show_approval_preview"}`
		if answer != nil {
			out = answer(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	t.Cleanup(provider.Close)
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "approval-preview-fixture", Protocol: model.ProtocolResponses,
			BaseURL: provider.URL, KeyRef: "env:CLUSBOT_APPROVAL_PREVIEW_TEST_MODEL_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	var err error
	f.db, err = sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	seedAssistantAttentionFixture(t, f.db, approvalPreviewRecord, f.actor, approvalPreviewObject)
	seedAssistantAttentionFixture(t, f.db, "not-mine-approval", "someone-else", "非本账号的待批准记录")
	now := time.Now().UnixMilli()
	if _, err = f.db.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,policy_version,valid_from,expires_at,owner,revision,created_at,updated_at)
VALUES('fixture-confirm-grant',?,'confirm',?,5,?,?,?,1,?,?)`, f.actor, approvalPreviewObject, now-10000, now+600000, f.actor, now-10000, now-10000); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRow("SELECT count(*) FROM authorization_decisions").Scan(&f.authorizations); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *approvalPreviewFixture) open(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"text": "查看待我处理", "client_message_id": "pending-approval-preview-source"})
	r := httptest.NewRequest("POST", "https://ops.example/api/v1/conversations", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://ops.example")
	r.Header.Set("X-ClusBot-Assistant-Cards", "1")
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.account.handler.ServeHTTP(w, r)
	var origin struct {
		Conversation string `json:"conversation_id"`
		Run          string `json:"run_id"`
	}
	accountJSON(t, w, &origin)
	if w.Code != 202 || origin.Conversation == "" || origin.Run == "" {
		t.Fatalf("wrong source run: %d %s", w.Code, w.Body)
	}
	path := "/api/v1/conversations/" + origin.Conversation + "/attention-card/" + origin.Run
	view := f.request("GET", path, "", nil, f.cookie)
	if view.Code != 200 || f.calls.Load() != 0 || strings.Contains(view.Body.String(), "非本账号的待批准记录") ||
		strings.Contains(view.Body.String(), approvalPreviewRecord) || strings.Contains(view.Body.String(), "审批只读预览") {
		t.Fatalf("unsigned disclosure/egress while loading attention list: %d %s", view.Code, view.Body)
	}
	var response a2uiDiscoveryReply
	accountJSON(t, view, &response)
	return path, origin.Run, renderedA2UIEvent(t, response, assistantApprovalPreviewAction)
}

func (f *approvalPreviewFixture) noDecision(t *testing.T) {
	t.Helper()
	var decision, by string
	var decided int64
	if err := f.db.QueryRow("SELECT decision,decided_by,decided_at FROM approvals WHERE approval_id=?", approvalPreviewRecord).Scan(&decision, &by, &decided); err != nil || decision != "pending" || by != "" || decided != 0 {
		t.Fatalf("read-only path changed approval: %q %q %d %v", decision, by, decided, err)
	}
	var n int
	if err := f.db.QueryRow("SELECT count(*) FROM authorization_decisions").Scan(&n); err != nil || n != f.authorizations {
		t.Fatalf("preview recorded an authorization decision: %d/%d %v", n, f.authorizations, err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM operations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("preview created an operation: %d %v", n, err)
	}
}

func TestAssistantApprovalPreviewScopedReadAndRestore(t *testing.T) {
	f := makeApprovalPreviewFixture(t, nil)
	path, run, event := f.open(t)
	fresh := f.account.login("admin", "correct horse battery staple")
	url := path + "/approval-preview"
	if res := f.request("POST", url, "fresh-login-cannot-reuse", event, fresh); res.Code != 409 {
		t.Fatalf("new login reused a click: %d %s", res.Code, res.Body)
	}
	cross := "/api/v1/conversations/" + strings.Repeat("a", 32) + "/attention-card/" + run + "/approval-preview"
	if res := f.request("POST", cross, "wrong-source-conversation", event, f.cookie); res.Code != 404 {
		t.Fatalf("wrong conversation: %d %s", res.Code, res.Body)
	}
	action := event["action"].(map[string]any)
	action["userMessage"] = "fixture-ui-untrusted-text"
	wire := action["context"].(map[string]any)
	wire["approval_id"] = approvalPreviewRecord
	if res := f.request("POST", url, "no-raw-approval-id", event, f.cookie); res.Code != 400 {
		t.Fatalf("client supplied approval id: %d %s", res.Code, res.Body)
	}
	delete(wire, "approval_id")
	action["name"] = "decide_approval"
	if res := f.request("POST", url, "no-embedded-approval", event, f.cookie); res.Code != 400 {
		t.Fatalf("client supplied approval action: %d %s", res.Code, res.Body)
	}
	action["name"] = assistantApprovalPreviewAction
	if f.calls.Load() != 0 {
		t.Fatal("forged action caused model call")
	}
	for range 2 {
		res := f.request("POST", url, "same-signed-read", event, f.cookie)
		body := res.Body.String()
		if res.Code != 200 || !strings.Contains(body, approvalPreviewOnly) || !strings.Contains(body, "合同修订 4 · 策略 5 · 资产修订 6") ||
			!strings.Contains(body, "待确认动作：maintain。对象："+approvalPreviewObject) || !strings.Contains(body, "本次核对时有确认权限") ||
			strings.Contains(body, "fixture-secret-parameters") || strings.Contains(body, approvalPreviewRecord) || f.calls.Load() != 1 {
			t.Fatalf("missing or repeated approval preview: %d %d %s", res.Code, f.calls.Load(), body)
		}
	}
	f.noDecision(t)
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if strings.HasPrefix(e.Kind, "assistant.approval_preview_snapshot:") {
			found = true
			if e.Data != "{}" {
				t.Fatal("private approval snapshot exposed in SSE")
			}
		}
	}
	if !found {
		t.Fatal("missing source-bound receipt")
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := work.OpenAccountBusinessStore(f.path, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.BindAccountBusinessStore(t.Context(), f.actor); err != nil {
		t.Fatal(err)
	}
	s := f.server
	s.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor != f.actor {
			return nil, work.ErrAccountDenied
		}
		return s.PersonalHandler(store), nil
	}
	f.account.handler = s.Handler(nil)
	res := f.request("GET", path, "", nil, fresh)
	if res.Code != 200 || !strings.Contains(res.Body.String(), approvalPreviewOnly) || f.calls.Load() != 1 {
		t.Fatalf("restart lost original receipt: %d %s", res.Code, res.Body)
	}
	if _, err = f.db.Exec("UPDATE authorizations SET revoked_at=? WHERE auth_id='fixture-confirm-grant'", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	res = f.request("GET", path, "", nil, fresh)
	if res.Code != 200 || strings.Contains(res.Body.String(), approvalPreviewOnly) || strings.Contains(res.Body.String(), "本次核对时有确认权限") {
		t.Fatalf("existing preview used revoked grant: %d %s", res.Code, res.Body)
	}
	f.noDecision(t)
}

func TestAssistantApprovalPreviewRejectsChangedAndExpiredRequests(t *testing.T) {
	for _, change := range []string{"parameters", "expiry", "subject", "approver"} {
		t.Run(change, func(t *testing.T) {
			f := makeApprovalPreviewFixture(t, nil)
			path, _, event := f.open(t)
			var err error
			switch change {
			case "parameters":
				_, err = f.db.Exec("UPDATE approvals SET params_digest='changed' WHERE approval_id=?", approvalPreviewRecord)
			case "expiry":
				_, err = f.db.Exec("UPDATE approvals SET expires_at=? WHERE approval_id=?", time.Now().UnixMilli()-1, approvalPreviewRecord)
			case "subject":
				_, err = f.db.Exec("UPDATE approvals SET subject='another-account' WHERE approval_id=?", approvalPreviewRecord)
			case "approver":
				_, err = f.db.Exec("UPDATE approvals SET decided_by='someone-else' WHERE approval_id=?", approvalPreviewRecord)
			}
			if err != nil {
				t.Fatal(err)
			}
			res := f.request("POST", path+"/approval-preview", "old-signed-view-changed", event, f.cookie)
			if res.Code != 409 || f.calls.Load() != 0 {
				t.Fatalf("changed approval reached model: %d %s", res.Code, res.Body)
			}
			if change != "approver" {
				f.noDecision(t)
			}
		})
	}
}

func TestAssistantApprovalPreviewChangedWhileModelWaits(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}()
	f := makeApprovalPreviewFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-done:
		case <-r.Context().Done():
		}
		return `{"next":"show_approval_preview"}`
	})
	path, _, event := f.open(t)
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- f.request("POST", path+"/approval-preview", "original-approval-not-updated", event, f.cookie)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not receive read intent")
	}
	if _, err := f.db.Exec("UPDATE approvals SET contract_revision=9 WHERE approval_id=?", approvalPreviewRecord); err != nil {
		t.Fatal(err)
	}
	close(done)
	select {
	case result := <-finished:
		if result.Code != 409 {
			t.Fatalf("changed proposal accepted after model: %d %s", result.Code, result.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval post did not settle")
	}
	view := f.request("GET", path, "", nil, f.cookie)
	if view.Code != 200 || strings.Contains(view.Body.String(), approvalPreviewOnly) || f.calls.Load() != 1 {
		t.Fatalf("stale model response stored: %d %s", view.Code, view.Body)
	}
	f.noDecision(t)
}

func TestAssistantApprovalPreviewRejectsModelDecision(t *testing.T) {
	f := makeApprovalPreviewFixture(t, func(*http.Request) string { return `{"next":"approve"}` })
	path, _, event := f.open(t)
	res := f.request("POST", path+"/approval-preview", "model-denied-to-approve", event, f.cookie)
	if res.Code != 502 || f.calls.Load() != 1 || strings.Contains(res.Body.String(), approvalPreviewOnly) {
		t.Fatalf("model became approver: %d %s", res.Code, res.Body)
	}
	f.noDecision(t)
}

func TestAssistantApprovalPreviewBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local browser tooling")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(stage string, stdin []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", stage, err, stderr.String())
		}
		return out
	}
	var files map[string][]byte
	if err := json.Unmarshal(browser("bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("in-memory app unavailable: %v", err)
	}
	assets := fstest.MapFS{}
	for filename, data := range files {
		assets[filename] = &fstest.MapFile{Data: data}
	}
	f := makeApprovalPreviewFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	api := f.server
	api.Origin, api.Hosts = origin, []string{addr}
	server.Config.Handler = api.Handler(assets)
	server.Start()
	defer server.Close()
	args, _ := json.Marshal(map[string]string{"origin": origin, "title": approvalPreviewObject})
	browser("browse-approval-preview", args)
	if f.calls.Load() != 1 {
		t.Fatalf("retrieval replayed the model: %d", f.calls.Load())
	}
	f.noDecision(t)
}
