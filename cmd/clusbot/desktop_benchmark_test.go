package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestDesktopAccountBenchmarkRevisionUsesPinnedProvider(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(dir, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	var providerCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { providerCalls.Add(1) }))
	defer providerServer.Close()
	t.Setenv("CLUSBOT_DESKTOP_BENCHMARK_TEST_KEY", "fixture-key")
	var cfg config
	cfg.DataDir = dir
	cfg.Model.Provider = model.ProtocolChat
	cfg.Model.BaseURL = "https://api.openai.com/v1"
	cfg.Model.KeyRef = "env:CLUSBOT_TEST_UNUSED_KEY"
	cfg.Runtime.MaxActiveRuns = 1
	cfg.Runtime.MaxRunSeconds = 10
	cfg.Runtime.ModelTimeoutSeconds = 5
	cfg.Runtime.MaxOutputTokens = 256
	cfg.Runtime.MaxModelRequestBytes = model.DefaultMaxRequestBytes
	cfg.Benchmark.BaseURL = providerServer.URL
	cfg.Benchmark.Connection = "bench"
	cfg.Benchmark.Revision = 1
	cfg.Benchmark.Tools = []string{"test"}
	cfg.Benchmark.CredentialEnv = "CLUSBOT_DESKTOP_BENCHMARK_TEST_KEY"
	const origin = "http://127.0.0.1:52123"
	api := httpapi.Server{Store: instance, Origin: origin, Hosts: []string{"127.0.0.1:52123"}}
	stop, err := startAccountRuntime(ctx, &api, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	handler := api.Handler(fstest.MapFS{"index.html": {Data: []byte("fixture")}})
	request := func(method, path, body string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
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
		if w.Code != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	creds := `{"username":"admin","password":"admin-password"}`
	request("POST", "/bootstrap", creds, nil, 201)
	admin := request("POST", "/session", creds, nil, 200).Result().Cookies()[0]
	var session struct {
		User work.Account `json:"user"`
	}
	if err := json.Unmarshal(request("GET", "/session", "", admin, 200).Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	principal := session.User.PrincipalID
	if _, err := api.PersonalAPI(principal); err != nil {
		t.Fatal(err)
	}
	store := api.AdminTaskStores()[principal]
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := store.ImportDomainAssetsForAccount(ctx, principal, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	provider, err := work.NewHTTPBenchmarkProvider(providerServer.URL, "bench", 1, []string{"test"}, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	binding := work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}
	var cases []work.BenchmarkCase
	for _, key := range []string{"first", "collective", "third"} {
		cases = append(cases, work.BenchmarkCase{Key: key, Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}})
	}
	if err := store.PublishBenchmarkProfile(ctx, principal, work.BenchmarkProfile{Key: "fixture", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitBenchmarkRun(ctx, principal, work.BenchmarkRunRequest{RequestKey: "fixture-task", Profile: "fixture", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	chat, _, err := store.SubmitBenchmarkCommand(ctx, "fixture-chat", principal, "", "/benchmark-status "+task, "", provider)
	if err != nil {
		t.Fatal(err)
	}
	text := "把未派发的集合通信测试移除，先预览影响。"
	suggestion, err := store.SubmitBenchmarkRevisionSuggestion(ctx, "fixture-suggestion", principal, chat.ConversationID, text, "", work.BenchmarkRevisionTarget{TaskID: task, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	var progress work.BenchmarkRunProgress
	if err := json.Unmarshal(request("GET", "/domain/benchmarks/"+task, "", admin, 200).Body.Bytes(), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.DispatchControl.Available || progress.DispatchControl.UnavailableReason == "" {
		t.Fatalf("account runtime exposed dispatch control: %+v", progress.DispatchControl)
	}
	pause := `{"action":"pause","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"fixture-account-pause"}`
	request("POST", "/domain/benchmarks/"+task+"/dispatch-control", pause, admin, http.StatusServiceUnavailable)
	if current, err := store.BenchmarkProgress(ctx, principal, task); err != nil || current.DispatchControl.Paused || current.DispatchControl.ControlVersion != 0 {
		t.Fatalf("unavailable account pause changed the task: %+v %v", current.DispatchControl, err)
	}
	base := "/conversations/" + chat.ConversationID + "/benchmark-revision-preview"
	body := fmt.Sprintf(`{"action":"preview_benchmark_revision","source_run_id":%q,"client_request_id":"fixture-preview"}`, suggestion.ID)
	var preview work.BenchmarkRevisionChatReceipt
	if err := json.Unmarshal(request("POST", base, body, admin, 200).Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Proposal.State != "preview_only" || len(preview.Proposal.Preview.Changes.Removed) != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	request("GET", base+"/"+preview.RunID, "", admin, 200)
	var confirmation work.BenchmarkRevisionChatConfirmation
	if err := json.Unmarshal(request("POST", base+"/"+preview.RunID+"/confirm", `{"action":"apply_benchmark_revision","expected_revision":1}`, admin, 200).Body.Bytes(), &confirmation); err != nil {
		t.Fatal(err)
	}
	if confirmation.State != "applied" || confirmation.Revision != 2 || providerCalls.Load() != 0 {
		t.Fatalf("confirmation = %+v, provider calls = %d", confirmation, providerCalls.Load())
	}
	// A task paused before this account runtime opened must remain unchanged;
	// the old durable receipt is still available for an uncertain client.
	version := 0
	priorPause := work.BenchmarkTaskDispatchInput{Action: "pause", ExpectedPlanRevision: 2, ExpectedControlVersion: &version, RequestID: "fixture-prior-pause"}
	if _, err := store.ControlBenchmarkTaskDispatch(ctx, principal, task, priorPause, provider); err != nil {
		t.Fatal(err)
	}
	request("GET", "/domain/benchmarks/"+task+"/dispatch-control?client_request_id=fixture-prior-pause", "", admin, 200)
	resume := `{"action":"resume","expected_plan_revision":2,"expected_control_version":1,"client_request_id":"fixture-account-resume"}`
	request("POST", "/domain/benchmarks/"+task+"/dispatch-control", resume, admin, http.StatusServiceUnavailable)
	if current, err := store.BenchmarkProgress(ctx, principal, task); err != nil || !current.DispatchControl.Paused || current.DispatchControl.ControlVersion != 1 {
		t.Fatalf("unavailable account resume changed the task: %+v %v", current.DispatchControl, err)
	}
	if options := request("GET", "/assistant/composer-options", "", admin, 200).Body.String(); strings.Contains(options, `"command":"/benchmark"`) {
		t.Fatal("desktop revision adapter exposed benchmark admission")
	}
}
