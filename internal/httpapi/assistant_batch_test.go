package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func assistantBatchItems(t *testing.T, response map[string]any, count int) []map[string]any {
	t.Helper()
	if response["state"] != "batch_completed" {
		t.Fatalf("expected batch receipt: %+v", response)
	}
	values, ok := response["items"].([]any)
	if !ok || len(values) != count {
		t.Fatalf("expected %d batch items: %+v", count, response)
	}
	items := make([]map[string]any, count)
	for i, value := range values {
		var ok bool
		items[i], ok = value.(map[string]any)
		if !ok {
			t.Fatalf("item %d is not an object: %+v", i, value)
		}
	}
	return items
}

func TestAssistantBatchUsesIndependentConfirmations(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "batch.sqlite"))
	defer a.close()
	code, response := a.post("/api/v1/assistant/tools", `{"calls":[
		{"tool":"list_tasks"},
		{"tool":"configure_model_profile"},
		{"tool":"create_task","body":{"text":"整理第一份记录"}},
		{"tool":"create_task","body":{"text":"整理第二份记录"}},
		{"tool":"open_arbitrary_http"},
		{"tool":"get_task"}
	]}`)
	if code != http.StatusOK {
		t.Fatalf("batch returned %d: %+v", code, response)
	}
	items := assistantBatchItems(t, response, 6)
	for i, state := range []string{"completed", "form_required", "confirmation_required", "confirmation_required", "failed", "failed"} {
		if items[i]["state"] != state {
			t.Fatalf("item %d: expected %s, got %+v", i, state, items[i])
		}
	}
	if items[0]["tool"] != "list_tasks" || items[0]["surface"] == nil || items[1]["tool"] != "configure_model_profile" ||
		items[4]["tool"] != "unknown" || items[5]["tool"] != "get_task" || items[4]["message"] == "" || items[5]["message"] == "" {
		t.Fatalf("bad item shape or ordering: %+v", items)
	}
	first, firstOK := items[2]["confirmation_id"].(string)
	second, secondOK := items[3]["confirmation_id"].(string)
	if !firstOK || !secondOK || first == second || len(first) != 48 || len(second) != 48 {
		t.Fatalf("confirmations are not independent: %+v", items)
	}
	requestKey := func(item map[string]any) string {
		t.Helper()
		input, ok := item["input"].(map[string]any)
		if !ok {
			t.Fatalf("missing confirmation input: %+v", item)
		}
		body, ok := input["body"].(map[string]any)
		if !ok {
			t.Fatalf("missing confirmation body: %+v", item)
		}
		key, ok := body["client_request_id"].(string)
		if !ok || key == "" {
			t.Fatalf("missing request key: %+v", item)
		}
		return key
	}
	firstKey, secondKey := requestKey(items[2]), requestKey(items[3])
	for _, key := range []string{firstKey, secondKey} {
		if code, result := a.getAdmission(key); code != http.StatusNotFound {
			t.Fatalf("batch preview submitted an admission: %d %+v", code, result)
		}
	}
	if tasks, err := a.store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("a batch preview created tasks: %+v %v", tasks, err)
	}
	if code, result := a.post("/api/v1/assistant/confirm", `{"confirmation_id":"`+second+`"}`); code != 200 || result["state"] != "completed" {
		t.Fatalf("second confirmation failed: %d %+v", code, result)
	}
	if code, result := a.getAdmission(secondKey); code != http.StatusOK {
		t.Fatalf("confirmed item was not admitted: %d %+v", code, result)
	}
	if code, result := a.getAdmission(firstKey); code != http.StatusNotFound {
		t.Fatalf("confirming one item admitted the other: %d %+v", code, result)
	}
	if code, _ := a.post("/api/v1/assistant/confirm", `{"confirmation_id":"`+second+`"}`); code != http.StatusConflict {
		t.Fatalf("confirmation reused: %d", code)
	}
	request := a.request("DELETE", "/api/v1/assistant/confirm/"+first, "")
	responseHTTP, err := a.ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseHTTP.Body.Close()
	if responseHTTP.StatusCode != http.StatusNoContent {
		t.Fatalf("discarding the other item returned %d", responseHTTP.StatusCode)
	}
	if code, result := a.getAdmission(firstKey); code != http.StatusNotFound {
		t.Fatalf("discarding one item submitted an admission: %d %+v", code, result)
	}
}

func TestAssistantBatchBoundsAndSensitiveInputs(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "batch-invalid.sqlite"))
	defer a.close()
	for _, body := range []string{
		`{"calls":[]}`, `{"calls":null}`, `{"calls":[{"tool":"list_tasks"}]}`,
		`{"calls":[{},{},{},{},{},{},{}]}`, `{"calls":[{},{}],"tool":"create_task"}`,
		`{"calls":[{},{}],"extra":"value"}`,
	} {
		if code, response := a.post("/api/v1/assistant/tools", body); code != http.StatusBadRequest {
			t.Fatalf("accepted malformed batch %s: %d %+v", body, code, response)
		}
	}
	const secret = "fixture-private-value-12345"
	code, response := a.post("/api/v1/assistant/tools", `{"calls":[
		{"tool":"create_task","body":{"text":"整理","password":"`+secret+`"}},
		{"tool":"configure_model_provider","body":{"api_key":"`+secret+`"}},
		{"tool":"create_task","body":{"text":"api_key=`+secret+`"}},
		{"tool":"list_tasks","query":{"status":"not-a-status"}},
		{"tool":"list_tasks"},
		{"tool":"list_tasks","extra":"`+secret+`"}
	]}`)
	if code != 200 {
		t.Fatalf("mixed batch returned %d: %+v", code, response)
	}
	items := assistantBatchItems(t, response, 6)
	for _, index := range []int{0, 1, 2, 3, 5} {
		if items[index]["state"] != "failed" || items[index]["message"] == "" {
			t.Fatalf("item %d was not an explicit failure: %+v", index, items[index])
		}
	}
	if items[4]["state"] != "completed" {
		t.Fatalf("failure blocked an independent read: %+v", items[4])
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("batch response exposed a credential")
	}
	if tasks, err := a.store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("invalid batch created tasks: %+v %v", tasks, err)
	}
}

func TestAssistantConversationBatchAndDeterministicReads(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "batch-conversation.sqlite"))
	defer a.close()
	const args = `{"calls":[{"tool":"list_tasks"},{"tool":"create_task","body":{"text":"不能直接执行"}},{"tool":"configure_model_profile"},{"tool":"get_discovery_setup_status"}]}`
	submission, _ := json.Marshal(map[string]string{"text": "/助手 " + args, "client_request_id": "assistant-batch-001"})
	request := a.request("POST", "/api/v1/conversations", string(submission))
	request.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
	httpResponse, err := a.ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResponse.Body.Close()
	var response map[string]any
	if err := json.NewDecoder(httpResponse.Body).Decode(&response); err != nil || httpResponse.StatusCode != 200 {
		t.Fatalf("conversation batch: %d %+v %v", httpResponse.StatusCode, response, err)
	}
	items := assistantBatchItems(t, response, 4)
	for i, state := range []string{"completed", "failed", "failed", "completed"} {
		if items[i]["state"] != state {
			t.Fatalf("deterministic item %d: %+v", i, items[i])
		}
	}
	if tasks, err := a.store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("deterministic query wrote tasks: %+v %v", tasks, err)
	}
}

func TestAssistantBatchChecksAccountPermissionsPerItem(t *testing.T) {
	f := newAccountFixture(t)
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	if response := f.request("POST", "/api/v1/admin/users", map[string]any{"username": "member", "password": "member password 123"}, admin); response.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", response.Code, response.Body)
	}
	member := f.login("member", "member password 123")
	body := map[string]any{"calls": []any{
		map[string]any{"tool": "get_my_account"},
		map[string]any{"tool": "list_accounts"},
		map[string]any{"tool": "configure_model_profile"},
		map[string]any{"tool": "configure_conversation"},
		map[string]any{"tool": "create_task", "body": map[string]string{"text": "成员自己的工作"}},
	}}
	if response := f.request("POST", "/api/v1/assistant/tools", body, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated batch returned %d", response.Code)
	}
	rec := f.request("POST", "/api/v1/assistant/tools", body, member)
	var response map[string]any
	accountJSON(t, rec, &response)
	if rec.Code != 200 {
		t.Fatalf("member batch: %d %+v", rec.Code, response)
	}
	items := assistantBatchItems(t, response, 5)
	for i, state := range []string{"completed", "failed", "failed", "form_required", "confirmation_required"} {
		if items[i]["state"] != state {
			t.Fatalf("member item %d: %+v", i, items[i])
		}
	}
	other := f.login("member", "member password 123")
	confirmation := map[string]any{"confirmation_id": items[4]["confirmation_id"]}
	if response := f.request("POST", "/api/v1/assistant/confirm", confirmation, other); response.Code != http.StatusConflict {
		t.Fatalf("batch confirmation crossed sessions: %d %s", response.Code, response.Body)
	}
}

func TestAssistantModelBatchProposalUsesExistingGates(t *testing.T) {
	var calls atomic.Int32
	const secret = "fixture-model-secret-123"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.Contains(input.Instructions, `"calls"`) {
			t.Errorf("model did not receive batch instructions: %v", err)
		}
		answer := `{"calls":[{"tool":"list_tasks"},{"tool":"create_task","body":{"text":"整理记录"}},{"tool":"open_arbitrary_http"},{"tool":"create_task","body":{"api_key":"` + secret + `"}}]}`
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer provider.Close()
	store, err := work.Open(filepath.Join(t.TempDir(), "model-batch.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "fixture", Protocol: model.ProtocolResponses, KeyRef: "env:CLUSBOT_BATCH_MODEL_KEY"}); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Token: "a1-token", Hosts: []string{"127.0.0.1"}}
	a := &a1Server{ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})), store: store}
	defer a.close()
	a.cookie = a.login()
	code, response := a.post("/api/v1/assistant/interpret", `{"text":"查看任务，并整理记录"}`)
	if code != 200 || calls.Load() != 1 {
		t.Fatalf("model batch did not run: %d %+v calls=%d", code, response, calls.Load())
	}
	items := assistantBatchItems(t, response, 4)
	for i, state := range []string{"completed", "confirmation_required", "failed", "failed"} {
		if items[i]["state"] != state {
			t.Fatalf("model item %d: %+v", i, items[i])
		}
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("model proposal credential was returned")
	}
	if tasks, err := store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("model batch bypassed confirmation: %+v %v", tasks, err)
	}
	conversation, _ := json.Marshal(map[string]string{
		"text": "/助手 查看任务，并整理记录", "client_request_id": "model-assistant-batch-001",
	})
	code, response = a.post("/api/v1/conversations", string(conversation))
	if code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("conversation did not use the model batch: %d %+v calls=%d", code, response, calls.Load())
	}
	items = assistantBatchItems(t, response, 4)
	if items[0]["state"] != "completed" || items[1]["state"] != "confirmation_required" ||
		items[2]["state"] != "failed" || items[3]["state"] != "failed" {
		t.Fatalf("conversation batch bypassed a gate: %+v", items)
	}
	credentialInput, _ := json.Marshal(map[string]string{"text": "配置 api-key: " + secret})
	code, response = a.post("/api/v1/assistant/interpret", string(credentialInput))
	encoded, _ = json.Marshal(response)
	if code != http.StatusOK || response["state"] != "form_required" || calls.Load() != 2 || strings.Contains(string(encoded), secret) {
		t.Fatalf("credential input did not use a trusted form: status=%d calls=%d", code, calls.Load())
	}

	request := a.request("POST", "/api/v1/assistant/interpret", `{"text":"查看任务，并整理记录"}`)
	request.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
	httpResponse, err := a.ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusBadRequest || calls.Load() != 2 {
		t.Fatalf("deterministic request called the model: status=%d calls=%d", httpResponse.StatusCode, calls.Load())
	}
}
