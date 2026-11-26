package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func seedAssistantAttentionFixture(t *testing.T, db *sql.DB, id, actor, target string) {
	t.Helper()
	stamp := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO approvals(approval_id,subject,action,object_ref,params_digest,
contract_revision,policy_version,asset_revision,decided_by,expires_at,decision,request_key,created_at)
VALUES(?,?, 'maintain',?, 'fixture-secret-parameters', 4,5,6,'',?, 'pending',?,?)`,
		id, actor, target, stamp+600000, id, stamp-1000); err != nil {
		t.Fatal(err)
	}
}

func TestAssistantAttentionCardScopeAndNoDecision(t *testing.T) {
	const own, foreign, ownApproval = "我的测试节点", "非本账号的测试节点", "attention-own-fixture"
	var calls atomic.Int32
	var response atomic.Value
	response.Store(`{"next":"approve"}`)
	t.Setenv("CLUSBOT_ATTENTION_CARD_FIXTURE_KEY", "local-fixture-only")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var modelInput struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if json.Unmarshal(body, &modelInput) != nil || len(modelInput.Input) != 1 ||
			modelInput.Input[0].Content != `{"requested_action":"query_attention_reason"}` {
			t.Error("model did not receive exactly the declared action")
		}
		for _, field := range []string{own, foreign, ownApproval, "fixture-secret-parameters", "attention_slot", "untrusted-client"} {
			if bytes.Contains(body, []byte(field)) {
				t.Errorf("sensitive attention detail entered model: %q", field)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", response.Load().(string))
	}))
	defer provider.Close()
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "attention-fixture", Protocol: model.ProtocolResponses, BaseURL: provider.URL, KeyRef: "env:CLUSBOT_ATTENTION_CARD_FIXTURE_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAssistantAttentionFixture(t, db, ownApproval, f.actor, own)
	seedAssistantAttentionFixture(t, db, "attention-foreign-fixture", "another-principal", foreign)
	request := func(method, path, key string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(data))
		if method != "GET" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://ops.example")
		}
		req.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if key != "" {
			req.Header.Set("X-ClusBot-Request-Key", key)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.account.handler.ServeHTTP(w, req)
		return w
	}
	opened := request("POST", "/api/v1/conversations", "", map[string]string{"text": "查看待我处理", "client_message_id": "fixture-attention-card"}, f.cookie)
	if opened.Code != 202 {
		t.Fatalf("composer: %d %s", opened.Code, opened.Body)
	}
	var origin struct {
		Conversation string `json:"conversation_id"`
		Run          string `json:"run_id"`
	}
	accountJSON(t, opened, &origin)
	base := "/api/v1/conversations/" + origin.Conversation + "/attention-card/" + origin.Run
	timeline, err := f.store.Detail(origin.Conversation, false)
	if err != nil || len(timeline.Messages) < 2 || !timeline.Messages[len(timeline.Messages)-1].AssistantAttentionList {
		t.Fatalf("attention history is not attached to the original message: %v", err)
	}
	view := request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || !strings.Contains(view.Body.String(), own) || strings.Contains(view.Body.String(), foreign) ||
		strings.Contains(view.Body.String(), ownApproval) || calls.Load() != 0 || view.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("attention list escaped its scope: %d %s", view.Code, view.Body)
	}
	var surface a2uiDiscoveryReply
	accountJSON(t, view, &surface)
	action := renderedA2UIEvent(t, surface, assistantAttentionReasonAction)
	body := action["action"].(map[string]any)
	body["userMessage"] = "untrusted-client"
	ctx := body["context"].(map[string]any)
	ctx["approval_id"] = ownApproval
	if w := request("POST", base+"/actions", "attention-overbroad", action, f.cookie); w.Code != 400 || calls.Load() != 0 {
		t.Fatalf("unexpected fields reached model: %d %s", w.Code, w.Body)
	}
	delete(ctx, "approval_id")
	body["name"] = "decide_approval"
	if w := request("POST", base+"/actions", "attention-unapproved-command", action, f.cookie); w.Code != 400 || calls.Load() != 0 {
		t.Fatalf("arbitrary action reached model: %d %s", w.Code, w.Body)
	}
	body["name"] = assistantAttentionReasonAction
	newCookie := f.account.login("admin", "correct horse battery staple")
	if w := request("POST", base+"/actions", "attention-other-session", action, newCookie); w.Code != 409 || calls.Load() != 0 {
		t.Fatalf("different session used old signed card: %d %s", w.Code, w.Body)
	}
	if w := request("POST", base+"/actions", "attention-model-write", action, f.cookie); w.Code != 502 || calls.Load() != 1 {
		t.Fatalf("model returned an unapproved action: %d %s", w.Code, w.Body)
	}
	response.Store(`{"next":"show_reason"}`)
	const terminalReceipt = "等待你的决定。依据：本人审批记录。未提交决定或重新执行任务。"
	for range 2 {
		w := request("POST", base+"/actions", "attention-read-once-key", action, f.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), terminalReceipt) || calls.Load() != 2 {
			t.Fatalf("wrong or repeated receipt: %d calls=%d %s", w.Code, calls.Load(), w.Body)
		}
	}
	var decision, decidedBy string
	if err := db.QueryRow("SELECT decision,decided_by FROM approvals WHERE approval_id=?", ownApproval).Scan(&decision, &decidedBy); err != nil || decision != "pending" || decidedBy != "" {
		t.Fatalf("read changed approval: %s %q %v", decision, decidedBy, err)
	}
	f.account.handler = f.server.Handler(nil)
	if w := request("GET", base, "", nil, f.cookie); w.Code != 200 || !strings.Contains(w.Body.String(), terminalReceipt) || calls.Load() != 2 {
		t.Fatalf("historical receipt missing after handler restart: %d %s", w.Code, w.Body)
	}
	if _, err := db.Exec("UPDATE approvals SET decision='approved',decided_at=? WHERE approval_id=?", time.Now().UnixMilli(), ownApproval); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", base, "", nil, f.cookie); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "审批已同意") || !strings.Contains(w.Body.String(), terminalReceipt) || calls.Load() != 2 {
		t.Fatalf("current state conflated with prior read: %d %s", w.Code, w.Body)
	}
	if _, err := db.Exec("UPDATE approvals SET subject='another-principal' WHERE approval_id=?", ownApproval); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", base, "", nil, f.cookie); w.Code != 200 || strings.Contains(w.Body.String(), own) ||
		strings.Contains(w.Body.String(), terminalReceipt) || calls.Load() != 2 {
		t.Fatalf("scope change exposed old approval: %d %s", w.Code, w.Body)
	}
	events, _, err := f.store.ReplayRunEvents(origin.Run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Kind, "assistant.attention_") && e.Data != "{}" {
			t.Fatalf("private raw facts in public replay: %s", e.Kind)
		}
	}
}
