package httpapi

import (
	"bytes"
	"database/sql"
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

	"clusbot/internal/work"
)

func TestAssistantOperationScopeIsNarrow(t *testing.T) {
	for _, text := range []string{
		"你好", "查看 POD7 的节点", "POD7 的健康是什么意思", "假设 POD7 故障，请给建议",
		"POD7 不需要建议", "给 POD7 处理建议并执行", "/benchmark POD7 给建议",
		"给 POD7 建议，密码=secret123456", "顺便给 POD7 建议", "查看 POD7 的方案详情",
		"先重启 POD7，给出排查建议",
	} {
		if _, ok := parseAssistantOperationScope(text); ok {
			t.Fatalf("unrelated or conditional text intercepted: %q", text)
		}
	}
	for _, text := range []string{
		"帮我看下 POD7 节点刚触发的慢训练告警，并给出恢复建议。",
		"查看 POD7 集群节点状态、所处运维阶段及模型 Token 消耗，并给出排查方案选项。",
		"查看全局 POD 健康并给出排查方案",
	} {
		if _, ok := parseAssistantOperationScope(text); !ok {
			t.Fatalf("operational recommendation not recognized: %q", text)
		}
	}
}

func TestAssistantOperationAccountIsolation(t *testing.T) {
	t.Setenv("CLUSBOT_RECOMMENDATION_MODEL_KEY", "fixture-key")
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		answer := `{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"核实"}]}}`
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer provider.Close()
	instance, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	stores := map[string]*work.Store{}
	var srv Server
	srv = Server{Store: instance, Settings: recommendationSettings(t, instance, provider.URL), Origin: "https://ops.example", Hosts: []string{"ops.example"},
		PersonalAPI: func(owner string) (http.Handler, error) {
			if stores[owner] == nil {
				return nil, fmt.Errorf("missing personal store")
			}
			return srv.PersonalHandler(stores[owner]), nil
		},
	}
	f := &accountFixture{t: t, store: instance, handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
	admin := f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")
	created := f.request("POST", "/api/v1/admin/users", map[string]string{"username": "member", "password": "member password 1234"}, adminCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", created.Code, created.Body)
	}
	var account struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, created, &account)
	member := account.User.PrincipalID
	memberCookie := f.login("member", "member password 1234")
	for actor, node := range map[string]string{admin: "admin-node", member: "member-node"} {
		path := filepath.Join(t.TempDir(), "personal.db")
		store, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.BindAccountBusinessStore(t.Context(), actor); err != nil {
			t.Fatal(err)
		}
		if err := store.ImportDomainAssetsForAccount(t.Context(), actor, []work.DomainAssetEntry{
			{ObjectRef: "pod-7", Kind: "pod", Identity: "POD7", Read: true},
			{ObjectRef: node, Kind: "node", Identity: node, PodRef: "pod-7", Read: true},
		}); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if actor == admin {
			for _, ref := range []string{"pod-7", node} {
				if _, err := db.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at)
					VALUES(?,?,'export_model',?,0,'owner',0,0)`, "export-"+ref, actor, ref); err != nil {
					t.Fatal(err)
				}
			}
		}
		_ = db.Close()
		stores[actor] = store
	}
	request := map[string]string{"text": "查看 POD7 节点并给出排查建议", "client_request_id": "account-operation"}
	operationRequest := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(request)
		r := httptest.NewRequest(http.MethodPost, "https://ops.example/api/v1/conversations", bytes.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Origin", "https://ops.example")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	if rec := operationRequest(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated operation: %d %s", rec.Code, rec.Body)
	}
	if rec := operationRequest(memberCookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "外发授权") || strings.Contains(rec.Body.String(), "admin-node") {
		t.Fatalf("member read another account or exported without grant: %d %s", rec.Code, rec.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("member caused unauthorized model call: %d", calls.Load())
	}
	adminRun := operationRequest(adminCookie)
	if adminRun.Code != 202 {
		t.Fatalf("admin suggestion was not attached to its own turn: %d %s", adminRun.Code, adminRun.Body)
	}
	var origin conversationAdviceOrigin
	accountJSON(t, adminRun, &origin)
	path := "/api/v1/conversations/" + origin.Conversation + "/operation-advice/" + origin.Run
	adminCard := f.request("GET", path, nil, adminCookie)
	if adminCard.Code != 200 || !strings.Contains(adminCard.Body.String(), "核实") || strings.Contains(adminCard.Body.String(), "member-node") {
		t.Fatalf("admin personal scope failed or leaked another account: %d %s", adminCard.Code, adminCard.Body)
	}
	memberCard := f.request("GET", path, nil, memberCookie)
	if memberCard.Code != 404 || strings.Contains(memberCard.Body.String(), "核实") {
		t.Fatalf("member read admin advice: %d %s", memberCard.Code, memberCard.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("admin model calls: %d", calls.Load())
	}
}

func TestAssistantOperationEvidenceAndEgress(t *testing.T) {
	t.Setenv("CLUSBOT_RECOMMENDATION_MODEL_KEY", "fixture-key")
	var mu sync.Mutex
	var payloads []string
	answer := `{"tool":"propose_operation_options","body":{"title":"排查建议","summary":"先核实事件","options":[{"label":"核实健康记录","recommended":true}]}}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("invalid model request: %v", err)
			return
		}
		mu.Lock()
		payloads = append(payloads, string(raw))
		reply := answer
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", reply)
	}))
	defer provider.Close()
	path := filepath.Join(t.TempDir(), "operation.db")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := []work.DomainAssetEntry{
		{ObjectRef: "pod-7", Kind: "pod", Identity: "POD7", Read: true},
		{ObjectRef: "pod-8", Kind: "pod", Identity: "POD8", Read: true},
		{ObjectRef: "node-7-a", Kind: "node", Identity: "seven-a", PodRef: "pod-7", Read: true},
		{ObjectRef: "node-7-b", Kind: "node", Identity: "seven-b", PodRef: "pod-7", Read: true},
		{ObjectRef: "node-7-private", Kind: "node", Identity: "private-node", PodRef: "pod-7"},
		{ObjectRef: "node-8", Kind: "node", Identity: "other-scope-node", PodRef: "pod-8", Read: true},
	}
	if err := store.ImportDomainAssetsForAccount(t.Context(), businessFixturePrincipal(t, store), entries); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stamp := time.Now().Add(-time.Second).UnixMilli()
	for _, item := range []struct{ ref, health string }{{"node-7-a", "healthy"}, {"node-7-b", "degraded"}, {"node-8", "unhealthy"}} {
		if _, err := db.Exec(`INSERT INTO asset_transitions(transition_id,object_ref,from_state,to_state,trigger_kind,expected_revision,expected_ownership_epoch,actor,health,occurred_at,created_at)
			VALUES(?,?,'在役','在役','record',1,1,'operator',?,?,?)`, "transition-"+item.ref, item.ref, item.health, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	actor := businessFixturePrincipal(t, store)

	settings := recommendationSettings(t, store, provider.URL)
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	a := &a1Server{ts: httptest.NewServer(businessFixtureHandler(t, server)), store: store}
	defer a.close()
	a.cookie = a.login()
	call := func(text string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"text": "/助手 " + text, "client_request_id": "operation-fixture"})
		return a.post("/api/v1/conversations", string(body))
	}
	request := "帮我看下 POD7 节点刚触发的慢训练告警，并给出恢复建议。"
	code, out := call(request)
	items := assistantBatchItems(t, out, 3)
	if code != 200 || items[0]["tool"] != "get_operation_evidence" || items[1]["state"] != "failed" || !strings.Contains(fmt.Sprint(items[1]), "外发授权") || items[2]["tool"] != "get_incident_workflow" {
		t.Fatalf("missing export grant did not keep local facts: %d %+v", code, out)
	}
	evidence := items[0]["result"].(map[string]any)
	health := evidence["health"].(map[string]any)
	if evidence["total_nodes"] != float64(2) || health["healthy"] != float64(1) || health["degraded"] != float64(1) || strings.Contains(fmt.Sprint(items[0]), "private-node") {
		t.Fatalf("wrong scoped authorized evidence: %+v", evidence)
	}
	grant := func(ref string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at)
			VALUES(?,?,'export_model',?,0,'owner',0,0)`, "export-"+ref, actor, ref); err != nil {
			t.Fatal(err)
		}
	}
	grant("pod-7")
	grant("node-7-a")
	if code, out = call(request); code != 200 || !strings.Contains(fmt.Sprint(out), "外发授权") {
		t.Fatalf("partial grants allowed aggregate export: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != 0 {
		t.Fatalf("missing grant reached model: %v", payloads)
	}
	mu.Unlock()
	grant("node-7-b")
	code, out = call(request)
	items = assistantBatchItems(t, out, 3)
	if code != 200 {
		t.Fatalf("authorized recommendation: %d %+v", code, out)
	}
	id, options := recommendationResult(t, items[1])
	mu.Lock()
	if len(payloads) != 1 {
		t.Fatalf("expected one model call, got %d", len(payloads))
	}
	modelPayload := payloads[0]
	mu.Unlock()
	var modelRequest struct {
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(modelPayload), &modelRequest); err != nil || len(modelRequest.Input) != 1 || len(modelRequest.Tools) != 0 {
		t.Fatalf("unexpected model envelope: %s %v", modelPayload, err)
	}
	var sent struct {
		Evidence struct {
			Scope, Focus string
			TotalNodes   int `json:"total_nodes"`
			Health       work.ClusterHealthCounts
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(modelRequest.Input[0].Content), &sent); err != nil || sent.Evidence.Scope != "pod" || sent.Evidence.TotalNodes != 2 || sent.Evidence.Health.Healthy != 1 || sent.Evidence.Health.Degraded != 1 || sent.Evidence.Health.Unhealthy != 0 {
		t.Fatalf("model did not receive exact computed POD facts: %s %v", modelRequest.Input[0].Content, err)
	}
	for _, forbidden := range []string{"POD7", "POD8", "node-7", "node-8", "private-node", "other-scope-node", request} {
		if strings.Contains(modelPayload, forbidden) {
			t.Fatalf("unnecessary or out-of-scope text in model payload: %q", forbidden)
		}
	}
	if code, receipt := a.post("/api/v1/assistant/recommendations/"+id+"/selection", `{"option_id":"`+options[0]["id"].(string)+`"}`); code != 200 || receipt["execution"] != "not_evaluated" {
		t.Fatalf("selection did not stay separate from execution: %d %+v", code, receipt)
	}
	if tasks, err := store.ListTasks(t.Context(), work.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("recommendation or selection created a task: %+v %v", tasks, err)
	}
	ordinary := func(text, key string, deterministic bool) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"text": text, "client_request_id": key})
		req := a.request(http.MethodPost, "/api/v1/conversations", string(body))
		req.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if deterministic {
			req.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
		}
		response, err := a.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, result
	}
	mu.Lock()
	beforeOrdinary := len(payloads)
	mu.Unlock()
	code, out = ordinary("查看 POD7 集群节点状态、所处运维阶段及模型 Token 消耗，并给出排查方案选项。", "operation-ordinary", false)
	items = assistantBatchItems(t, out, 5)
	if code != 200 || items[0]["tool"] != "get_operation_evidence" || items[1]["tool"] != assistantRecommendationTool ||
		items[2]["tool"] != "get_cluster_inventory" || items[3]["tool"] != "get_token_usage_stats" || items[4]["tool"] != "get_incident_workflow" {
		t.Fatalf("ordinary operational chat lost its independent evidence cards: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != beforeOrdinary+1 {
		t.Fatalf("ordinary operational chat made extra model calls: %d", len(payloads)-beforeOrdinary)
	}
	beforeDeterministic := len(payloads)
	mu.Unlock()
	if code, out = ordinary(request, "operation-deterministic", true); code != http.StatusBadRequest {
		t.Fatalf("deterministic query unexpectedly generated a suggestion: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != beforeDeterministic {
		t.Fatal("deterministic query reached model")
	}
	answer = `{"kind":"chat"}`
	beforeUnrelated := len(payloads)
	mu.Unlock()
	code, out = ordinary("你好", "ordinary-unrelated", false)
	if code != http.StatusAccepted || out["state"] == "batch_completed" {
		t.Fatalf("unrelated conversation was intercepted: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != beforeUnrelated+1 {
		t.Fatalf("unrelated conversation made extra model calls: %d", len(payloads)-beforeUnrelated)
	}
	beforeLegacy := len(payloads)
	mu.Unlock()
	legacyBody, _ := json.Marshal(map[string]string{"text": request, "client_request_id": "operation-legacy"})
	if code, out = a.post("/api/v1/conversations", string(legacyBody)); code != http.StatusAccepted || out["state"] == "batch_completed" {
		t.Fatalf("client without card support received cards: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != beforeLegacy+1 {
		t.Fatalf("legacy conversation made extra model calls: %d", len(payloads)-beforeLegacy)
	}
	answer = `{"tool":"propose_operation_options","body":{"title":"排查建议","options":[{"label":"核实"}]}}`
	mu.Unlock()
	mu.Lock()
	answer = `{"calls":[{"tool":"request_ticket"},{"tool":"propose_operation_options","body":{"title":"建议","options":[{"label":"执行"}]}}]}`
	mu.Unlock()
	if code, out = call(request); code != 200 || !strings.Contains(fmt.Sprint(out), "未返回有效") {
		t.Fatalf("model injected an additional tool: %d %+v", code, out)
	}
	mu.Lock()
	answer = `{"tool":"propose_operation_options","body":{"title":"排查建议","options":[{"label":"核实"}]}}`
	beforeGlobal := len(payloads)
	mu.Unlock()
	global := "查看全局 POD 健康并给出排查方案"
	if code, out = call(global); code != 200 || !strings.Contains(fmt.Sprint(out), "外发授权") {
		t.Fatalf("partial global grant allowed export: %d %+v", code, out)
	}
	mu.Lock()
	if len(payloads) != beforeGlobal {
		t.Fatal("partially authorized global aggregate reached model")
	}
	mu.Unlock()
	grant("pod-8")
	grant("node-8")
	code, out = call(global)
	items = assistantBatchItems(t, out, 3)
	if code != 200 || items[2]["tool"] != "get_cluster_inventory" || items[0]["result"].(map[string]any)["total_nodes"] != float64(3) {
		t.Fatalf("authorized global aggregate: %d %+v", code, out)
	}
	mu.Lock()
	globalPayload := payloads[len(payloads)-1]
	mu.Unlock()
	if !strings.Contains(globalPayload, `\"total_nodes\":3`) || strings.Contains(globalPayload, "private-node") || strings.Contains(globalPayload, "other-scope-node") {
		t.Fatalf("unexpected global model payload: %s", globalPayload)
	}
	if code, _ := call("查看 POD70 节点并给出排查建议"); code != http.StatusNotFound {
		t.Fatalf("an inexact POD identifier selected another scope: %d", code)
	}
	if code, _ := call("查看 POD7 和 POD8 节点并给出排查建议"); code != http.StatusBadRequest {
		t.Fatalf("ambiguous PODs selected a scope: %d", code)
	}
	if code, _ := call("查看 POD7 中 node-7-a 的状态并给出建议"); code != http.StatusBadRequest {
		t.Fatalf("node-specific request broadened to the whole POD: %d", code)
	}
	task, err := store.Submit(t.Context(), "context-seed", "仅本地可见的任务原文 /private/task-file", "POD7", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"text": "查看该任务 POD7 节点并给出建议", "client_request_id": "operation-task-context"})
	req := a.request(http.MethodPost, "/api/v1/conversations/"+task.ConversationID+"/messages", string(body))
	req.Header.Set("X-ClusBot-Assistant-Cards", "1")
	response, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var taskOut map[string]any
	if err := json.NewDecoder(response.Body).Decode(&taskOut); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	taskItems := assistantBatchItems(t, taskOut, 3)
	if response.StatusCode != 200 || taskItems[2]["tool"] != "get_task" || taskItems[2]["state"] != "completed" {
		t.Fatalf("matching conversation task was not shown locally: %d %+v", response.StatusCode, taskOut)
	}
	mu.Lock()
	taskPayload := payloads[len(payloads)-1]
	mu.Unlock()
	if strings.Contains(taskPayload, "/private/task-file") || strings.Contains(taskPayload, "仅本地可见") || strings.Contains(taskPayload, task.TaskID) {
		t.Fatalf("task context leaked into recommendation payload: %s", taskPayload)
	}
}
