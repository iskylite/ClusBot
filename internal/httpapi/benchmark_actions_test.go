package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestBenchmarkActionProjectionMatchesControlGuards(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "benchmark-actions.sqlite")
	a := setupA1(t, dbPath)
	defer a.close()
	ctx := t.Context()
	const actor, key = "user:session", "full"
	const hold, resume, skip = "hold_before_dispatch", "resume_before_dispatch", "skip_this_run"
	g := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssets(ctx, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{g}}}); err != nil {
		t.Fatal(err)
	}
	p := work.BenchmarkProfile{Key: key, Version: 1, Cases: []work.BenchmarkCase{{Key: "first", Targets: []string{"node-1"},
		Binding:         work.BenchmarkBinding{Connection: "bench", Tool: "test", Revision: 1, Validator: "structured-metrics@1"},
		RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}
	if err := a.store.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	task, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "action-read-fixture", Profile: key, Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/domain/benchmarks/" + task
	get := func(want int) (work.BenchmarkCaseProgress, string) {
		t.Helper()
		resp, err := a.ts.Client().Do(a.request(http.MethodGet, base, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || resp.StatusCode != want {
			t.Fatalf("progress: HTTP %d, %s %v", resp.StatusCode, raw, err)
		}
		if want != 200 {
			return work.BenchmarkCaseProgress{}, string(raw)
		}
		var out work.BenchmarkRunProgress
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Cases) != 1 || out.Cases[0].Targets[0].Ref != "node-1" {
			t.Fatalf("progress projection: %+v %v", out, err)
		}
		return out.Cases[0], string(raw)
	}
	expect := func(c work.BenchmarkCaseProgress, action, reason string, version int) {
		t.Helper()
		got, ok := c.Actions[action]
		if !ok || got.Enabled != (reason == "") || got.ReasonCode != reason || got.ExpectedPlanRevision != 1 ||
			got.ExpectedControlVersion != version || (got.Reason == "") != (reason == "") {
			t.Fatalf("%s: got %+v, expected version %d reason %q", action, got, version, reason)
		}
	}
	first, raw := get(200)
	if strings.Contains(raw, "authorization_refs") || strings.Contains(raw, "current_revision") || strings.Contains(raw, "param_constraints") {
		t.Fatalf("internal pin metadata was returned: %s", raw)
	}
	expect(first, hold, "", 0)
	expect(first, skip, "", 0)
	expect(first, resume, "case_not_held", 0)

	path := base + "/cases/" + first.StepID + "/execution-control"
	const holdPayload = `{"action":"hold_before_dispatch","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"hold-action-fixture","reason":"等窗口"}`
	if status, response := a.post(path, holdPayload); status != 200 {
		t.Fatalf("hold: %d %+v", status, response)
	}
	held, _ := get(200)
	expect(held, hold, "case_already_held", 1)
	expect(held, resume, "", 1)
	expect(held, skip, "", 1)
	const staleResume = `{"action":"resume_before_dispatch","expected_plan_revision":1,"expected_control_version":0,"client_request_id":"stale-resume-fixture","reason":"窗口已到"}`
	if status, _ := a.post(path, staleResume); status != 409 {
		t.Fatalf("stale projection became permission: %d", status)
	}

	// A second SQLite reader obtains only the opaque fixture grant ID so
	// the real Work revocation method can be used; it never appears in HTTP.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	var grant string
	if err := db.QueryRow("SELECT auth_id FROM authorizations WHERE action='benchmark.execute'").Scan(&grant); err != nil {
		t.Fatal(err)
	}
	if err := a.store.RevokeAuthorizationByID(ctx, grant, actor); err != nil {
		t.Fatal(err)
	}
	revoked, raw := get(200)
	if strings.Contains(raw, grant) {
		t.Fatal("grant identity leaked in action projection")
	}
	expect(revoked, hold, "case_already_held", 1)
	for _, action := range []string{resume, skip} {
		expect(revoked, action, "frozen_authorization_unavailable", 1)
	}
	const currentResume = `{"action":"resume_before_dispatch","expected_plan_revision":1,"expected_control_version":1,"client_request_id":"revoked-resume-fixture","reason":"窗口已到"}`
	if status, _ := a.post(path, currentResume); status != 403 {
		t.Fatalf("revoked snapshot/mutation mismatch: %d", status)
	}
	if status, r := a.post(path, holdPayload); status != 200 || r["control_version"] != float64(1) {
		t.Fatalf("authorization change erased historical idempotent receipt: %d %+v", status, r)
	}
	if status, _ := a.post("/api/v1/tasks/"+task+"/cancel", `{"revision":1}`); status != 200 {
		t.Fatalf("stop failed: %d", status)
	}
	stopped, _ := get(200)
	for _, action := range []string{hold, resume, skip} {
		expect(stopped, action, "task_stopping", 1)
	}
	if _, err := db.Exec("UPDATE benchmark_runs SET actor='another-owner' WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	if _, body := get(http.StatusNotFound); strings.Contains(body, "actions") || strings.Contains(body, first.StepID) {
		t.Fatalf("foreign owner read action projection: %s", body)
	}
}
