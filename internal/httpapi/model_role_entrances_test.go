package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

type roleEntrancesBenchmark struct {
	localIntegrationBenchmark
	allowResult chan struct{}
}

func (p *roleEntrancesBenchmark) Get(ctx context.Context, request work.BenchmarkJobRequest, job string) (work.BenchmarkJobResult, error) {
	select {
	case <-p.allowResult:
		return p.localIntegrationBenchmark.Get(ctx, request, job)
	case <-ctx.Done():
		return work.BenchmarkJobResult{}, ctx.Err()
	}
}

func TestModelRoleEntrancesRouteChatButNotTaskFollowupOrProviderReceipt(t *testing.T) {
	const env = "CLUSBOT_MODEL_ENTRANCES_FIXTURE"
	const credential = "env:" + env
	const eventSecret = "test-only benchmark event secret >=32"
	t.Setenv(env, "local-only-model-fixture")
	modelCalls := make(chan string, 12)
	localModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model == "" {
			t.Errorf("invalid local model request: %+v %v", input, err)
			return
		}
		modelCalls <- input.Model
		answer := "本机模拟回复"
		if input.Model == "router" {
			answer = `{"kind":"chat"}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer localModel.Close()
	path := filepath.Join(t.TempDir(), "role-entrances.sqlite")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: store}
	ctx := t.Context()
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: localModel.URL, ModelID: "worker-default", Protocol: model.ProtocolResponses, KeyRef: credential}); err != nil {
		t.Fatal(err)
	}
	save := func(id string) model.ProfileView {
		t.Helper()
		p, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: id, ModelID: id, BaseURL: localModel.URL, Protocol: model.ProtocolResponses, KeyRef: credential})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	router, reply := save("router"), save("reply")
	if _, err := settings.SetGeneralRouterRole(ctx, model.BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: router.ID, ProfileRevision: 1, ConfirmRequestTextEgress: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.SetConversationReplyRole(ctx, model.ConversationReplyRoleInput{Action: "bind_profile", ProfileID: reply.ID, ProfileRevision: 1, ConfirmConversationContextEgress: true}); err != nil {
		t.Fatal(err)
	}

	// The receiver uses the real production decoder and verifies a signed
	// source/job hint. The benchmark executor itself is an existing local fake.
	httpProvider, err := work.NewHTTPBenchmarkProvider("http://127.0.0.1:1", "bench", 1, []string{"test"}, "unused-fixture")
	if err != nil {
		t.Fatal(err)
	}
	events, err := httpProvider.EventReceiver(eventSecret)
	if err != nil {
		t.Fatal(err)
	}
	provider := &roleEntrancesBenchmark{allowResult: make(chan struct{})}
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	api := &a1Server{ts: httptest.NewServer(businessFixtureHandler(t, server)), store: store}
	defer api.close()
	api.cookie = api.login()
	if api.cookie == "" {
		t.Fatal("fixture login failed")
	}
	runCtx, cancel := context.WithCancel(ctx)
	workers := store.Start(runCtx, settings, 1, work.RunLimits{ActiveTime: 30 * time.Second, ModelTimeout: 3 * time.Second, OutputTokens: 768})
	defer func() { cancel(); workers.Wait() }()

	post := func(path, key, text string) (run, task, room string) {
		t.Helper()
		code, receipt := api.post(path, fmt.Sprintf(`{"client_request_id":%q,"text":%q}`, key, text))
		if code != http.StatusAccepted {
			t.Fatalf("submit %s: %d %+v", path, code, receipt)
		}
		run, _ = receipt["run_id"].(string)
		task, _ = receipt["task_id"].(string)
		room, _ = receipt["conversation_id"].(string)
		if run == "" || room == "" {
			t.Fatalf("empty run receipt: %+v", receipt)
		}
		return run, task, room
	}
	complete := func(run string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			_, r, err := store.Events(run, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch r.Status {
			case "completed":
				return
			case "failed", "interrupted":
				t.Fatalf("run %s ended: %+v", run, r)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("run not completed", run)
	}
	r, task, _ := post("/api/v1/conversations", "ordinary-role-entry", "解释这个概念")
	if task != "" {
		t.Fatal("ordinary question created a task", task)
	}
	complete(r)
	r, task, room := post("/api/v1/tasks", "existing-task-entry", "写一份简短项目说明")
	if task == "" {
		t.Fatal("fixture task did not establish a task-bound conversation")
	}
	complete(r)
	r, nextTask, nextRoom := post("/api/v1/conversations/"+room+"/messages", "bound-followup-entry", "补充：加入本周交付结果")
	if nextTask != task || nextRoom != room {
		t.Fatalf("follow-up lost original task %s/%s: %s/%s", task, room, nextTask, nextRoom)
	}
	complete(r)
	want := []string{"router", "reply", "worker-default", "worker-default"}
	got := []string{}
	drain := func() {
		for {
			select {
			case v := <-modelCalls:
				got = append(got, v)
			default:
				return
			}
		}
	}
	drain()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong model per user entry: got %v want %v", got, want)
	}

	// The direct signed provider hook is not an account router and must not
	// write to the authenticated business fixture. Use its supported isolated
	// store while retaining the same model observer. Account-routed receipts
	// are covered by the separate two-account runtime integration test.
	receiptPath := filepath.Join(t.TempDir(), "provider-receipts.sqlite")
	receiptStore, err := work.Open(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	defer receiptStore.Close()
	receiptServer := Server{Store: receiptStore, Settings: settings, BenchmarkEvents: events}
	receiptMux := http.NewServeMux()
	receiptMux.HandleFunc("POST /api/v1/provider/benchmark-events", receiptServer.benchmarkEvent)
	receiptEndpoint := httptest.NewServer(receiptMux)
	defer receiptEndpoint.Close()
	const actor = "user:session"
	binding := work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}
	if err := receiptStore.SeedAssetForTest("node-1", work.AssetStateRecheck); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	grantTime := time.Now()
	if _, err = db.ExecContext(ctx, `INSERT INTO authorizations
(auth_id,subject,action,object_ref,param_constraints,valid_from,expires_at,owner,created_at,updated_at)
VALUES('fixture-benchmark-grant',?,'benchmark.execute','node-1','{"revision":1,"connection":"bench","tool":"test"}',?,?,?,?,?)`,
		actor, grantTime.UnixMilli(), grantTime.Add(time.Minute).UnixMilli(), actor, grantTime.UnixMilli(), grantTime.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := receiptStore.PublishBenchmarkProfile(ctx, actor, work.BenchmarkProfile{Key: "entrances", Version: 1, Cases: []work.BenchmarkCase{{Key: "first", Targets: []string{"node-1"}, Binding: binding, RequiredMetrics: map[string]string{"rate": "GB/s"}, NeedsRawEvidence: true}}}); err != nil {
		t.Fatal(err)
	}
	bench, err := receiptStore.SubmitBenchmarkRun(ctx, actor, work.BenchmarkRunRequest{RequestKey: "existing-benchmark-entrance", Profile: "entrances", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	benchDone := make(chan struct{})
	go func() { defer close(benchDone); receiptStore.RunBenchmarks(runCtx, provider) }()
	defer func() { cancel(); <-benchDone }()
	readState := func(want string) work.BenchmarkRunProgress {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			out, err := receiptStore.BenchmarkProgress(ctx, actor, bench)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Cases) == 1 && out.Cases[0].Phase == want {
				return out
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("benchmark did not reach %s", want)
		return work.BenchmarkRunProgress{}
	}
	before := readState("following")
	const eventID = "fixture-matched-job"
	const payload = `{"event_id":"fixture-matched-job","job_id":"fixture-job"}`
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(eventSecret))
	mac.Write([]byte(stamp + "\n" + payload))
	deliver := func(wantDuplicate bool) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, receiptEndpoint.URL+"/api/v1/provider/benchmark-events", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1"
		req.Header.Set("X-ClusBot-Timestamp", stamp)
		req.Header.Set("X-ClusBot-Signature", fmt.Sprintf("%x", mac.Sum(nil)))
		res, err := receiptEndpoint.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]bool
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusAccepted || out["duplicate"] != wantDuplicate {
			t.Fatalf("receipt did not preserve deduplication: status=%d %+v %v", res.StatusCode, out, err)
		}
	}
	deliver(false)
	deliver(true)
	close(provider.allowResult)
	after := readState("verified")
	// Benchmark state alone would also advance on ordinary polling. Check the
	// actual persisted source event was consumed by its bound job matcher.
	var state string
	deadline := time.Now().Add(4 * time.Second)
	for state != "applied" && time.Now().Before(deadline) {
		if err := db.QueryRowContext(ctx, "SELECT state FROM inbox_events WHERE source=? AND event_id=?", "benchmark-provider:bench:1", eventID).Scan(&state); err != nil {
			t.Fatalf("read event state: %v", err)
		}
		if state != "applied" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if state != "applied" {
		t.Fatalf("did not consume matched source event: %q", state)
	}
	if after.TaskID != before.TaskID || after.Cases[0].Operation == nil || before.Cases[0].Operation == nil ||
		after.Cases[0].Operation.ID != before.Cases[0].Operation.ID || provider.starts.Load() != 1 || provider.queries.Load() != 1 {
		t.Fatalf("receipt restarted or detached the original operation: before=%+v after=%+v starts=%d queries=%d", before, after, provider.starts.Load(), provider.queries.Load())
	}
	drain()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider receipt went through a model role: got %v want %v", got, want)
	}
}
