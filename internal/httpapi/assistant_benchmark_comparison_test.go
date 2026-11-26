package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const comparisonFixtureAsset = "compare-fixture-node"
const comparisonFixtureEvidence = "private normalized synthetic result file"

type assistantComparisonWorker struct{ starts, reads atomic.Int32 }

func (*assistantComparisonWorker) Supports(b work.BenchmarkBinding) bool {
	return b.Connection == "fixture-comparison" && b.Revision == 1 && b.Tool == "compare-fixture" && b.Validator == "structured-metrics@1"
}
func (p *assistantComparisonWorker) Start(_ context.Context, req work.BenchmarkJobRequest) (string, error) {
	if req.Case != "bandwidth" || len(req.Targets) != 1 || req.Targets[0] != comparisonFixtureAsset {
		return "", fmt.Errorf("wrong local fixture request")
	}
	return fmt.Sprintf("fixture-job-%d", p.starts.Add(1)), nil
}
func (*assistantComparisonWorker) Lookup(context.Context, work.BenchmarkJobRequest) (string, error) {
	return "", fmt.Errorf("comparison must not look up an external job")
}
func (p *assistantComparisonWorker) Get(_ context.Context, req work.BenchmarkJobRequest, job string) (work.BenchmarkJobResult, error) {
	if req.Case != "bandwidth" || job != "fixture-job-1" && job != "fixture-job-2" {
		return work.BenchmarkJobResult{}, fmt.Errorf("unrelated local fixture receipt")
	}
	p.reads.Add(1)
	v := 100.0
	if job == "fixture-job-2" {
		v = 125
	}
	sum := sha256.Sum256([]byte(comparisonFixtureEvidence))
	return work.BenchmarkJobResult{JobID: job, State: "completed", Measurements: []work.BenchmarkMeasurement{{
		Target: comparisonFixtureAsset, Outcome: "measured", ActualConfig: map[string]string{"method": "fixture-method-01", "gpu_count": "40"},
		Metrics: map[string]work.BenchmarkMeasuredMetric{"rate": {Value: v, Unit: "GB/s"}},
		Raw:     &work.BenchmarkRawArtifact{Ref: "fixture-original", Bytes: []byte(comparisonFixtureEvidence), SHA256: fmt.Sprintf("%x", sum)},
	}}}, nil
}

type assistantComparisonFixture struct {
	*discoveryA2UIFixture
	worker assistantComparisonWorker
	calls  atomic.Int32
	pair   [2]string
}

func makeAssistantComparisonFixture(t *testing.T, next func(*http.Request) string) *assistantComparisonFixture {
	t.Helper()
	f := &assistantComparisonFixture{}
	t.Setenv("CLUSBOT_REPORT_COMPARE_FIXTURE_KEY", "loopback-model-only")
	modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		var req struct{ Input []struct{ Content string } }
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(raw, &req) != nil || len(req.Input) != 1 ||
			req.Input[0].Content != `{"requested_action":"preview_benchmark_comparison","scope_kind":"original_report_pair"}` ||
			bytes.Contains(raw, []byte(comparisonFixtureAsset)) || bytes.Contains(raw, []byte(comparisonFixtureEvidence)) ||
			bytes.Contains(raw, []byte("GB/s")) {
			t.Error("comparison sent private evidence, metric values or a task identifier to the model")
			http.Error(w, "invalid controlled model input", 400)
			return
		}
		f.calls.Add(1)
		out := `{"next":"show_comparison"}`
		if next != nil {
			out = next(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	t.Cleanup(modelHTTP.Close)
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "local-comparison-fixture", Protocol: model.ProtocolResponses, BaseURL: modelHTTP.URL,
			KeyRef: "env:CLUSBOT_REPORT_COMPARE_FIXTURE_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	binding := work.BenchmarkBinding{Connection: "fixture-comparison", Revision: 1, Tool: "compare-fixture", Validator: "structured-metrics@1"}
	// Account workers require a pinned provider namespace and physical
	// serial. Use the real approved adapter to a test-only loopback server.
	physicalFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-compare" {
			http.Error(w, "invalid isolated fixture", 400)
			return
		}
		var in struct {
			work.BenchmarkJobRequest
			Job string `json:"job_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid fixture payload", 400)
			return
		}
		switch r.URL.Path {
		case "/start":
			id, err := f.worker.Start(r.Context(), in.BenchmarkJobRequest)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": id})
		case "/get":
			result, err := f.worker.Get(r.Context(), in.BenchmarkJobRequest, in.Job)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			t.Error("comparison called a non-fixture method")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(physicalFixture.Close)
	configured, err := work.NewHTTPBenchmarkProvider(physicalFixture.URL, binding.Connection, binding.Revision, []string{binding.Tool}, "fixture-compare")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := configured.ForAccountDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InstallBenchmarkProvider(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	grant := work.DomainBenchmarkGrant{Connection: binding.Connection, Revision: binding.Revision, Tool: binding.Tool,
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := f.store.ImportDomainAssetsForAccount(t.Context(), f.actor, []work.DomainAssetEntry{{
		ObjectRef: comparisonFixtureAsset, Kind: "node", Identity: "synthetic-hardware", PhysicalSerial: "synthetic-compare-serial", Read: true, Benchmark: []work.DomainBenchmarkGrant{grant},
	}}); err != nil {
		t.Fatal(err)
	}
	p := work.BenchmarkProfile{Key: "comparison-fixture", Version: 1, Cases: []work.BenchmarkCase{{
		Key: "bandwidth", Binding: binding, Targets: []string{comparisonFixtureAsset}, ActualConfig: map[string]string{"method": "fixture-method-01", "gpu_count": "40"},
		RequiredMetrics: map[string]string{"rate": "GB/s"}, ComparisonKeys: []string{"method", "gpu_count"}, NeedsRawEvidence: true,
	}}}
	if err := f.store.PublishBenchmarkProfile(t.Context(), f.actor, p); err != nil {
		t.Fatal(err)
	}
	// Drive the real local state machine and verifier. No connector/network
	// provider is installed on the server for the comparison UI.
	for n := range f.pair {
		id, err := f.store.SubmitBenchmarkRun(t.Context(), f.actor, work.BenchmarkRunRequest{RequestKey: fmt.Sprintf("report-fixture-%d", n), Profile: p.Key, Version: p.Version}, worker)
		if err != nil {
			t.Fatal(err)
		}
		f.pair[n] = id
		ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
		done := make(chan struct{})
		go func() { defer close(done); f.store.RunBenchmarks(ctx, worker) }()
		func() {
			defer func() { stop(); <-done }()
			var report work.BenchmarkReport
			for {
				select {
				case <-ctx.Done():
					t.Fatalf("local original report did not settle: %+v", report)
				case <-time.After(35 * time.Millisecond):
					report, err = f.store.ReadBenchmarkReport(ctx, f.actor, id)
					if err != nil {
						t.Fatal(err)
					}
					if len(report.Cases) == 1 && report.TaskState == "succeeded" && report.Cases[0].RequiredEvidence == "covered" {
						return
					}
				}
			}
		}()
	}
	return f
}

func (f *assistantComparisonFixture) prompt() string {
	return "比较基准报告 " + f.pair[0] + " " + f.pair[1] + " bandwidth"
}

func (f *assistantComparisonFixture) open(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": f.prompt(), "client_message_id": "fixture-comparison-open"})
	req := httptest.NewRequest("POST", "https://ops.example/api/v1/conversations", bytes.NewReader(raw))
	req.AddCookie(f.cookie)
	req.Header.Set("Origin", "https://ops.example")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ClusBot-Assistant-Cards", "1")
	first := httptest.NewRecorder()
	f.account.handler.ServeHTTP(first, req)
	var run struct {
		ID           string `json:"run_id"`
		Task         string `json:"task_id"`
		Conversation string `json:"conversation_id"`
	}
	accountJSON(t, first, &run)
	if first.Code != 202 || run.ID == "" || run.Conversation == "" || run.Task != "" {
		t.Fatalf("read command did not produce a dedicated reply: %d %s", first.Code, first.Body)
	}
	path := "/api/v1/conversations/" + run.Conversation + "/report-card/" + run.ID
	res := f.request("GET", path, "", nil, f.cookie)
	var card a2uiDiscoveryReply
	accountJSON(t, res, &card)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "核对并展开报告对比") || strings.Contains(res.Body.String(), "GB/s") || f.calls.Load() != 0 {
		t.Fatalf("report output was not gated by scoped action: %d %s", res.Code, res.Body)
	}
	return path, run.ID, renderedA2UIEvent(t, card, assistantBenchmarkCompareAction)
}

func TestAssistantBenchmarkComparisonNoDispatchOrPrivateEgress(t *testing.T) {
	f := makeAssistantComparisonFixture(t, nil)
	path, run, action := f.open(t)
	otherLogin := f.account.login("admin", "correct horse battery staple")
	if res := f.request("POST", path+"/actions", "report-cross-login", action, otherLogin); res.Code != 409 {
		t.Fatalf("separate login could replay original action: %d %s", res.Code, res.Body)
	}
	a := action["action"].(map[string]any)
	fields := a["context"].(map[string]any)
	fields["task_id"] = f.pair[1]
	if res := f.request("POST", path+"/actions", "report-task-injection", action, f.cookie); res.Code != 400 {
		t.Fatalf("client selected a different source: %d %s", res.Code, res.Body)
	}
	delete(fields, "task_id")
	res := f.request("POST", path+"/actions", "report-check-both", action, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "基线 100 GB/s；本次 125 GB/s；差值 25 GB/s") ||
		strings.Contains(res.Body.String(), comparisonFixtureEvidence) || strings.Contains(res.Body.String(), f.pair[0]) {
		t.Fatalf("wrong or excessive output: %d %s", res.Code, res.Body)
	}
	if res := f.request("GET", path, "", nil, f.cookie); res.Code != 200 || strings.Contains(res.Body.String(), "125 GB/s") ||
		f.calls.Load() != 1 || f.worker.starts.Load() != 2 || f.worker.reads.Load() != 2 {
		t.Fatalf("comparison was repeated or did provider work: %d %s", res.Code, res.Body)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs, jobs, artifacts int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM benchmark_runs),(SELECT count(*) FROM operation_jobs),
(SELECT count(*) FROM benchmark_evidence)`).Scan(&runs, &jobs, &artifacts); err != nil || runs != 2 || jobs != 2 || artifacts != 2 {
		t.Fatalf("preview added or removed runs, jobs or evidence: %d/%d/%d %v", runs, jobs, artifacts, err)
	}
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "assistant.benchmark_comparison_requested" && event.Data != "{}" {
			t.Fatal("private benchmark inputs leaked into generic events")
		}
	}
}

func TestAssistantBenchmarkComparisonRechecksOriginalEvidenceAfterModel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := makeAssistantComparisonFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return `{"next":"show_comparison"}`
	})
	path, _, action := f.open(t)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.request("POST", path+"/actions", "report-check-then-delete", action, f.cookie) }()
	select {
	case <-started:
	case <-time.After(6 * time.Second):
		t.Fatal("controlled model never got the event")
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM benchmark_evidence WHERE step_id=(SELECT step_id FROM benchmark_cases WHERE task_id=? LIMIT 1)", f.pair[1]); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case res := <-result:
		if res.Code != 409 || strings.Contains(res.Body.String(), "125 GB/s") {
			t.Fatalf("revoked original still shown: %d %s", res.Code, res.Body)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("comparison did not recheck")
	}
}

func TestAssistantBenchmarkComparisonBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local browser tooling")
	}
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	defer stop()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(stage string, input []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", stage, err, stderr.String())
		}
		return out
	}
	var encoded map[string][]byte
	if err := json.Unmarshal(browser("bundle", nil), &encoded); err != nil || len(encoded["index.html"]) == 0 {
		t.Fatalf("failed to compile actual UI: %v", err)
	}
	files := fstest.MapFS{}
	for path, data := range encoded {
		files[path] = &fstest.MapFile{Data: data}
	}
	f := makeAssistantComparisonFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	real := f.server
	real.Origin, real.Hosts = origin, []string{addr}
	server.Config.Handler = real.Handler(files)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "prompt": f.prompt()})
	_ = browser("browse-benchmark-comparison", input)
	if f.calls.Load() != 1 || f.worker.starts.Load() != 2 || f.worker.reads.Load() != 2 {
		t.Fatalf("unexpected model or execution calls after comparison: %d, %d, %d", f.calls.Load(), f.worker.starts.Load(), f.worker.reads.Load())
	}
}
