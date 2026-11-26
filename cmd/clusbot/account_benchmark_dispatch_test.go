package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAccountBenchmarkDispatchOptInEndToEnd(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := openAccountInstance(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const env = "CLUSBOT_TEST_ACCOUNT_BENCHMARK_TOKEN"
	t.Setenv(env, "fixture-benchmark-token")
	var mu sync.Mutex
	var starts, lookups, gets int
	var recordedKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-benchmark-token" {
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected provider operation", 400)
			return
		}
		var in struct {
			work.BenchmarkJobRequest
			JobID string `json:"job_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.IdempotencyKey == "" || len(in.Targets) != 1 || in.Targets[0] != "node-1" {
			t.Errorf("unsafe provider request: %+v %v", in, err)
			http.Error(w, "bad input", 400)
			return
		}
		switch r.URL.Path {
		case "/start":
			starts++
			recordedKey = in.IdempotencyKey
			// The fake accepts the operation before its response is lost.
			http.Error(w, "response lost", 502)
		case "/lookup":
			lookups++
			if in.IdempotencyKey != recordedKey {
				t.Error("lookup lost original provider idempotency key")
				http.Error(w, "wrong operation", 409)
				return
			}
			_, _ = w.Write([]byte(`{"job_id":"original-provider-job"}`))
		case "/get":
			gets++
			if in.IdempotencyKey != recordedKey || in.JobID != "original-provider-job" {
				t.Error("get did not address original job")
				http.Error(w, "wrong operation", 409)
				return
			}
			_, _ = w.Write([]byte(`{"job_id":"original-provider-job","state":"completed","measurements":[{"target":"node-1","outcome":"measured","metrics":{"rate":{"value":117,"unit":"GB/s"}}}]}`))
		default:
			t.Errorf("unexpected upstream endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	var cfg config
	cfg.DataDir = dir
	cfg.Model.Provider = model.ProtocolResponses
	// The exact user command bypasses a model; a fallback cannot reach a
	// real provider even if this fixture unexpectedly takes that route.
	cfg.Model.BaseURL = "http://127.0.0.1:1"
	cfg.Model.KeyRef = "env:CLUSBOT_TEST_UNUSED_ACCOUNT_BENCHMARK_MODEL"
	cfg.Runtime.MaxActiveRuns = 1
	cfg.Runtime.MaxRunSeconds = 10
	cfg.Runtime.ModelTimeoutSeconds = 5
	cfg.Runtime.MaxOutputTokens = 256
	cfg.Runtime.MaxModelRequestBytes = model.DefaultMaxRequestBytes
	cfg.Benchmark.BaseURL = upstream.URL
	cfg.Benchmark.Connection = "fixed-bench"
	cfg.Benchmark.Revision = 1
	cfg.Benchmark.Tools = []string{"test"}
	cfg.Benchmark.CredentialEnv = env

	const origin = "http://127.0.0.1:52121"
	var api httpapi.Server
	var stop func()
	var handler http.Handler
	startServer := func() {
		t.Helper()
		api = httpapi.Server{Store: db, Origin: origin, Hosts: []string{"127.0.0.1:52121"}}
		var err error
		stop, err = startAccountRuntime(context.Background(), &api, cfg)
		if err != nil {
			t.Fatal(err)
		}
		handler = api.Handler(fstest.MapFS{"index.html": {Data: []byte("fixture")}})
	}
	startServer()
	defer func() { stop() }()
	request := func(method, path, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, origin+"/api/v1"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s = %d, want %d; %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	const creds = `{"username":"admin","password":"fixture-admin-secret"}`
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
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "fixed-bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := store.ImportDomainAssetsForAccount(ctx, principal, []work.DomainAssetEntry{
		{ObjectRef: "node-1", Kind: "node", Identity: "inventory-node-1", PhysicalSerial: "SERIAL-1", Benchmark: []work.DomainBenchmarkGrant{grant}},
		{ObjectRef: "unbound-node", Kind: "node", Identity: "alias-only", Benchmark: []work.DomainBenchmarkGrant{grant}},
	}); err != nil {
		t.Fatal(err)
	}
	binding := work.BenchmarkBinding{Connection: "fixed-bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}
	profile := func(key, target string) work.BenchmarkProfile {
		return work.BenchmarkProfile{Key: key, Version: 1, Cases: []work.BenchmarkCase{{Key: "first", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	}
	if err := store.PublishBenchmarkProfile(ctx, principal, profile("prepared", "node-1")); err != nil {
		t.Fatal(err)
	}
	adapter, err := configuredAccountBenchmarkProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.SubmitBenchmarkRun(ctx, principal, work.BenchmarkRunRequest{RequestKey: "saved-before-enable", Profile: "prepared", Version: 1}, adapter)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", "/domain/benchmarks", `{"request_key":"must-not-run","profile":"prepared","version":1}`, admin, 503)
	stop()

	cfg.Benchmark.EnableDispatch = true
	startServer()
	statusOf := func(task string) work.BenchmarkRunProgress {
		t.Helper()
		var p work.BenchmarkRunProgress
		if err := json.Unmarshal(request("GET", "/domain/benchmarks/"+task, "", admin, 200).Body.Bytes(), &p); err != nil || len(p.Cases) != 1 {
			t.Fatalf("bad progress %+v %v", p, err)
		}
		return p
	}
	configure := func(p work.BenchmarkProfile) {
		t.Helper()
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		request("POST", "/domain/benchmark-profiles", string(body), admin, 201)
	}
	configure(profile("unbound", "unbound-node"))
	var preflight work.BenchmarkPreflight
	if err := json.Unmarshal(request("POST", "/domain/benchmark-preflight", `{"profile":"unbound","version":1}`, admin, 200).Body.Bytes(), &preflight); err != nil ||
		preflight.CanQueue || len(preflight.Cases) != 1 || len(preflight.Cases[0].Blockers) != 1 || preflight.Cases[0].Blockers[0].Code != "physical_serial_missing" {
		t.Fatalf("preflight allowed target without physical identity: %+v %v", preflight, err)
	}
	request("POST", "/domain/benchmarks", `{"request_key":"unbound","profile":"unbound","version":1}`, admin, 400)
	request("POST", "/domain/benchmarks", `{"request_key":"no-session","profile":"prepared","version":1}`, nil, 401)
	// Returning an old request's original receipt does not convert it into
	// a newly authorized dispatch.
	var oldReplay map[string]string
	if err := json.Unmarshal(request("POST", "/domain/benchmarks", `{"request_key":"saved-before-enable","profile":"prepared","version":1}`, admin, 202).Body.Bytes(), &oldReplay); err != nil || oldReplay["task_id"] != old {
		t.Fatalf("original receipt replay: %+v %v", oldReplay, err)
	}
	request("POST", "/domain/benchmarks/"+old+"/dispatch-control",
		`{"action":"pause","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"no-admission-control"}`, admin, 409)
	const order = `{"client_request_id":"explicit-chat-run","text":"/benchmark prepared@1"}`
	submit := func() string {
		t.Helper()
		request("POST", "/conversations", order, admin, 202)
		var p struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(request("GET", "/admissions/explicit-chat-run", "", admin, 200).Body.Bytes(), &p); err != nil || p.TaskID == "" {
			t.Fatalf("chat did not durably link the original task: %+v %v", p, err)
		}
		return p.TaskID
	}
	task := submit()
	if submit() != task {
		t.Fatal("repeated chat order created a new task")
	}
	wait := func(phase, operation string, timeout time.Duration) work.BenchmarkRunProgress {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			p := statusOf(task)
			c := p.Cases[0]
			if c.Phase == phase && (operation == "" || c.Operation != nil && c.Operation.State == operation) {
				return p
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("task did not reach %s/%s: %+v", phase, operation, statusOf(task))
		return work.BenchmarkRunProgress{}
	}
	got := wait("submitting", string(work.RuntimeOperationReconcile), 5*time.Second)
	if got.Cases[0].TargetHolds != 1 || statusOf(old).Cases[0].Phase != "queued" {
		t.Fatal("unknown operation lost hold or old queued work became eligible")
	}
	stop()
	startServer()
	verify := wait("verified", "", 16*time.Second)
	if verify.Cases[0].JobID != "original-provider-job" || verify.Cases[0].TargetHolds != 0 || statusOf(old).Cases[0].Phase != "queued" {
		t.Fatalf("original job not recovered correctly: %+v", verify)
	}
	request("GET", "/domain/benchmarks/"+task+"/report", "", admin, 200)
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 || lookups != 1 || gets != 1 {
		t.Fatalf("unexpected provider counts: start=%d lookup=%d get=%d", starts, lookups, gets)
	}
}

func TestAccountBenchmarkDispatchRejectsIncompleteConfiguration(t *testing.T) {
	var cfg config
	cfg.Benchmark.EnableDispatch = true
	if err := validateAccountBenchmarkConfig(cfg); err == nil {
		t.Fatal("dispatch configured without provider was accepted")
	}
	cfg.Benchmark.BaseURL = "http://127.0.0.1:1"
	cfg.Benchmark.Connection = "bench"
	cfg.Benchmark.Revision = 1
	cfg.Benchmark.Tools = []string{"test"}
	if err := validateAccountBenchmarkConfig(cfg); err == nil {
		t.Fatal("dispatch configured without credential was accepted")
	}
	cfg.Benchmark.CluskitStdio.Command = "/no-such-runtime-fixture"
	if err := validateAccountBenchmarkConfig(cfg); err == nil {
		t.Fatal("simultaneously configured HTTP and native stdio accepted")
	}
	cfg.Benchmark.BaseURL = ""
	cfg.Benchmark.EnableDispatch = false
	if err := validateAccountBenchmarkConfig(cfg); err == nil {
		t.Fatal("native stdio accepted without explicit opt-in")
	}
}
