package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestBenchmarkTaskDispatchHTTPNoModel(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "task-pause.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	provider := benchmarkReadProvider{}
	server := Server{Store: store, Token: "a1-token", Hosts: []string{"127.0.0.1"}, Settings: &model.Settings{Store: store}, BenchmarkProvider: provider}
	a := &a1Server{store: store, ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("fixture")}}))}
	defer a.close()
	a.cookie = a.login()
	ctx := t.Context()
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := store.ImportDomainAssets(ctx, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	binding := work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}
	if err := store.PublishBenchmarkProfile(ctx, "user:session", work.BenchmarkProfile{Key: "pause-api", Version: 1, Cases: []work.BenchmarkCase{{Key: "first", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}); err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitBenchmarkRun(ctx, "user:session", work.BenchmarkRunRequest{RequestKey: "pause-http-fixture", Profile: "pause-api", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/domain/benchmarks/" + task
	const pause = `{"action":"pause","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"pause-http-request"}`
	for range 2 {
		if status, body := a.post(base+"/dispatch-control", pause); status != http.StatusOK || body["state"] != "paused" || body["control_version"] != float64(1) {
			t.Fatalf("pause/replay %d: %+v", status, body)
		}
	}
	response, err := a.ts.Client().Do(a.request(http.MethodGet, base+"/dispatch-control?client_request_id=pause-http-request", ""))
	if err != nil {
		t.Fatal(err)
	}
	var receipt work.BenchmarkTaskDispatchReceipt
	if err := json.NewDecoder(response.Body).Decode(&receipt); err != nil || response.StatusCode != http.StatusOK || receipt.State != "paused" || receipt.ControlVersion != 1 {
		t.Fatalf("lookup %d: %+v %v", response.StatusCode, receipt, err)
	}
	response.Body.Close()
	response, err = a.ts.Client().Do(a.request(http.MethodGet, base, ""))
	if err != nil {
		t.Fatal(err)
	}
	var progress work.BenchmarkRunProgress
	if err := json.NewDecoder(response.Body).Decode(&progress); err != nil || response.StatusCode != http.StatusOK || !progress.DispatchControl.Paused || !progress.DispatchControl.Available {
		t.Fatalf("progress %d: %+v %v", response.StatusCode, progress, err)
	}
	response.Body.Close()
	const stale = `{"action":"resume","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"stale-resume-http"}`
	if status, _ := a.post(base+"/dispatch-control", stale); status != http.StatusConflict {
		t.Fatalf("stale accepted: %d", status)
	}
	const resume = `{"action":"resume","expected_plan_revision":1,"expected_control_version":1,"client_request_id":"resume-http-request"}`
	if status, body := a.post(base+"/dispatch-control", resume); status != http.StatusOK || body["state"] != "resumed" || body["control_version"] != float64(2) {
		t.Fatalf("resume %d: %+v", status, body)
	}
}
