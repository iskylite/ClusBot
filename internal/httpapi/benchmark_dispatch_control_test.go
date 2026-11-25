package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestBenchmarkDispatchControlAPINoModel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "benchmark-gate-api.sqlite")
	a := setupA1(t, dbPath)
	defer a.close()
	ctx := t.Context()
	actor := businessFixturePrincipal(t, a.store)
	const key = "full"
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssetsForAccount(ctx, businessFixturePrincipal(t, a.store), []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}

	b := work.BenchmarkBinding{Connection: grant.Connection, Revision: grant.Revision, Tool: grant.Tool, Validator: "structured-metrics@1"}
	if err := a.store.PublishBenchmarkProfile(ctx, actor, work.BenchmarkProfile{Key: key, Version: 1, Cases: []work.BenchmarkCase{
		{Key: "first", Binding: b, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}},
	}}); err != nil {
		t.Fatal(err)
	}
	task, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "held-api-fixture", Profile: key, Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.store.BenchmarkProgress(ctx, actor, task)
	if err != nil || len(p.Cases) != 1 {
		t.Fatalf("no staged case: %+v %v", p, err)
	}
	base := "/api/v1/domain/benchmarks/" + task
	target := base + "/cases/" + p.Cases[0].StepID + "/execution-control"
	const missing = `{"action":"hold_before_dispatch","expected_plan_revision":1,"client_request_id":"missing-version","reason":"稍后办理"}`
	const hold = `{"action":"hold_before_dispatch","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"hold-api-request","reason":"稍后办理"}`
	if status, _ := a.post(target, missing); status != http.StatusBadRequest {
		t.Fatalf("unversioned hold accepted: %d", status)
	}
	for range 2 {
		if status, r := a.post(target, hold); status != http.StatusOK || r["state"] != "held_before_dispatch" || r["control_version"] != float64(1) {
			t.Fatalf("hold/replay %d: %+v", status, r)
		}
	}
	resp, err := a.ts.Client().Do(a.request(http.MethodGet, base, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var progress work.BenchmarkRunProgress
	if err := json.NewDecoder(resp.Body).Decode(&progress); err != nil || resp.StatusCode != http.StatusOK || len(progress.Cases) != 1 {
		t.Fatalf("progress %d: %+v %v", resp.StatusCode, progress, err)
	}
	c := progress.Cases[0]
	if c.DispatchControl == nil || c.DispatchControl.ControlVersion != 1 || c.Phase != "queued" || c.ExecutionControl != nil ||
		c.Operation != nil || c.Wait != nil || c.TargetHolds != 0 {
		t.Fatalf("progress conflates gate with skip or remote operation: %+v", c)
	}
	const resume = `{"action":"resume_before_dispatch","expected_plan_revision":1,"expected_control_version":1,"client_request_id":"resume-api-request","reason":"继续办理"}`
	if status, r := a.post(target, resume); status != http.StatusOK || r["state"] != "released_before_dispatch" || r["control_version"] != float64(2) {
		t.Fatalf("resume %d: %+v", status, r)
	}
	if status, r := a.post("/api/v1/tasks/"+task+"/cancel", `{"revision":1}`); status != http.StatusOK {
		t.Fatalf("stop %d: %+v", status, r)
	}
	const afterStop = `{"action":"hold_before_dispatch","expected_plan_revision":1,"expected_control_version":2,"client_request_id":"stopped-new-request","reason":"稍后办理"}`
	if status, r := a.post(target, afterStop); status != http.StatusConflict || r["code"] != "benchmark_dispatch_not_pending" {
		t.Fatalf("stop allowed further gate change %d: %+v", status, r)
	}
}
