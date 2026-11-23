package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func recommendationModelFixture(t *testing.T, answer *atomic.Value, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
			!strings.Contains(input.Instructions, assistantRecommendationTool) || !strings.Contains(string(input.Input), assistantRecommendationTool) {
			t.Errorf("model did not receive the recommendation catalog and instructions: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer.Load().(string))
	}))
}

func recommendationSettings(t *testing.T, store *work.Store, providerURL string) *model.Settings {
	t.Helper()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(t.Context(), work.ModelSettings{BaseURL: providerURL, ModelID: "fixture", Protocol: model.ProtocolResponses, KeyRef: "env:CLUSBOT_RECOMMENDATION_MODEL_KEY"}); err != nil {
		t.Fatal(err)
	}
	return settings
}

func recommendationResult(t *testing.T, response map[string]any) (string, []map[string]any) {
	t.Helper()
	if response["state"] != "completed" || response["tool"] != assistantRecommendationTool {
		t.Fatalf("not a recommendation: %+v", response)
	}
	result, ok := response["result"].(map[string]any)
	if !ok || result["origin"] != "model" || result["execution"] != "not_evaluated" || result["title"] == "" || result["expires_at_unix_ms"] == nil {
		t.Fatalf("invalid recommendation result: %+v", response)
	}
	id, ok := result["proposal_id"].(string)
	if !ok || len(id) != 48 || !assistantLowerHex(id) {
		t.Fatalf("invalid proposal ID: %+v", result)
	}
	values, ok := result["options"].([]any)
	if !ok || len(values) < 1 || len(values) > 4 {
		t.Fatalf("invalid options: %+v", result)
	}
	options := make([]map[string]any, len(values))
	for i, v := range values {
		options[i], ok = v.(map[string]any)
		if !ok {
			t.Fatalf("option is not an object: %+v", v)
		}
		optionID, ok := options[i]["id"].(string)
		if !ok || len(optionID) != 32 || !assistantLowerHex(optionID) || options[i]["label"] == "" {
			t.Fatalf("invalid option: %+v", options[i])
		}
	}
	return id, options
}

func TestAssistantRecommendationModelAndSelection(t *testing.T) {
	var answer atomic.Value
	var calls atomic.Int32
	answer.Store(`{"tool":"propose_operation_options","body":{"title":"处理建议","summary":"需要核实能力","options":[{"label":"7 卡降跑","detail":"先核实资源","recommended":true},{"label":"等待处理"}]}}`)
	provider := recommendationModelFixture(t, &answer, &calls)
	defer provider.Close()
	store, err := work.Open(filepath.Join(t.TempDir(), "recommendations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: recommendationSettings(t, store, provider.URL), Token: "a1-token", Hosts: []string{"127.0.0.1"}}
	a := &a1Server{ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})), store: store}
	defer a.close()
	a.cookie = a.login()
	command, _ := json.Marshal(map[string]string{"text": "/助手 提出处理建议", "client_request_id": "recommendation-001"})
	code, response := a.post("/api/v1/conversations", string(command))
	if code != 200 || calls.Load() != 1 {
		t.Fatalf("model proposal: status=%d calls=%d response=%+v", code, calls.Load(), response)
	}
	id, options := recommendationResult(t, response)
	if len(options) != 2 || options[0]["label"] != "7 卡降跑" || options[0]["recommended"] != true {
		t.Fatalf("model text was not retained: %+v", options)
	}
	selectionPath := "/api/v1/assistant/recommendations/" + id + "/selection"
	first := options[0]["id"].(string)
	second := options[1]["id"].(string)
	for _, body := range []string{
		`{}`, `{"option_id":null}`, `{"option_id":"short"}`, `{"option_id":"` + first + `","target":"anything"}`,
		`{"option_id":"` + first + `","option_id":"` + second + `"}`,
	} {
		if code, _ := a.post(selectionPath, body); code != http.StatusBadRequest {
			t.Fatalf("accepted invalid selection %s: %d", body, code)
		}
	}
	if code, _ := a.post(selectionPath, `{"option_id":"`+strings.Repeat("a", 32)+`"}`); code != http.StatusConflict {
		t.Fatalf("accepted forged option: %d", code)
	}
	if code, _ := a.post("/api/v1/assistant/recommendations/"+strings.Repeat("a", 48)+"/selection", `{"option_id":"`+first+`"}`); code != http.StatusConflict {
		t.Fatalf("accepted forged proposal: %d", code)
	}
	for i := 0; i < 2; i++ {
		code, receipt := a.post(selectionPath, `{"option_id":"`+first+`"}`)
		if code != 200 || len(receipt) != 4 || receipt["proposal_id"] != id || receipt["option_id"] != first || receipt["state"] != "confirmed" || receipt["execution"] != "not_evaluated" {
			t.Fatalf("selection or idempotent retry: %d %+v", code, receipt)
		}
	}
	if code, _ := a.post(selectionPath, `{"option_id":"`+second+`"}`); code != http.StatusConflict {
		t.Fatalf("accepted a conflicting selection: %d", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("selection invoked the provider: %d", calls.Load())
	}
	if tasks, err := store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("proposal or selection created a task: %+v %v", tasks, err)
	}

	answer.Store(`{"calls":[{"tool":"get_cluster_inventory"},{"tool":"get_cluster_trends"},{"tool":"propose_operation_options","body":{"title":"重置建议","options":[{"label":"批量重置"}]}}]}`)
	code, response = a.post("/api/v1/assistant/interpret", `{"text":"查看集群和趋势，并提出重置建议"}`)
	if code != 200 || calls.Load() != 2 {
		t.Fatalf("model batch: %d %+v calls=%d", code, response, calls.Load())
	}
	items := assistantBatchItems(t, response, 3)
	if items[0]["state"] != "completed" || items[0]["tool"] != "get_cluster_inventory" || items[1]["state"] != "completed" || items[1]["tool"] != "get_cluster_trends" {
		t.Fatalf("local reads did not complete independently: %+v", items)
	}
	_, single := recommendationResult(t, items[2])
	if len(single) != 1 || single[0]["label"] != "批量重置" {
		t.Fatalf("single recommendation not supported: %+v", single)
	}
	if code, _ := a.post("/api/v1/assistant/tools", `{"tool":"propose_operation_options","body":{"title":"伪造","options":[{"label":"伪造"}]}}`); code < 400 {
		t.Fatalf("direct tool call forged a model proposal: %d", code)
	}
	code, directBatch := a.post("/api/v1/assistant/tools", `{"calls":[{"tool":"get_cluster_inventory"},{"tool":"propose_operation_options","body":{"title":"伪造","options":[{"label":"伪造"}]}}]}`)
	if code != 200 || assistantBatchItems(t, directBatch, 2)[1]["state"] != "failed" {
		t.Fatalf("direct batch forged a model proposal: %d %+v", code, directBatch)
	}
	answer.Store(`{"calls":[{"tool":"get_cluster_trends"},{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择","target":"伪造目标"}]}}]}`)
	code, response = a.post("/api/v1/assistant/interpret", `{"text":"查看趋势并提出建议"}`)
	if code != 200 || calls.Load() != 3 {
		t.Fatalf("invalid model batch: %d %+v calls=%d", code, response, calls.Load())
	}
	invalidItems := assistantBatchItems(t, response, 2)
	if invalidItems[0]["state"] != "completed" || invalidItems[1]["state"] != "failed" || strings.Contains(fmt.Sprint(invalidItems[1]), "伪造目标") {
		t.Fatalf("invalid model fields were accepted or returned: %+v", invalidItems)
	}
	request := a.request("POST", "/api/v1/assistant/interpret", `{"text":"提出处理建议"}`)
	request.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
	httpResponse, err := a.ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusBadRequest || calls.Load() != 3 {
		t.Fatalf("deterministic request called the model: %d calls=%d", httpResponse.StatusCode, calls.Load())
	}
}

func TestAssistantRecommendationRejectsUntrustedModelFields(t *testing.T) {
	valid := `{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择"}]}}`
	if _, ok := decodeAssistantRecommendation(json.RawMessage(valid)); !ok {
		t.Fatal("rejected a valid single option")
	}
	for _, raw := range []string{
		`{"tool":"propose_operation_options","params":{},"body":{"title":"建议","options":[{"label":"选择"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择","id":"forged"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择","target":"node"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择","script":"run"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择","recommended":null}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","summary":null,"options":[{"label":"选择"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","title":"伪造","options":[{"label":"选择"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"1","recommended":true},{"label":"2","recommended":true}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"选择\n执行"}]}}`,
		`{"tool":"propose_operation_options","body":{"title":"` + strings.Repeat("长", 81) + `","options":[{"label":"选择"}]}}`,
	} {
		if _, ok := decodeAssistantRecommendation(json.RawMessage(raw)); ok {
			t.Fatalf("accepted untrusted model fields: %s", raw)
		}
	}
}

func TestAssistantRecommendationBoundedAtomicSelection(t *testing.T) {
	bridge := &assistantBridge{}
	draft := assistantRecommendationDraft{title: "建议", options: []assistantRecommendationOption{{Label: "一"}, {Label: "二"}}}
	result, ok := bridge.prepareRecommendation("session", draft)
	if !ok {
		t.Fatal("failed to prepare recommendation")
	}
	if bridge.selectRecommendation(result.ProposalID, result.Options[0].ID, "other-session") {
		t.Fatal("selection crossed sessions")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, option := range result.Options {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if bridge.selectRecommendation(result.ProposalID, id, "session") {
				successes.Add(1)
			}
		}(option.ID)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("different concurrent selections succeeded: %d", successes.Load())
	}
	item := bridge.recommendations[result.ProposalID]
	item.expires = time.Now().Add(-time.Second)
	bridge.recommendations[result.ProposalID] = item
	if bridge.selectRecommendation(result.ProposalID, item.selected, "session") {
		t.Fatal("expired proposal accepted an idempotent retry")
	}
	for i := 0; i < 128; i++ {
		if _, ok := bridge.prepareRecommendation("session", draft); !ok {
			t.Fatalf("bounded storage rejected item %d", i)
		}
	}
	if _, ok := bridge.prepareRecommendation("session", draft); ok {
		t.Fatal("unbounded recommendation storage")
	}
}

func TestAssistantRecommendationAccountSession(t *testing.T) {
	var answer atomic.Value
	var calls atomic.Int32
	answer.Store(`{"tool":"propose_operation_options","body":{"title":"处理建议","options":[{"label":"建议"}]}}`)
	provider := recommendationModelFixture(t, &answer, &calls)
	defer provider.Close()
	store, err := work.Open(filepath.Join(t.TempDir(), "account-recommendations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := Server{Store: store, Settings: recommendationSettings(t, store, provider.URL), ServerMode: true, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	f := &accountFixture{t: t, store: store, handler: server.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	if rec := f.request("POST", "/api/v1/admin/users", map[string]any{"username": "member", "password": "member password 123"}, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", rec.Code, rec.Body)
	}
	first := f.login("member", "member password 123")
	second := f.login("member", "member password 123")
	input := map[string]string{"text": "提出处理建议"}
	if rec := f.request("POST", "/api/v1/assistant/interpret", input, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proposal: %d", rec.Code)
	}
	rec := f.request("POST", "/api/v1/assistant/interpret", input, first)
	var response map[string]any
	accountJSON(t, rec, &response)
	if rec.Code != http.StatusOK {
		t.Fatalf("member proposal: %d %+v", rec.Code, response)
	}
	id, options := recommendationResult(t, response)
	path := "/api/v1/assistant/recommendations/" + id + "/selection"
	selection := map[string]any{"option_id": options[0]["id"]}
	if rec := f.request("POST", path, selection, second); rec.Code != http.StatusConflict {
		t.Fatalf("selection crossed account sessions: %d %s", rec.Code, rec.Body)
	}
	if rec := f.request("POST", path, selection, first); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"execution":"not_evaluated"`) {
		t.Fatalf("member selection: %d %s", rec.Code, rec.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("selection called model: %d", calls.Load())
	}
}
