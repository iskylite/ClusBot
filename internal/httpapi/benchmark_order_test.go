package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestBenchmarkChatTargetAPI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "benchmark.sqlite")
	a := setupA1(t, dbPath)
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
	readState := func(path string, want int) work.BenchmarkChatTargetState {
		t.Helper()
		resp, err := a.ts.Client().Do(a.request(http.MethodGet, path, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("state HTTP %d want %d", resp.StatusCode, want)
		}
		var state work.BenchmarkChatTargetState
		if want == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	if state := readState(target+"?include_disabled=true", http.StatusOK); state.BindingRevision != 0 || state.State != "disabled" || state.Target != "POD7" {
		t.Fatalf("untouched target %+v", state)
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
	if state := readState(target, http.StatusOK); state.BindingRevision != 1 || state.State != "bound" || state.Profile != "full" || state.Version != 1 {
		t.Fatalf("bound target %+v", state)
	}

	// The familiar sentence has a local suggestion fallback even with no
	// configured model or provider, but no longer accepts a real test run.
	grant := work.DomainBenchmarkGrant{Connection: "bench", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssetsForAccount(t.Context(), businessFixturePrincipal(t, a.store), []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}

	const oldPhrase = `{"client_request_id":"benchmark-inactive","text":"跑 POD7 全套基准"}`
	for range 2 {
		code, out := a.post("/api/v1/conversations", oldPhrase)
		hint, ok := out["benchmark_suggestion"].(map[string]any)
		if code != http.StatusAccepted || !ok || hint["target"] != "POD7" || hint["state"] != "explicit_preview_required" || out["task_id"] != "" {
			t.Fatalf("familiar phrase skipped confirmation or gave no preview path: %d %v", code, out)
		}
	}
	if code := call(http.MethodDelete, target, "", true); code != http.StatusConflict {
		t.Fatalf("legacy clear silently changed an existing binding: %d", code)
	}
	if code := call(http.MethodDelete, target+"?expected_binding_revision=1", "", true); code != http.StatusNoContent {
		t.Fatalf("disable: %d", code)
	}
	readState(target, http.StatusNotFound)
	if state := readState(target+"?include_disabled=true", http.StatusOK); state.BindingRevision != 2 || state.State != "disabled" {
		t.Fatalf("clear did not retain revision tombstone %+v", state)
	}
	resp, err := a.ts.Client().Do(a.request(http.MethodPut, target, `{"profile":"full","version":1,"expected_binding_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var conflict map[string]any
	err = json.NewDecoder(resp.Body).Decode(&conflict)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || err != nil || conflict["code"] != "benchmark_chat_target_conflict" {
		t.Fatalf("wrong stale binding error: %d %+v %v", resp.StatusCode, conflict, err)
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
	dbPath := filepath.Join(t.TempDir(), "benchmark-read.sqlite")
	a := setupA1(t, dbPath)
	defer a.close()
	ctx := t.Context()
	grant := work.DomainBenchmarkGrant{Revision: 1, Connection: "bench", Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssetsForAccount(ctx, businessFixturePrincipal(t, a.store), []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	profile := work.BenchmarkProfile{Key: "full", Version: 1, Cases: []work.BenchmarkCase{{Key: "test", Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	actor := businessFixturePrincipal(t, a.store)

	if err := a.store.PublishBenchmarkProfile(ctx, actor, profile); err != nil {
		t.Fatal(err)
	}
	id, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "projection-read", Profile: "full", Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	dedicated, err := a.store.Detail(id, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct{ path, body string }{
		{"/task", `{"client_request_id":"benchmark-no-model-generic-task","text":"继续办理"}`},
		{"/messages", `{"client_request_id":"benchmark-no-model-generic-delivery","text":"修改验收条件","acceptance_criteria":"不要再测","revision":1}`},
	} {
		status, out := a.post("/api/v1/conversations/"+dedicated.Conversation.ID+req.path, req.body)
		if status != http.StatusConflict || out["code"] != "benchmark_recovery_unsupported" {
			t.Fatalf("dedicated task action waited for model or was accepted: %d %+v", status, out)
		}
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
	if res.Header.Get("Cache-Control") != "no-store" || out.TaskID != id || out.State != "pending" || out.StopRequested || out.AssetScope.TargetCount != 1 || len(out.AssetScope.TargetRefs) != 1 || out.AssetScope.TargetRefs[0] != "node-1" || len(out.Cases) != 1 || out.Cases[0].Phase != "queued" || out.Cases[0].StepID == "" {
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

	get(id+"/evidence", false, http.StatusUnauthorized)
	get("missing/evidence", true, http.StatusNotFound)
	res = get(id+"/evidence", true, http.StatusOK)
	var page work.BenchmarkEvidencePage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil || page.TaskID != id || page.Items == nil || len(page.Items) != 0 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("wrong empty no-model inventory: %+v %v", page, err)
	}

	// The domain-specific rejection takes precedence over an unrelated
	// model setup error, both while stopping and once fully stopped.
	assertRecoveryRejected := func(revision int) {
		t.Helper()
		for i, extra := range []string{"", `,"retry":true`, `,"reassign":true`} {
			body := fmt.Sprintf(`{"revision":%d,"client_request_id":"benchmark-generic-recovery-%d"%s}`, revision, i, extra)
			path := "/api/v1/tasks/" + id + "/resume"
			if status, result := a.post(path, body); status != http.StatusConflict || result["code"] != "benchmark_recovery_unsupported" {
				t.Fatalf("generic recovery %d was not rejected independently of model: %d %+v", i, status, result)
			}
		}
	}
	if status, out := a.post("/api/v1/tasks/"+id+"/cancel", `{"revision":1}`); status != http.StatusOK {
		t.Fatalf("could not stop the queued benchmark through HTTP: %d %+v", status, out)
	}
	assertRecoveryRejected(1)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.store.RunBenchmarks(workerCtx, benchmarkReadProvider{}) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		progress, err := a.store.BenchmarkProgress(ctx, actor, id)
		if err != nil {
			t.Fatal(err)
		}
		if progress.State == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued cancellation did not settle: %+v", progress)
		}
		time.Sleep(10 * time.Millisecond)
	}
	detail, err := a.store.Detail(id, true)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryRejected(detail.Task.Revision)
	runs, err := a.store.TaskRuns(ctx, id, "")
	if err != nil || len(runs.Items) != 0 {
		t.Fatalf("domain stop/recovery created a generic model execution: %+v %v", runs, err)
	}

	cancel()
	<-done
	controlled, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "execution-control-api", Profile: "full", Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := a.store.BenchmarkProgress(ctx, actor, controlled)
	if err != nil || len(baseline.Cases) != 1 {
		t.Fatalf("cannot locate API execution target: %+v %v", baseline, err)
	}
	path := "/api/v1/domain/benchmarks/" + controlled + "/cases/" + baseline.Cases[0].StepID + "/execution-control"
	body := `{"action":"skip_this_run","expected_plan_revision":1,"client_request_id":"execution-control-api-request","reason":"本次不执行"}`
	for range 2 {
		status, receipt := a.post(path, body)
		if status != http.StatusOK || receipt["state"] != "skipped_this_run" || receipt["step_id"] != baseline.Cases[0].StepID || receipt["plan_revision"] != float64(1) {
			t.Fatalf("HTTP skip returned wrong stable receipt: %d %+v", status, receipt)
		}
	}
	if status, receipt := a.post(path, strings.Replace(body, "execution-control-api-request", "next-control-api-request", 1)); status != http.StatusConflict || receipt["code"] != "benchmark_case_not_pending" {
		t.Fatalf("new skip did not refuse terminal case: %d %+v", status, receipt)
	}
	res = get(controlled, true, http.StatusOK)
	var skipped work.BenchmarkRunProgress
	if err := json.NewDecoder(res.Body).Decode(&skipped); err != nil || skipped.State != "failed" || skipped.PlanRevision != 1 || len(skipped.Cases) != 1 ||
		skipped.Cases[0].ExecutionControl == nil || skipped.Cases[0].ExecutionControl.State != "skipped_this_run" {
		t.Fatalf("no-model progress missing real manual skip: %+v %v", skipped, err)
	}

	chatTask, err := a.store.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "execution-control-chat-api", Profile: "full", Version: 1}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	chatCode, chat := a.post("/api/v1/conversations", fmt.Sprintf(`{"client_request_id":"execution-chat-status-api","text":"/benchmark-status %s"}`, chatTask))
	if chatCode != http.StatusAccepted {
		t.Fatalf("no chat scope: %d %+v", chatCode, chat)
	}
	messagePath := fmt.Sprintf("/api/v1/conversations/%s/messages", chat["conversation_id"])
	ambiguous := fmt.Sprintf(`{"client_request_id":"execution-chat-request-api","text":"第一项先别跑","benchmark_execution_target":{"task_id":"%s","expected_plan_revision":1}}`, chatTask)
	if code, d := a.post(messagePath, ambiguous); code != http.StatusBadRequest || d["code"] != "benchmark_execution_clarify" {
		t.Fatalf("ambiguous request was not clarified: %d %+v", code, d)
	}
	direct := strings.Replace(ambiguous, "第一项先别跑", "本次跳过第一项", 1)
	var previous any
	for range 2 {
		code, d := a.post(messagePath, direct)
		rec, ok := d["benchmark_execution_control"].(map[string]any)
		if code != http.StatusAccepted || !ok || rec["state"] != "skipped_this_run" || previous != nil && d["run_id"] != previous {
			t.Fatalf("explicit no-model request did not return a stable operation receipt: %d %+v", code, d)
		}
		previous = d["run_id"]
	}
}

func TestBenchmarkChatConfirmDoesNotAcceptClientPlan(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "revision-confirm-api.sqlite"))
	defer a.close()
	path := "/api/v1/conversations/" + strings.Repeat("a", 32) + "/benchmark-revision-preview/" + strings.Repeat("b", 32) + "/confirm"
	const valid = `{"action":"apply_benchmark_revision","expected_revision":1}`
	if status, result := a.post(path, `{"action":"apply_benchmark_revision","expected_revision":1,"cases":[]}`); status != http.StatusBadRequest {
		t.Fatalf("client case payload accepted: %d %+v", status, result)
	}
	if status, result := a.post(path, valid); status != http.StatusNotFound {
		t.Fatalf("unknown server preview ref accepted: %d %+v", status, result)
	}
}

func TestStartBenchmarkDraftHTTP(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "start-draft-http.sqlite"))
	defer a.close()
	const path = "/api/v1/domain/benchmark-profiles/full/draft"
	const body = `{"base_version":1,"expected_revision":0}`
	req := a.request(http.MethodPost, path+"/start-from-published", body)
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous starter: %d", res.StatusCode)
	}
	if code, _ := a.post(path+"/start-from-published", body); code != http.StatusNotFound {
		t.Fatalf("missing template: %d", code)
	}
	profile := `{"key":"full","version":1,"cases":[{"key":"test","binding":{"connection":"bench","revision":1,"tool":"test","validator":"structured-metrics@1"},"targets":["node-1"],"required_metrics":{"rate":"GB/s"}}]}`
	if code, _ := a.post("/api/v1/domain/benchmark-profiles", profile); code != http.StatusCreated {
		t.Fatalf("publish fixture: %d", code)
	}
	if code, _ := a.post(path+"/start-from-published", `{"base_version":1}`); code != http.StatusBadRequest {
		t.Fatalf("missing CAS token: %d", code)
	}
	if code, _ := a.post(path+"/start-from-published", `{"base_version":2,"expected_revision":0}`); code != http.StatusNotFound {
		t.Fatalf("never published template: %d", code)
	}
	if code, out := a.post(path+"/start-from-published", body); code != http.StatusOK || out["published"] != false {
		t.Fatalf("start returned %d %+v", code, out)
	}
	if code, _ := a.post(path+"/start-from-published", body); code != http.StatusConflict {
		t.Fatalf("stale restart: %d", code)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got work.BenchmarkDraft
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != http.StatusOK || got.BaseVersion != 1 || got.Revision != 1 || got.Published {
		t.Fatalf("lost start response not discoverable: %+v %d %v", got, res.StatusCode, err)
	}
}
