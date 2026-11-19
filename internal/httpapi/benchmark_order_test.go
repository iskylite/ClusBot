package httpapi

import (
	"net/http"
	"path/filepath"
	"testing"
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
