package httpapi

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssistantToolConfirmationUsesOriginalRevisionAndIsSingleUse(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	defer a.close()
	const tools = "/api/v1/assistant/tools"
	const confirm = "/api/v1/assistant/confirm"
	code, preview := a.post(tools, `{"tool":"create_task","body":{"text":"整理月报"}}`)
	if code != http.StatusOK || preview["state"] != "confirmation_required" {
		t.Fatalf("expected pending confirmation: %d %+v", code, preview)
	}
	token, ok := preview["confirmation_id"].(string)
	if !ok {
		t.Fatal("missing confirmation ID")
	}
	if code, result := a.post(confirm, `{"confirmation_id":"`+token+`","body":{"text":"different task"}}`); code != http.StatusBadRequest {
		t.Fatalf("accepted changes during confirmation: %d %+v", code, result)
	}
	if code, result := a.post(confirm, `{"confirmation_id":"`+token+`"}`); code != 200 || result["state"] != "completed" {
		t.Fatalf("confirmed request failed: %d %+v", code, result)
	}
	if code, _ := a.post(confirm, `{"confirmation_id":"`+token+`"}`); code != http.StatusConflict {
		t.Fatalf("confirmation was reusable: %d", code)
	}
	if code, _ := a.post(tools, `{"tool":"open_arbitrary_http","params":{"path":"/api/v1/settings"}}`); code != 404 {
		t.Fatalf("unregistered route was accepted: %d", code)
	}
	if code, result := a.post("/api/v1/conversations", `{"text":"/助手 {\"tool\":\"get_current_model_routing\"}","client_request_id":"assistant-model-123"}`); code != 200 || result["state"] != "completed" {
		t.Fatalf("conversation model query failed: %d %+v", code, result)
	}
}

func TestAssistantConfirmationCannotMoveBetweenSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.setup()
	first := f.login("admin", "correct horse battery staple")
	second := f.login("admin", "correct horse battery staple")
	input := map[string]any{"tool": "create_task", "body": map[string]string{"client_request_id": "session-bound-123", "text": "整理记录"}}
	const tools, confirm = "/api/v1/assistant/tools", "/api/v1/assistant/confirm"
	if rec := f.request("POST", tools, input, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated assistant action returned %d", rec.Code)
	}
	rec := f.request("POST", tools, input, first)
	if rec.Code != 200 {
		t.Fatalf("preview failed: %d %s", rec.Code, rec.Body)
	}
	var preview map[string]any
	accountJSON(t, rec, &preview)
	body := map[string]any{"confirmation_id": preview["confirmation_id"]}
	if rec := f.request("POST", confirm, body, second); rec.Code != 409 {
		t.Fatalf("confirmation crossed sessions: %d", rec.Code)
	}
	if rec := f.request("POST", "/api/v1/assistant/interpret", map[string]string{"text": "我要修改密码 不应出现在回执的内容"}, first); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), "form_required") || strings.Contains(rec.Body.String(), "不应出现在回执") {
		t.Fatalf("password request did not use the trusted form: %d %s", rec.Code, rec.Body)
	}
	if rec := f.request("POST", confirm, body, first); rec.Code != 503 {
		t.Fatalf("valid confirmation did not reach existing business gate: %d %s", rec.Code, rec.Body)
	}
}
