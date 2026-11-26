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
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAssistantAssetCardScopeAndReceipt(t *testing.T) {
	const first, second, hidden, serial = "POD-fixture-a", "POD-fixture-b", "POD-private-no-read", "private-hardware-identity"
	var modelCalls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	t.Setenv("CLUSBOT_ASSET_CARD_FIXTURE_KEY", "local-fixture-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := modelCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &request); err != nil || len(request.Input) != 1 ||
			request.Input[0].Content != `{"requested_action":"query_asset_ledger"}` {
			t.Error("model did not receive exactly the approved action")
		}
		for _, blocked := range []string{first, second, hidden, serial, "asset_slot", "private-message"} {
			if bytes.Contains(raw, []byte(blocked)) {
				t.Errorf("model request exposed %q", blocked)
			}
		}
		if call == 2 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", `{"next":"show_ledger"}`)
	}))
	defer provider.Close()
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "asset-card-fixture",
			KeyRef: "env:CLUSBOT_ASSET_CARD_FIXTURE_KEY", Protocol: model.ProtocolResponses}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	if err := f.store.ImportDomainAssetsForAccount(t.Context(), f.actor, []work.DomainAssetEntry{
		{ObjectRef: first, Kind: "pod", Identity: serial, State: work.AssetStateInService, Read: true},
		{ObjectRef: second, Kind: "pod", Identity: serial + "-other", State: work.AssetStateRecheck, Read: true},
		{ObjectRef: hidden, Kind: "pod", Identity: serial + "-hidden"},
	}); err != nil {
		t.Fatal(err)
	}
	created := f.account.request("POST", "/api/v1/admin/users", map[string]string{
		"username": "asset-member", "password": "separate asset fixture password",
	}, f.cookie)
	if created.Code != 201 {
		t.Fatalf("member setup: %d %s", created.Code, created.Body)
	}
	var member struct{ User work.Account }
	accountJSON(t, created, &member)
	memberCookie := f.account.login("asset-member", "separate asset fixture password")
	memberStore, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "member.sqlite"), f.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer memberStore.Close()
	if err = memberStore.BindAccountBusinessStore(t.Context(), member.User.PrincipalID); err != nil {
		t.Fatal(err)
	}
	server := f.server
	server.PersonalAPI = func(actor string) (http.Handler, error) {
		switch actor {
		case f.actor:
			return server.PersonalHandler(f.store), nil
		case member.User.PrincipalID:
			return server.PersonalHandler(memberStore), nil
		default:
			return nil, work.ErrAccountDenied
		}
	}
	f.account.handler = server.Handler(nil)
	request := func(method, path, key string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(raw))
		if method != "GET" {
			r.Header.Set("Origin", "https://ops.example")
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if key != "" {
			r.Header.Set("X-ClusBot-Request-Key", key)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.account.handler.ServeHTTP(w, r)
		return w
	}
	const prompt = "查看我的资产？"
	open := func(cookie *http.Cookie, key string) (string, string, string) {
		t.Helper()
		w := request("POST", "/api/v1/conversations", "", map[string]string{"text": prompt, "client_message_id": key}, cookie)
		if w.Code != 202 {
			t.Fatalf("opening asset ledger: %d %s", w.Code, w.Body)
		}
		var receipt struct {
			Conversation string `json:"conversation_id"`
			Run          string `json:"run_id"`
		}
		accountJSON(t, w, &receipt)
		if !validID(receipt.Run) || !validID(receipt.Conversation) {
			t.Fatalf("invalid receipt %+v", receipt)
		}
		return "/api/v1/conversations/" + receipt.Conversation + "/asset-card/" + receipt.Run, receipt.Conversation, receipt.Run
	}
	base, conversation, run := open(f.cookie, "asset-ledger-composer-1")
	timeline, err := f.store.Detail(conversation, false)
	if err != nil || len(timeline.Messages) < 2 || timeline.Messages[0].Text != prompt ||
		!timeline.Messages[len(timeline.Messages)-1].AssistantAssetList {
		t.Fatalf("asset ledger is not anchored in the original chat: %+v %v", timeline, err)
	}
	view := request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || view.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(view.Body.String(), first) || !strings.Contains(view.Body.String(), second) ||
		strings.Contains(view.Body.String(), hidden) || strings.Contains(view.Body.String(), serial) {
		t.Fatalf("asset ledger listed an unauthorized field or missed visible rows: %d %s", view.Code, view.Body)
	}
	var card a2uiDiscoveryReply
	accountJSON(t, view, &card)
	firstEvent := renderedA2UIEvent(t, card, assistantAssetLedgerAction)
	body := firstEvent["action"].(map[string]any)
	choice := body["context"].(map[string]any)
	body["name"] = "operate_asset"
	if w := request("POST", base+"/actions", "asset-false-action", firstEvent, f.cookie); w.Code != 400 {
		t.Fatalf("unsupported action accepted: %d %s", w.Code, w.Body)
	}
	body["name"] = assistantAssetLedgerAction
	choice["asset_slot"] = "2"
	if w := request("POST", base+"/actions", "asset-false-target", firstEvent, f.cookie); w.Code != 409 {
		t.Fatalf("swapped slot was accepted: %d %s", w.Code, w.Body)
	}
	choice["asset_slot"] = "1"
	choice["object_ref"] = first
	if w := request("POST", base+"/actions", "asset-overbroad-target", firstEvent, f.cookie); w.Code != 400 {
		t.Fatalf("unexpected context field accepted: %d %s", w.Code, w.Body)
	}
	delete(choice, "object_ref")
	body["userMessage"] = "private-message"
	body["metadata"] = map[string]any{"extensions": map[string]any{"client": "private-message"}}
	for _, check := range []struct {
		cookie *http.Cookie
		code   int
	}{
		{nil, 401},
		{memberCookie, 404},
		{f.account.login("admin", "correct horse battery staple"), 409},
	} {
		w := request("POST", base+"/actions", "asset-unauthorized-session", firstEvent, check.cookie)
		if w.Code != check.code || strings.Contains(w.Body.String(), first) || modelCalls.Load() != 0 {
			t.Fatalf("stale or foreign action reached facts or model: %d != %d %s", w.Code, check.code, w.Body)
		}
	}
	memberBase, _, _ := open(memberCookie, "asset-ledger-member-1")
	view = request("GET", memberBase, "", nil, memberCookie)
	if view.Code != 200 || !strings.Contains(view.Body.String(), "暂无可读的资产台账") {
		t.Fatalf("ordinary account saw somebody else's ledger or could not open: %d %s", view.Code, view.Body)
	}
	submit := func(key string, event any) *httptest.ResponseRecorder {
		return request("POST", base+"/actions", key, event, f.cookie)
	}
	before, err := f.store.DomainSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	const resultText = "已核对台账（记录 1）：在役 · 健康：未记录 · 可达性：未记录 · 业务占用：未记录"
	for range 2 {
		w := submit("asset-valid-receipt-key", firstEvent)
		if w.Code != 200 || !strings.Contains(w.Body.String(), resultText) || modelCalls.Load() != 1 {
			t.Fatalf("wrong receipt or duplicate dispatch: %d calls=%d %s", w.Code, modelCalls.Load(), w.Body)
		}
	}
	assetSnapshot, err := f.store.DomainSnapshot(t.Context())
	if err != nil || len(assetSnapshot.Assets) != len(before.Assets) ||
		assetSnapshot.Assets[0] != before.Assets[0] || len(assetSnapshot.AssetTransitions) != len(before.AssetTransitions) ||
		len(assetSnapshot.Operations) != len(before.Operations) {
		t.Fatalf("ledger read changed a business object: %v", err)
	}
	// Lose process-local signer and reopen the actual private SQLite database.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := work.OpenAccountBusinessStore(f.path, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = reopened
	f.account.handler = server.Handler(nil)
	view = request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || !strings.Contains(view.Body.String(), resultText) || modelCalls.Load() != 1 {
		t.Fatalf("read receipt missing after reopen: %d %s", view.Code, view.Body)
	}
	// Generic Run replay must not bypass the ledger's live permission projection.
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("event replay: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Data, first) || strings.Contains(event.Data, second) || strings.Contains(event.Data, serial) ||
			(strings.HasPrefix(event.Kind, "assistant.asset_") && event.Data != "{}") {
			t.Fatalf("private ledger data in generic replay: %s %s", event.Kind, event.Data)
		}
	}
	var current a2uiDiscoveryReply
	accountJSON(t, view, &current)
	secondEvent := renderedA2UIEvent(t, current, assistantAssetLedgerAction)
	if w := submit("asset-valid-receipt-key", secondEvent); w.Code != 409 || modelCalls.Load() != 1 {
		t.Fatalf("original receipt key selected another asset: %d %s", w.Code, w.Body)
	}
	revoke := func(ref string) {
		t.Helper()
		for _, auth := range before.Authorizations {
			if auth.ObjectRef == ref && auth.Subject == f.actor && auth.Action == "read" {
				if err := f.store.RevokeAuthorizationByID(t.Context(), auth.AuthID, f.actor); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("asset fixture did not create its scoped authorization")
	}
	revoke(first)
	view = request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || strings.Contains(view.Body.String(), first) || strings.Contains(view.Body.String(), resultText) ||
		!strings.Contains(view.Body.String(), "有资产已变更或不可查看") {
		t.Fatalf("revoked asset still shown in saved card: %d %s", view.Code, view.Body)
	}
	if w := submit("asset-revoked-key", firstEvent); w.Code != 409 || modelCalls.Load() != 1 {
		t.Fatalf("revoked asset reached the model: %d %s", w.Code, w.Body)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- submit("asset-racing-read", secondEvent) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture model not reached")
	}
	revoke(second)
	close(release)
	if w := <-result; w.Code != 409 || strings.Contains(w.Body.String(), second) || modelCalls.Load() != 2 {
		t.Fatalf("grant revoked during interpretation still returned facts: %d %s", w.Code, w.Body)
	}
}
