package httpapi

import (
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

func checkUnboundApprovalConflict(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	body := response.Body.String()
	if response.Code != http.StatusConflict || !strings.Contains(body, `"code":"approval_source_unverified"`) ||
		!strings.Contains(body, "未提交决定") || strings.Contains(body, "fixture-secret-parameters") {
		t.Fatalf("unbound approval decision did not fail closed: %d %s", response.Code, body)
	}
}

func TestUnboundPendingApprovalDecisionHTTP(t *testing.T) {
	f := makeApprovalPreviewFixture(t, nil)
	const approvalPath = "/api/v1/approvals/" + approvalPreviewRecord + "/decision"
	for _, decision := range []string{"approved", "rejected"} {
		checkUnboundApprovalConflict(t, f.request("POST", approvalPath, "", map[string]string{"decision": decision}, f.cookie))
	}
	if response := f.request("POST", approvalPath, "", map[string]any{"decision": "approved", "contract_revision": 4, "policy_version": 5, "asset_revision": 6}, f.cookie); response.Code != 400 {
		t.Fatalf("client-supplied version fields were accepted: %d %s", response.Code, response.Body)
	}
	if response := f.request("GET", "/api/v1/decisions", "", nil, f.cookie); response.Code != 200 || !strings.Contains(response.Body.String(), approvalPreviewRecord) {
		t.Fatalf("refusal hid a genuine pending decision: %d %s", response.Code, response.Body)
	}
	f.noDecision(t)
	// A pre-existing final result remains observable but cannot be overwritten.
	if _, err := f.db.Exec("UPDATE approvals SET decision='approved',decided_by=?,decided_at=? WHERE approval_id=?", f.actor, time.Now().UnixMilli(), approvalPreviewRecord); err != nil {
		t.Fatal(err)
	}
	if response := f.request("POST", approvalPath, "", map[string]string{"decision": "rejected"}, f.cookie); response.Code != 200 || !strings.Contains(response.Body.String(), `"decision":"approved"`) {
		t.Fatalf("old terminal result was not preserved: %d %s", response.Code, response.Body)
	}
	var decided string
	if err := f.db.QueryRow("SELECT decision FROM approvals WHERE approval_id=?", approvalPreviewRecord).Scan(&decided); err != nil || decided != "approved" {
		t.Fatalf("terminal record overwritten: %s %v", decided, err)
	}
	const expiredID = "local-unbound-decision-expiry"
	seedAssistantAttentionFixture(t, f.db, expiredID, f.actor, approvalPreviewObject)
	if _, err := f.db.Exec("UPDATE approvals SET expires_at=? WHERE approval_id=?", time.Now().UnixMilli()-100, expiredID); err != nil {
		t.Fatal(err)
	}
	if response := f.request("POST", "/api/v1/approvals/"+expiredID+"/decision", "", map[string]string{"decision": "approved"}, f.cookie); response.Code != 409 || !strings.Contains(response.Body.String(), `"code":"approval_expired"`) {
		t.Fatalf("original expiry behavior lost: %d %s", response.Code, response.Body)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM attention_feed_events WHERE item_id=? AND state='expired'", "approval:"+expiredID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("authoritative expiry was not recorded exactly once: %d %v", count, err)
	}
	if f.calls.Load() != 0 {
		t.Fatalf("direct route invoked general provider: %d", f.calls.Load())
	}
}

func TestUnboundPendingApprovalDecisionAfterModelConfirmation(t *testing.T) {
	const text = "对这条测试夹具里的审批提交批准决定"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
		var request struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(body, &request) != nil || len(request.Input) != 1 ||
			!strings.Contains(request.Input[0].Content, text) || !strings.Contains(request.Input[0].Content, "decide_approval") ||
			strings.Contains(request.Input[0].Content, "fixture-secret-parameters") {
			t.Error("controlled model did not receive the explicit tool command or received private data")
			http.Error(w, "unexpected local fixture input", 400)
			return
		}
		calls.Add(1)
		result := fmt.Sprintf(`{"tool":"decide_approval","params":{"id":%q},"body":{"decision":"approved"}}`, approvalPreviewRecord)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", result)
	}))
	defer provider.Close()
	base := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "fixture-approval-guard", Protocol: model.ProtocolResponses, BaseURL: provider.URL,
			KeyRef: "env:CLUSBOT_APPROVAL_GUARD_TEST_MODEL_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	db, err := sql.Open("sqlite", base.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := &approvalPreviewFixture{discoveryA2UIFixture: base, db: db}
	seedAssistantAttentionFixture(t, db, approvalPreviewRecord, f.actor, approvalPreviewObject)
	stamp := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO authorizations
(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at)
VALUES('model-guard-confirm-grant',?,'confirm',?,?,?,?,?)`, f.actor, approvalPreviewObject, stamp-1000, f.actor, stamp-1000, stamp-1000); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM authorization_decisions").Scan(&f.authorizations); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/v1/assistant/interpret", "", map[string]string{"text": text}, f.cookie)
	var proposal struct {
		State string `json:"state"`
		Token string `json:"confirmation_id"`
	}
	accountJSON(t, response, &proposal)
	if response.Code != 200 || proposal.State != "confirmation_required" || proposal.Token == "" || calls.Load() != 1 {
		t.Fatalf("fixture never reached the independent confirmation flow: %d %s calls=%d", response.Code, response.Body, calls.Load())
	}
	f.noDecision(t)
	input := map[string]string{"confirmation_id": proposal.Token}
	checkUnboundApprovalConflict(t, f.request("POST", "/api/v1/assistant/confirm", "", input, f.cookie))
	if response = f.request("POST", "/api/v1/assistant/confirm", "", input, f.cookie); response.Code != 409 || !strings.Contains(response.Body.String(), `"code":"assistant_confirmation_expired"`) {
		t.Fatalf("confirmation could replay: %d %s", response.Code, response.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("confirmation resent model traffic: %d", calls.Load())
	}
	f.noDecision(t)
}
