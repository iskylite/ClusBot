package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestBenchmarkChatTargetAPI(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "benchmark.sqlite"))
	defer a.close()
	const profiles = "/api/v1/domain/benchmark-profiles"
	const target = "/api/v1/domain/benchmark-chat-targets/POD7"
	const selector = `{"profile":"full","version":1}`
	call := func(method, path, body string, authenticated bool) int {
		t.Helper()
		req := a.request(method, path, body)
		if !authenticated {
			req.Header.Del("Cookie")
		}
		resp, err := a.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call(http.MethodPut, target, selector, false); code != http.StatusUnauthorized {
		t.Fatalf("anonymous binding: %d", code)
	}
	if code := call(http.MethodPut, target, selector, true); code != http.StatusBadRequest {
		t.Fatalf("bound unpublished version: %d", code)
	}
	profile := `{"key":"full","version":1,"cases":[{"key":"test","binding":{"connection":"bench","revision":1,"tool":"test","validator":"structured-metrics@1"},"targets":["node-1"],"required_metrics":{"rate":"GB/s"}}]}`
	if code, out := a.post(profiles, profile); code != http.StatusCreated {
		t.Fatalf("publish: %d %v", code, out)
	}
	if code := call(http.MethodPut, target, selector, true); code != http.StatusNoContent {
		t.Fatalf("activate: %d", code)
	}
	// For this test server no provider is configured; matching the profile
	// does not stage an unexecutable job or wait for an unavailable model.
	if code, out := a.post("/api/v1/conversations", `{"client_request_id":"benchmark-inactive","text":"跑 POD7 全套基准"}`); code != http.StatusConflict {
		t.Fatalf("matched without provider: %d %v", code, out)
	}
	if code := call(http.MethodDelete, target, "", true); code != http.StatusNoContent {
		t.Fatalf("disable: %d", code)
	}
	if code, out := a.post("/api/v1/conversations", `{"client_request_id":"benchmark-disabled","text":"跑 POD7 全套基准"}`); code != http.StatusAccepted || out["status"] != "completed" || out["task_id"] != "" {
		t.Fatalf("unbound direct request: %d %v", code, out)
	}
	if code, out := a.post("/api/v1/conversations", `{"client_request_id":"benchmark-read","text":"跑 POD7 全套基准？"}`); code != http.StatusAccepted || out["model_pending"] != true {
		t.Fatalf("question should use ordinary chat: %d %v", code, out)
	}
}

// Only Supports is needed to stage a run. Any unexpected provider I/O
// during this read test will panic through the unset embedded interface.
type benchmarkReadProvider struct{ work.BenchmarkProvider }

func (benchmarkReadProvider) Supports(work.BenchmarkBinding) bool { return true }

func TestBenchmarkProgressLifecycleAPINoModel(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "benchmark-read.sqlite"))
	defer a.close()
	ctx := t.Context()
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssets(ctx, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	profile := work.BenchmarkProfile{Key: "full", Version: 1, Cases: []work.BenchmarkCase{{Key: "test", Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	const actor = "user:session"
	if err := a.store.PublishBenchmarkProfile(ctx, actor, profile); err != nil {
		t.Fatal(err)
	}
	id, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "projection-read", Profile: "full", Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string, auth bool, status int) *http.Response {
		t.Helper()
		req := a.request(http.MethodGet, "/api/v1/domain/benchmarks/"+id, "")
		if !auth {
			req.Header.Del("Cookie")
		}
		res, err := a.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != status {
			t.Fatalf("GET %s: status=%d want %d", id, res.StatusCode, status)
		}
		return res
	}
	get(id, false, http.StatusUnauthorized)
	get("missing", true, http.StatusNotFound)
	res := get(id, true, http.StatusOK)
	var out work.BenchmarkRunProgress
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Cache-Control") != "no-store" || out.TaskID != id || out.State != "pending" || out.StopRequested || len(out.Cases) != 1 || out.Cases[0].Phase != "queued" || out.Cases[0].StepID == "" {
		t.Fatalf("wrong read-only snapshot: %+v", out)
	}
	get(id+"/report", false, http.StatusUnauthorized)
	get("missing/report", true, http.StatusNotFound)
	res = get(id+"/report", true, http.StatusOK)
	var report work.BenchmarkReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Cache-Control") != "no-store" || report.SchemaVersion != 1 || report.TaskID != id ||
		report.AssetAcceptance != "not_evaluated" || len(report.Cases) != 1 || report.Cases[0].ToolOutcome != "not_started" {
		t.Fatalf("wrong no-model report: %+v", report)
	}

}
