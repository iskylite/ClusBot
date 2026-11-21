package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestServerAccountRuntimeIsolation(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(dir, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	const origin = "https://ops.example"
	api := httpapi.Server{Store: instance, ServerMode: true, Origin: origin, Hosts: []string{"ops.example"}}
	var cfg config
	cfg.DataDir = dir
	cfg.Model.Provider = model.ProtocolChat
	cfg.Model.BaseURL = "https://api.openai.com/v1"
	cfg.Model.KeyRef = "env:CLUSBOT_TEST_UNUSED_KEY"
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/discover" {
			t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"source":"fixture","scope":"lab","observations":[{"serial":"SERVER-TEST-NODE","address":"192.0.2.5"}]}`)
	}))
	t.Cleanup(discovery.Close)
	cfg.Discovery.BaseURL = discovery.URL
	cfg.Discovery.Source = "fixture"
	cfg.Discovery.Scopes = []string{"lab"}
	cfg.Discovery.CredentialEnv = "CLUSBOT_TEST_DISCOVERY_KEY"
	t.Setenv(cfg.Discovery.CredentialEnv, "fixture-token")

	cfg.Runtime.MaxActiveRuns = 1
	cfg.Runtime.MaxRunSeconds = 10
	cfg.Runtime.ModelTimeoutSeconds = 5
	cfg.Runtime.MaxOutputTokens = 256
	cfg.Runtime.MaxModelRequestBytes = model.DefaultMaxRequestBytes
	t.Setenv("CLUSBOT_MODEL_BASE_URL", "")
	t.Setenv("CLUSBOT_MODEL_ID", "")
	assets := fstest.MapFS{"index.html": {Data: []byte("unused")}}
	stop, err := startAccountRuntime(context.Background(), &api, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stop()
		}
	})
	api.Settings.Demo = true // No external model or operational services in this test.
	handler := api.Handler(assets)
	request := func(method, path, body string, cookie *http.Cookie, code int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, origin+"/api/v1"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, code, w.Body)
		}
		return w
	}
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	request("POST", "/bootstrap", `{"username":"admin","password":"admin-password"}`, nil, 201)
	admin := request("POST", "/session", `{"username":"admin","password":"admin-password"}`, nil, 200).Result().Cookies()[0]
	request("POST", "/admin/users", `{"username":"member","password":"member-password"}`, admin, 201)
	member := request("POST", "/session", `{"username":"member","password":"member-password"}`, nil, 200).Result().Cookies()[0]
	const policies = "/settings/model/routing/policies"
	request("GET", policies, "", admin, 200)
	request("GET", policies, "", member, 403)
	request("PUT", policies+"/conversation/conversation_reply", `{"revision":0,"candidates":[]}`, member, 403)
	request("GET", "/assistant/model-routing", "", member, 200)
	for _, query := range []struct {
		text   string
		status int
	}{
		{"查看我的任务", 200},
		{"/assistant 未匹配的自然语言查询", 400},
		{`/assistant {"tool":"create_task","body":{"text":"不可创建"}}`, 404},
	} {
		payload, err := json.Marshal(map[string]string{"text": query.text, "client_request_id": "deterministic-query"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", origin+"/api/v1/conversations", bytes.NewReader(payload))
		req.AddCookie(member)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != query.status {
			t.Fatalf("deterministic_only query %q = %d, want %d: %s", query.text, out.Code, query.status, out.Body)
		}
	}

	grant := fmt.Sprintf(`{"source":"fixture","scope":"lab","expires_at_unix_ms":%d}`, time.Now().Add(time.Hour).UnixMilli())
	request("POST", "/domain/discovery/authorizations", grant, member, 403)
	request("POST", "/domain/discovery/authorizations", grant, admin, 201)
	plan := fmt.Sprintf(`{"id":"private-discovery","source":"fixture","scope":"lab","period_seconds":60,"first_at_unix_ms":%d}`, time.Now().Add(time.Second).UnixMilli())
	request("POST", "/domain/discovery/plans", plan, admin, 201)
	if out := decode(request("GET", "/domain/discovery/setup-status", "", member, 200)); out["state"] != "source_unavailable" {
		t.Fatalf("member saw a privileged discovery source: %v", out)
	}
	var candidates []work.DiscoveryCandidate
	discoveryDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(discoveryDeadline) {
		rec := request("GET", "/domain/discovery/candidates", "", admin, 200)
		if err := json.Unmarshal(rec.Body.Bytes(), &candidates); err != nil {
			t.Fatal(err)
		}
		if len(candidates) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(candidates) != 1 {
		t.Fatalf("discovery worker did not record fixture candidate: %+v", candidates)
	}
	candidate := candidates[0]
	admit, err := json.Marshal(work.DiscoveryAdmission{
		Candidate: candidate.ID, ConfirmedSerial: candidate.Serial, ExpectedSeenAt: candidate.SeenAt,
		Owner: "fixture owner", Location: "lab", Role: "compute",
	})
	if err != nil {
		t.Fatal(err)
	}
	resultAsset := decode(request("POST", "/domain/discovery/candidates/"+candidate.ID+"/admit", string(admit), admin, 200))
	if resultAsset["asset"] == nil || resultAsset["asset"] == "" {
		t.Fatalf("admission omitted asset: %+v", resultAsset)
	}
	asset := resultAsset["asset"].(string)
	if snapshot := request("GET", "/domain/snapshot", "", member, 200).Body.String(); strings.Contains(snapshot, asset) {
		t.Fatalf("member saw admin's asset: %s", snapshot)
	}
	query := fmt.Sprintf(`{"client_request_id":"server-asset-query","object_ref":%q}`, asset)
	request("POST", "/domain/asset-acceptance", query, admin, 202)

	create := func(cookie *http.Cookie, text string) map[string]any {
		return decode(request("POST", "/tasks", `{"client_request_id":"same-request-123","text":"`+text+`"}`, cookie, 202))
	}
	a := create(admin, "private-admin-report")
	b := create(member, "private-member-report")
	taskA, taskB := a["task_id"].(string), b["task_id"].(string)
	runA, runB := a["run_id"].(string), b["run_id"].(string)
	convA := a["conversation_id"].(string)
	if taskA == "" || taskB == "" || taskA == taskB || runA == runB {
		t.Fatalf("duplicate entity identity: %v %v", a, b)
	}
	if replay := create(admin, "private-admin-report"); replay["run_id"] != runA {
		t.Fatalf("request retry created another run: %v", replay)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		ownID  string
		other  string
	}{
		{admin, taskA, taskB},
		{member, taskB, taskA},
	} {
		list := request("GET", "/tasks", "", tc.cookie, 200).Body.String()
		if !strings.Contains(list, tc.ownID) || strings.Contains(list, tc.other) {
			t.Fatalf("incorrect task list: %s", list)
		}
		request("GET", "/tasks/"+tc.other, "", tc.cookie, 404)
		request("POST", "/tasks/"+tc.other+"/cancel", `{"revision":1}`, tc.cookie, 404)
	}
	request("GET", "/runs/"+runA+"/events", "", member, 404)
	request("GET", "/conversations/"+convA+"/history", "", member, 404)
	request("GET", "/conversations/"+convA+"/events", "", member, 404)

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "private.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "secret material"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", origin+"/api/v1/attachments", &upload)
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.AddCookie(admin)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("attachment upload = %d: %s", w.Code, w.Body)
	}
	attachment := decode(w)["id"].(string)
	if attachment == "" {
		t.Fatal("missing attachment identifier")
	}
	// A captured file ID belongs to its owner's store even when the same
	// request and attachment shape are accepted from both accounts.
	r = httptest.NewRequest("POST", origin+"/api/v1/tasks", strings.NewReader(`{"client_request_id":"attachment-check","text":"read","attachment_ids":["`+attachment+`"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	r.AddCookie(member)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code < 400 || strings.Contains(w.Body.String(), "secret material") {
		t.Fatalf("cross-account attachment accepted: %d %s", w.Code, w.Body)
	}

	waitForArtifact := func(task string, cookie *http.Cookie) string {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for {
			body := request("GET", "/artifacts?task_id="+task, "", cookie, 200).Body.String()
			var artifacts []struct{ ID string }
			if err := json.Unmarshal([]byte(body), &artifacts); err != nil {
				t.Fatal(err)
			}
			if len(artifacts) > 0 {
				return "/artifacts/" + artifacts[0].ID + "/content"
			}
			if time.Now().After(deadline) {
				t.Fatalf("account worker did not create an artifact: %s", request("GET", "/tasks/"+task, "", cookie, 200).Body)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	artifactA := waitForArtifact(taskA, admin)
	artifactB := waitForArtifact(taskB, member)
	request("GET", artifactA, "", admin, 200)
	request("GET", artifactB, "", member, 200)
	request("GET", artifactA, "", member, 404)
	request("GET", artifactB, "", admin, 404)
	owner := decode(request("GET", "/session", "", member, 200))["user"].(map[string]any)["principal_id"].(string)
	const adminTasks = "/admin/tasks"
	request("GET", adminTasks, "", nil, 401)
	request("GET", adminTasks, "", member, 403)
	first := decode(request("GET", adminTasks+"?limit=1", "", admin, 200))
	next := first["next_cursor"].(string)
	second := decode(request("GET", adminTasks+"?limit=1&after="+url.QueryEscape(next), "", admin, 200))
	idOf := func(page map[string]any) string {
		return page["items"].([]any)[0].(map[string]any)["id"].(string)
	}
	pages := map[string]bool{idOf(first): true, idOf(second): true}
	if !pages[taskA] || !pages[taskB] {
		t.Fatalf("admin pagination did not include both tasks: %v", pages)
	}
	path := adminTasks + "/" + owner + "/" + taskB
	detail := decode(request("GET", path, "", admin, 200))
	if detail["owner_id"] != owner || detail["conversation_id"] != nil {
		t.Fatalf("incorrect admin task projection: %+v", detail)
	}
	runs := request("GET", path+"/runs", "", admin, 200).Body.String()
	if !strings.Contains(runs, runB) || strings.Contains(runs, "private-member-report") {
		t.Fatalf("admin run projection leaked private input or omitted run: %s", runs)
	}
	request("GET", path+"/runs", "", member, 403)
	request("POST", "/tasks/"+taskB+"/cancel", `{"revision":1}`, admin, 404)
	request("GET", path+artifactB, "", admin, 200)
	request("GET", path+artifactA, "", admin, 404)
	request("GET", "/conversations/"+b["conversation_id"].(string)+"/history", "", admin, 404)
	request("GET", "/conversations/"+convA+"/events", "", admin, 200)

	revision := int(detail["revision"].(float64))
	mutation := fmt.Sprintf(`{"revision":%d,"title":"由超管更新"}`, revision)
	request("PATCH", path, fmt.Sprintf(`{"revision":%d,"goal":"changed input"}`, revision), admin, 403)
	request("PATCH", path, mutation, member, 403)
	edited := decode(request("PATCH", path, mutation, admin, 200))
	if edited["title"] != "由超管更新" || edited["conversation_id"] != nil {
		t.Fatalf("incorrect admin edit projection: %+v", edited)
	}
	request("PATCH", path, mutation, admin, 409)
	cancelBody := fmt.Sprintf(`{"revision":%d}`, int(edited["revision"].(float64)))
	request("POST", path+"/cancel", cancelBody, member, 403)
	toolBody := fmt.Sprintf(`{"tool":"admin_cancel_task","params":{"owner":%q,"id":%q},"body":%s}`, owner, taskB, cancelBody)
	request("POST", "/assistant/tools", toolBody, member, 404)
	preview := decode(request("POST", "/assistant/tools", toolBody, admin, 200))
	target, ok := preview["target"].(map[string]any)
	if preview["state"] != "confirmation_required" || !ok || target["owner_id"] != owner || target["id"] != taskB || target["conversation_id"] != nil {
		t.Fatalf("unexpected cross-account preview: %+v", preview)
	}
	confirm := fmt.Sprintf(`{"confirmation_id":%q}`, preview["confirmation_id"])
	result := decode(request("POST", "/assistant/confirm", confirm, admin, 200))
	cancelled := result["result"].(map[string]any)
	if cancelled["status"] != "cancelled" || cancelled["conversation_id"] != nil || cancelled["messages"] != nil {
		t.Fatalf("incorrect admin cancellation projection: %+v", cancelled)
	}
	resumeBody := fmt.Sprintf(`{"revision":%d,"client_request_id":"same-request-123"}`, int(cancelled["revision"].(float64)))
	resumed := decode(request("POST", path+"/resume", resumeBody, admin, 202))
	if resumed["task_id"] != taskB || resumed["run_id"] == runB || resumed["owner_id"] != owner || resumed["conversation_id"] != nil {
		t.Fatalf("incorrect admin resume projection: %+v", resumed)
	}
	replayed := decode(request("POST", path+"/resume", resumeBody, admin, 202))
	if replayed["run_id"] != resumed["run_id"] {
		t.Fatalf("admin replay created another run: %+v", replayed)
	}
	request("GET", "/runs/"+resumed["run_id"].(string), "", member, 200)
	request("GET", "/runs/"+resumed["run_id"].(string), "", admin, 404)
	deadline := time.Now().Add(8 * time.Second)
	for {
		content := request("GET", path+"/artifacts", "", admin, 200)
		var results []struct {
			ID    string `json:"id"`
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(content.Body.Bytes(), &results); err != nil {
			t.Fatal(err)
		}
		if len(results) > 0 && results[0].RunID == resumed["run_id"] {
			current := decode(request("GET", path, "", admin, 200))
			body := fmt.Sprintf(`{"revision":%d,"artifact_id":%q}`, int(current["revision"].(float64)), results[0].ID)
			accepted := decode(request("POST", path+"/acceptance", body, admin, 200))
			if accepted["status"] != "completed" || accepted["messages"] != nil {
				t.Fatalf("incorrect admin acceptance projection: %+v", accepted)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admin resumed task did not generate a result")
		}
		time.Sleep(50 * time.Millisecond)
	}

	stop()
	stopped = true
	freshAPI := httpapi.Server{Store: instance, ServerMode: true, Origin: origin, Hosts: []string{"ops.example"}}
	closeRestarted, err := startAccountRuntime(context.Background(), &freshAPI, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeRestarted != nil {
			closeRestarted()
		}
	})
	handler = freshAPI.Handler(assets)
	request("GET", "/tasks/"+taskA, "", admin, 200)
	request("GET", "/tasks/"+taskB, "", member, 200)
	request("GET", "/tasks/"+taskA, "", member, 404)

	store := freshAPI.AdminTaskStores()[owner]
	queued, err := store.Submit(context.Background(), "fixture-pause-task", "task to pause", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	pausePath := adminTasks + "/" + owner + "/" + queued.TaskID
	current := decode(request("GET", pausePath, "", admin, 200))
	pauseBody := fmt.Sprintf(`{"revision":%d}`, int(current["revision"].(float64)))
	request("POST", pausePath+"/pause", pauseBody, member, 403)
	paused := decode(request("POST", pausePath+"/pause", pauseBody, admin, 200))
	if paused["status_reason"] != "paused" {
		t.Fatalf("admin pause did not update the task: %+v", paused)
	}

	request("POST", "/conversations/"+convA+"/messages", `{"client_request_id":"foreign-message","text":"wrong account"}`, member, 404)
	if receipts := decode(request("POST", "/tasks", `{"client_request_id":"restart-pending-123","text":"member-pending-fixture"}`, member, 202)); receipts["model_pending"] != true {
		t.Fatalf("expected durable pending admission: %+v", receipts)
	}
	request("GET", "/admissions/restart-pending-123", "", admin, 404)
	closeRestarted()
	closeRestarted = nil
	modelInputs := make(chan string, 16)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		modelInputs <- string(input)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture result\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer modelServer.Close()
	recovered := httpapi.Server{Store: instance, ServerMode: true, Origin: origin, Hosts: []string{"ops.example"}}
	// Instance configuration is persisted, so the replacement instance points at
	// the fixture via the same override used by normal startup.
	t.Setenv("CLUSBOT_MODEL_BASE_URL", modelServer.URL)
	t.Setenv("CLUSBOT_MODEL_ID", "local-fixture")
	t.Setenv("CLUSBOT_TEST_UNUSED_KEY", "fixture-key")
	finish, err := startAccountRuntime(context.Background(), &recovered, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	handler = recovered.Handler(assets)
	select {
	case modelInput := <-modelInputs:
		if !strings.Contains(modelInput, "member-pending-fixture") || strings.Contains(modelInput, "private-admin-report") {
			t.Fatalf("recovered model input crossed accounts: %s", modelInput)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("queued work did not resume after restart")
	}
	receipt := decode(request("GET", "/admissions/restart-pending-123", "", member, 200))
	if receipt["task_id"] == "" || receipt["task_id"] == nil {
		t.Fatalf("recovered admission did not link to a task: %+v", receipt)
	}
	recoveredTask := receipt["task_id"].(string)
	request("GET", "/tasks/"+recoveredTask, "", admin, 404)
	waitForArtifact(recoveredTask, member)

}
