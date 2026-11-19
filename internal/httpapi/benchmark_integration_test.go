package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

type localIntegrationBenchmark struct {
	starts  atomic.Int32
	queries atomic.Int32
}

const integrationEvidence = "fixture: measured 128 GB/s via original operation"

func (*localIntegrationBenchmark) Supports(b work.BenchmarkBinding) bool {
	return b.Connection == "bench" && b.Revision == 1 && b.Tool == "test" && b.Validator == "structured-metrics@1"
}
func (f *localIntegrationBenchmark) Start(_ context.Context, r work.BenchmarkJobRequest) (string, error) {
	if r.Case != "first" || len(r.Targets) != 1 || r.Targets[0] != "node-1" || r.IdempotencyKey == "" {
		return "", fmt.Errorf("unexpected fake dispatch: %v", r)
	}
	f.starts.Add(1)
	return "fixture-job", nil
}
func (*localIntegrationBenchmark) Lookup(context.Context, work.BenchmarkJobRequest) (string, error) {
	return "", fmt.Errorf("unexpected uncertain dispatch")
}
func (f *localIntegrationBenchmark) Get(_ context.Context, r work.BenchmarkJobRequest, job string) (work.BenchmarkJobResult, error) {
	f.queries.Add(1)
	if r.Case != "first" || job != "fixture-job" {
		return work.BenchmarkJobResult{}, fmt.Errorf("unrelated fixture query")
	}
	sum := sha256.Sum256([]byte(integrationEvidence))
	return work.BenchmarkJobResult{JobID: job, State: "completed", Measurements: []work.BenchmarkMeasurement{{
		Target: "node-1", Outcome: "measured", Metrics: map[string]work.BenchmarkMeasuredMetric{"rate": {Value: 128, Unit: "GB/s"}},
		Raw: &work.BenchmarkRawArtifact{Ref: "local-evidence", SHA256: fmt.Sprintf("%x", sum), Bytes: []byte(integrationEvidence)},
	}}}, nil
}

func TestLocalBenchmarkChatLifecycleAcrossRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v31-integration.sqlite")
	const (
		admission    = "/api/v1/domain/benchmark-admission/preview"
		runs         = "/api/v1/domain/benchmarks/"
		admitText    = "把 POD7 那一整套基准都安排起来，跑完我再看"
		revisionText = "第二项先不跑，只把第一项留下"
	)
	var modelCalls, profileCalls, executionModelCalls atomic.Int32
	const executionText = "这次不跑集合通信，默认不变"
	t.Setenv("CLUSBOT_HTTP_FIXTURE_REVISION_KEY", "fixture-dedicated-revision-token")
	localModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			http.Error(w, "read fixture", http.StatusBadRequest)
			return
		}
		dedicated := r.Header.Get("Authorization") == "Bearer fixture-dedicated-revision-token"
		if dedicated {
			profileCalls.Add(1)
			if !strings.Contains(string(body), "dedicated-revision") || !strings.Contains(string(body), revisionText) ||
				!strings.Contains(string(body), "case_key") || strings.Contains(string(body), "node-1") ||
				strings.Contains(string(body), "fixture-node") || strings.Contains(string(body), "has_expected_actual_check") {
				t.Error("dedicated revision model received unapproved input")
			}
		}
		var answer string
		execution := strings.Contains(string(body), executionText)
		switch {
		case execution:
			executionModelCalls.Add(1)
			if dedicated || !strings.Contains(string(body), "position") || strings.Contains(string(body), "node-1") || strings.Contains(string(body), "history") {
				t.Error("execution suggestions sent unapproved input or used the wrong model")
			}
			answer = fmt.Sprintf(`{"action":"suggest_skip_this_run","case_key":"second","quote":%q}`, executionText)

		case strings.Contains(string(body), admitText) && strings.Contains(string(body), "benchmark_candidate"):
			answer = `{"kind":"benchmark_preview","benchmark_target":"POD7"}`

		case strings.Contains(string(body), admitText):
			answer = fmt.Sprintf(`{"action":"preview_admission","target":"POD7","profile":"full","version":1,"quote":%q}`, admitText)
		case strings.Contains(string(body), revisionText):
			answer = fmt.Sprintf(`{"action":"remove_case","case_key":"second","quote":%q}`, revisionText)
		default:
			http.Error(w, "unexpected fixture model input", http.StatusBadRequest)
			return
		}
		if !dedicated && !execution {
			modelCalls.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer localModel.Close()
	provider := &localIntegrationBenchmark{}
	start := func(configure bool) *a1Server {
		t.Helper()
		store, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		store.SetConnectionCredentialKey("local-http-fixture-key")
		settings := &model.Settings{Store: store}
		if configure {
			if _, err := settings.Initialize(ctx, work.ModelSettings{ModelID: "fixture", Protocol: model.ProtocolResponses, BaseURL: localModel.URL, KeyRef: "env:CLUSBOT_UNUSED_FIXTURE_KEY"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecoverV3(ctx, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		server := Server{Store: store, Settings: settings, BenchmarkProvider: provider, Token: "a1-token", Hosts: []string{"127.0.0.1"}}
		a := &a1Server{ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("integration")}})), store: store}
		a.cookie = a.login()
		if a.cookie == "" {
			t.Fatal("cannot authenticate to local API")
		}
		return a
	}
	a := start(true)
	defer func() { a.close() }()
	jsonCall := func(method, path string, input, output any, want int) {
		t.Helper()
		var body string
		if input != nil {
			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			body = string(data)
		}
		res, err := a.ts.Client().Do(a.request(method, path, body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		content, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s %s returned %d: %s (%v)", method, path, res.StatusCode, content, err)
		}
		if output != nil {
			if err := json.Unmarshal(content, output); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, content)
			}
		}
	}
	grant := work.DomainBenchmarkGrant{Connection: "bench", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.store.ImportDomainAssets(ctx, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture-node", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	cases := []work.BenchmarkCase{}
	for _, key := range []string{"first", "second"} {
		cases = append(cases, work.BenchmarkCase{Key: key, Targets: []string{"node-1"}, Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"},
			RequiredMetrics: map[string]string{"rate": "GB/s"}, NeedsRawEvidence: true})
	}
	profile := work.BenchmarkProfile{Key: "full", Version: 1, Cases: cases}
	jsonCall(http.MethodPost, "/api/v1/domain/benchmark-profiles", profile, nil, http.StatusCreated)
	jsonCall(http.MethodPut, "/api/v1/domain/benchmark-chat-targets/POD7", work.BenchmarkProfileRef{Profile: "full", Version: 1}, nil, http.StatusNoContent)
	jsonCall(http.MethodPost, admission, work.BenchmarkAdmissionPreviewInput{Text: "给 POD7 跑 40 卡的整套基准"}, nil, http.StatusConflict)
	if modelCalls.Load() != 0 || provider.starts.Load() != 0 {
		t.Fatal("unknown capacity request reached model or provider")
	}

	var suggestion struct {
		ConversationID      string                          `json:"conversation_id"`
		RunID               string                          `json:"run_id"`
		TaskID              string                          `json:"task_id"`
		BenchmarkSuggestion *work.BenchmarkRouterSuggestion `json:"benchmark_suggestion"`
	}
	plain := map[string]string{"client_request_id": "integrated-natural-benchmark", "text": admitText}
	jsonCall(http.MethodPost, "/api/v1/conversations", plain, &suggestion, http.StatusAccepted)
	firstRun := suggestion.RunID
	jsonCall(http.MethodPost, "/api/v1/conversations", plain, &suggestion, http.StatusAccepted)
	if suggestion.RunID == "" || suggestion.RunID != firstRun || suggestion.TaskID != "" || suggestion.BenchmarkSuggestion == nil ||
		suggestion.BenchmarkSuggestion.Target != "POD7" || suggestion.BenchmarkSuggestion.State != "explicit_preview_required" || modelCalls.Load() != 1 {
		t.Fatalf("plain chat dispatched or did not replay the typed suggestion: %+v", suggestion)
	}

	var draft work.BenchmarkAdmissionPreview
	suggestionPath := "/api/v1/conversations/" + suggestion.ConversationID + "/benchmark-suggestions/" + firstRun + "/preview"
	action := work.BenchmarkSuggestionPreviewInput{Action: "preview_benchmark_suggestion"}
	jsonCall(http.MethodPost, suggestionPath, map[string]string{"action": action.Action, "target": "POD8"}, nil, http.StatusBadRequest)
	jsonCall(http.MethodPost, suggestionPath, action, &draft, http.StatusOK)
	var repeated work.BenchmarkAdmissionPreview
	jsonCall(http.MethodPost, suggestionPath, action, &repeated, http.StatusOK)
	if repeated.PreviewID != draft.PreviewID || modelCalls.Load() != 1 || provider.starts.Load() != 0 {
		t.Fatalf("opening the same suggestion generated a new preview or invoked a model/provider: %+v", repeated)
	}
	if draft.PreviewID == "" || draft.State != "preview_only" || draft.ExecutionStarted || draft.Preflight.FacilityStatus != "not_verified" || provider.starts.Load() != 0 {
		t.Fatalf("preview started work or invented facility certainty: %+v", draft)
	}
	admissionFeed := "/api/v1/conversations/" + suggestion.ConversationID + "/events"
	var beforeAccept work.ConversationEventPage
	jsonCall(http.MethodGet, admissionFeed, nil, &beforeAccept, http.StatusOK)
	if len(beforeAccept.Events) < 1 || beforeAccept.Events[len(beforeAccept.Events)-1].Kind != "benchmark.admission_suggested" {
		t.Fatalf("preview incorrectly projected receipt before confirmation: %+v", beforeAccept)
	}

	var accepted work.BenchmarkAdmissionConfirmation
	acceptPath := admission + "/" + draft.PreviewID + "/confirm"
	accept := work.BenchmarkAdmissionConfirmationInput{Action: "accept_benchmark_admission"}
	jsonCall(http.MethodPost, acceptPath, accept, &accepted, http.StatusAccepted)
	taskPath := runs + accepted.TaskID
	var progress work.BenchmarkRunProgress
	jsonCall(http.MethodGet, taskPath, nil, &progress, http.StatusOK)
	if progress.State != "pending" || progress.AssetScope.TargetCount != 1 || len(progress.Cases) != 2 || provider.starts.Load() != 0 {
		t.Fatalf("explicit acceptance did not merely queue: %+v", progress)
	}
	revisionProfilePath := "/api/v1/settings/model/roles/benchmark-revision"
	dedicatedRequest := map[string]any{"expected_revision": 1, "text": revisionText, "use_configured_revision_profile": true}
	jsonCall(http.MethodPost, taskPath+"/revision/ai-preview", dedicatedRequest, nil, http.StatusConflict)
	var revisionProfile model.ProfileView
	profileInput := model.ProfileInput{Name: "专用 fixture", Protocol: model.ProtocolResponses, BaseURL: localModel.URL, ModelID: "dedicated-revision", KeyRef: "env:CLUSBOT_HTTP_FIXTURE_REVISION_KEY"}
	jsonCall(http.MethodPost, "/api/v1/settings/model/profiles", profileInput, &revisionProfile, http.StatusCreated)
	binding := model.BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: revisionProfile.ID, ProfileRevision: 1}
	jsonCall(http.MethodPut, revisionProfilePath, binding, nil, http.StatusBadRequest)
	binding.ConfirmRequestTextEgress = true
	var role model.BenchmarkAdmissionRoleView
	jsonCall(http.MethodPut, revisionProfilePath, binding, &role, http.StatusOK)
	if role.Role != "benchmark_revision" || role.Status != "locally_configured" || len(role.EgressFields) != 3 {
		t.Fatalf("dedicated revision role not bound: %+v", role)
	}
	var standalone work.BenchmarkRevisionProposal
	jsonCall(http.MethodPost, taskPath+"/revision/ai-preview", dedicatedRequest, &standalone, http.StatusOK)
	if standalone.State != "preview_only" || profileCalls.Load() != 1 || modelCalls.Load() != 1 {
		t.Fatalf("opted-in revision did not use exact profile without executing: %+v", standalone)
	}
	dedicatedRequest["allow_expected_actual_change"] = true
	jsonCall(http.MethodPost, taskPath+"/revision/ai-preview", dedicatedRequest, nil, http.StatusBadRequest)
	delete(dedicatedRequest, "allow_expected_actual_change")

	var chat struct {
		ConversationID string `json:"conversation_id"`
	}
	jsonCall(http.MethodPost, "/api/v1/conversations", map[string]string{"client_request_id": "status-for-integrated-revision", "text": "/benchmark-status " + accepted.TaskID}, &chat, http.StatusAccepted)
	chatPath := "/api/v1/conversations/" + chat.ConversationID + "/benchmark-revision-preview"
	messagePath := "/api/v1/conversations/" + chat.ConversationID + "/messages"
	target := work.BenchmarkRevisionTarget{TaskID: accepted.TaskID, ExpectedRevision: 1}
	message := map[string]any{"client_request_id": "integrated-scoped-revision", "text": revisionText, "benchmark_revision_target": target}
	jsonCall(http.MethodPost, messagePath, map[string]any{"client_request_id": "integrated-scoped-query", "text": "可以改第二项吗？", "benchmark_revision_target": target}, nil, http.StatusBadRequest)
	jsonCall(http.MethodPost, messagePath, map[string]any{"client_request_id": "integrated-scoped-query", "text": revisionText, "benchmark_revision_target": work.BenchmarkRevisionTarget{TaskID: accepted.TaskID, ExpectedRevision: 2}}, nil, http.StatusConflict)
	var saved struct {
		RunID      string                        `json:"run_id"`
		TaskID     string                        `json:"task_id"`
		Suggestion *work.BenchmarkRevisionTarget `json:"benchmark_revision_suggestion"`
	}
	jsonCall(http.MethodPost, messagePath, message, &saved, http.StatusAccepted)
	if saved.RunID == "" || saved.TaskID != "" || saved.Suggestion == nil || *saved.Suggestion != target || modelCalls.Load() != 1 {
		t.Fatalf("scoped message did not persist a read-only labelled suggestion: %+v", saved)
	}
	intent := work.BenchmarkRevisionChatInput{Action: "preview_benchmark_revision", SourceRunID: saved.RunID, RequestID: "integration-revision-001"}
	jsonCall(http.MethodPost, chatPath, map[string]any{"action": intent.Action, "source_run_id": saved.RunID, "client_request_id": intent.RequestID, "task_id": accepted.TaskID}, nil, http.StatusBadRequest)
	jsonCall(http.MethodPost, "/api/v1/conversations/"+suggestion.ConversationID+"/benchmark-revision-preview", intent, nil, http.StatusNotFound)
	conversationFeed := "/api/v1/conversations/" + chat.ConversationID + "/events"
	var beforeFeed work.ConversationEventPage
	jsonCall(http.MethodGet, conversationFeed, nil, &beforeFeed, http.StatusOK)
	if len(beforeFeed.Events) == 0 || beforeFeed.Events[len(beforeFeed.Events)-1].Kind != "benchmark.revision_suggested" || beforeFeed.Events[len(beforeFeed.Events)-1].RunID != saved.RunID {
		t.Fatalf("reconnect did not read the labelled revision: %+v", beforeFeed)
	}
	jsonCall(http.MethodGet, "/api/v1/conversations/"+suggestion.ConversationID+"/events?cursor="+beforeFeed.Cursor, nil, nil, http.StatusConflict)

	var pending work.BenchmarkRevisionChatReceipt
	jsonCall(http.MethodPost, chatPath, intent, &pending, http.StatusOK)
	if pending.Proposal.State != "preview_only" || pending.Proposal.Preview.AssetImpact == nil || pending.SourceRunID != saved.RunID || profileCalls.Load() != 1 {
		t.Fatalf("missing actionable but non-applied preview: %+v", pending)
	}
	jsonCall(http.MethodGet, taskPath, nil, &progress, http.StatusOK)
	if progress.PlanRevision != 1 || len(progress.Cases) != 2 {
		t.Fatalf("AI revision changed live plan without confirmation: %+v", progress)
	}

	// Restart the actual SQLite owner before explicit revision confirmation.
	a.close()
	a = start(false)
	jsonCall(http.MethodPost, "/api/v1/conversations", plain, &suggestion, http.StatusAccepted)
	if suggestion.RunID != firstRun || suggestion.BenchmarkSuggestion == nil || modelCalls.Load() != 2 {
		t.Fatalf("router suggestion did not survive restart without a model replay: %+v", suggestion)
	}

	jsonCall(http.MethodPost, suggestionPath, action, &repeated, http.StatusOK)
	if repeated.PreviewID != draft.PreviewID || modelCalls.Load() != 2 {
		t.Fatalf("preview did not survive restart without a model replay: %+v", repeated)
	}
	var acceptedAgain work.BenchmarkAdmissionConfirmation
	jsonCall(http.MethodPost, acceptPath, accept, &acceptedAgain, http.StatusAccepted)
	if acceptedAgain.TaskID != accepted.TaskID || provider.starts.Load() != 0 {
		t.Fatalf("confirmation replay queued another task or started execution: %+v", acceptedAgain)
	}

	savedRun := saved.RunID
	jsonCall(http.MethodPost, messagePath, message, &saved, http.StatusAccepted)
	if saved.RunID != savedRun || saved.Suggestion == nil || *saved.Suggestion != target {
		t.Fatalf("revision suggestion binding was lost on restart: %+v", saved)
	}
	var pendingAgain work.BenchmarkRevisionChatReceipt
	jsonCall(http.MethodPost, chatPath, intent, &pendingAgain, http.StatusOK)
	if pendingAgain.RunID != pending.RunID || modelCalls.Load() != 2 {
		t.Fatalf("the same explicit click replayed model or generated a new preview: %+v", pendingAgain)
	}

	var afterFeed work.ConversationEventPage
	jsonCall(http.MethodGet, conversationFeed+"?cursor="+beforeFeed.Cursor, nil, &afterFeed, http.StatusOK)
	if len(afterFeed.Events) != 1 || afterFeed.Events[0].Kind != "benchmark.revision_preview" || afterFeed.Events[0].RunID != pending.RunID {
		t.Fatalf("reconnect did not catch the real persisted preview event: %+v", afterFeed)
	}

	var queuedFeed work.ConversationEventPage
	jsonCall(http.MethodGet, admissionFeed+"?cursor="+beforeAccept.Cursor, nil, &queuedFeed, http.StatusOK)
	if len(queuedFeed.Events) != 1 || queuedFeed.Events[0].Kind != "benchmark.admission_queued" ||
		queuedFeed.Events[0].RunID != firstRun || queuedFeed.Events[0].TaskID != accepted.TaskID || queuedFeed.Events[0].Status != "queued" {
		t.Fatalf("original conversation cannot recover the verified queued task after restart: %+v", queuedFeed)
	}
	jsonCall(http.MethodGet, conversationFeed+"?cursor="+queuedFeed.Cursor, nil, nil, http.StatusConflict)

	var reopened work.BenchmarkRevisionChatReceipt
	jsonCall(http.MethodGet, chatPath+"/"+pending.RunID, nil, &reopened, http.StatusOK)
	if reopened.Proposal.Preview.AssetImpact == nil || modelCalls.Load() != 2 {
		t.Fatalf("reopen failed or started model again: %+v", reopened)
	}
	t.Setenv("CLUSBOT_HTTP_FIXTURE_REVISION_KEY", "rotated-fixture-revision-key")
	jsonCall(http.MethodPost, taskPath+"/revision/ai-preview", dedicatedRequest, nil, http.StatusConflict)
	if modelCalls.Load() != 2 || profileCalls.Load() != 1 {
		t.Fatal("credential drift sent to a model or silently fell back")
	}

	confirmPath := chatPath + "/" + pending.RunID + "/confirm"
	confirm := work.BenchmarkRevisionChatConfirmInput{Action: "apply_benchmark_revision", ExpectedRevision: 1}
	var revision work.BenchmarkRevisionChatConfirmation
	jsonCall(http.MethodPost, confirmPath, confirm, &revision, http.StatusOK)
	if revision.Revision != 2 || provider.starts.Load() != 0 {
		t.Fatalf("revision confirmation unexpectedly dispatched: %+v", revision)
	}

	var confirmedFeed work.ConversationEventPage
	var replayedConfirmation work.BenchmarkRevisionChatConfirmation
	jsonCall(http.MethodPost, confirmPath, confirm, &replayedConfirmation, http.StatusOK)
	jsonCall(http.MethodGet, conversationFeed+"?cursor="+afterFeed.Cursor, nil, &confirmedFeed, http.StatusOK)
	if len(confirmedFeed.Events) != 1 || confirmedFeed.Events[0].Kind != "benchmark.revision_applied" ||
		confirmedFeed.Events[0].RunID != pending.RunID || confirmedFeed.Events[0].Revision != revision.Revision {
		t.Fatalf("the same conversation cannot recover the real confirmation exactly once: %+v", confirmedFeed)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.store.RunBenchmarks(workerCtx, provider) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(8 * time.Second)
	for {
		jsonCall(http.MethodGet, taskPath, nil, &progress, http.StatusOK)
		if progress.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local runtime did not finish: %+v", progress)
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-done
	if progress.PlanRevision != 2 || len(progress.Cases) != 1 || progress.Cases[0].Key != "first" ||
		provider.starts.Load() != 1 || provider.queries.Load() != 1 {
		t.Fatalf("wrong final scope or duplicate dispatch: %+v / %d starts", progress, provider.starts.Load())
	}

	// A second restart must not recreate either the accepted task or revision.
	a.close()
	a = start(false)
	var replay work.BenchmarkAdmissionConfirmation
	jsonCall(http.MethodPost, acceptPath, accept, &replay, http.StatusAccepted)
	if replay.TaskID != accepted.TaskID {
		t.Fatalf("admission replay created a new task: %+v", replay)
	}
	jsonCall(http.MethodPost, confirmPath, confirm, &revision, http.StatusOK)
	if revision.Revision != 2 || modelCalls.Load() != 2 {
		t.Fatalf("revision replay changed revision or spent model again: %+v", revision)
	}
	// Let production scheduler tick again on the reopened DB, rather
	// than treating API receipt reuse alone as proof of no issued work.
	restartCtx, stopRestart := context.WithTimeout(ctx, 1200*time.Millisecond)
	a.store.RunBenchmarks(restartCtx, provider)
	stopRestart()
	if provider.starts.Load() != 1 || provider.queries.Load() != 1 {
		t.Fatal("restart reissued a settled operation")
	}

	var report work.BenchmarkReport
	var evidence work.BenchmarkEvidencePage
	jsonCall(http.MethodGet, taskPath+"/report", nil, &report, http.StatusOK)
	jsonCall(http.MethodGet, taskPath+"/evidence", nil, &evidence, http.StatusOK)
	if report.AssetAcceptance != "not_evaluated" || len(report.Cases) != 1 || report.Cases[0].RequiredEvidence != "covered" ||
		len(evidence.Items) != 1 || !evidence.Items[0].AssessmentLinked {
		t.Fatalf("untrusted evidence or asset acceptance: %+v %+v", report, evidence)
	}
	download, err := a.ts.Client().Do(a.request(http.MethodGet, taskPath+"/evidence/"+evidence.Items[0].ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	raw, err := io.ReadAll(download.Body)
	if err != nil || download.StatusCode != http.StatusOK || string(raw) != integrationEvidence {
		t.Fatalf("raw evidence changed or missing: %q %d %v", raw, download.StatusCode, err)
	}
	// The dedicated language interpretation is optional and only makes a
	// proposal; a later explicit action owns the actual execution change.
	suggestTask, err := a.store.SubmitBenchmarkRun(ctx, "user:session", work.BenchmarkRunRequest{RequestKey: "execution-natural-task", Profile: "full", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	suggestTaskPath := runs + suggestTask
	natural := map[string]any{
		"client_request_id": "execution-natural-proposal", "text": executionText, "interpret_benchmark_execution": true,
		"benchmark_execution_target": work.BenchmarkExecutionTarget{TaskID: suggestTask, ExpectedPlanRevision: 1},
	}
	var interpreted struct {
		RunID      string                             `json:"run_id"`
		Suggestion *work.BenchmarkExecutionSuggestion `json:"benchmark_execution_suggestion"`
	}
	jsonCall(http.MethodPost, messagePath, natural, &interpreted, http.StatusAccepted)
	if interpreted.Suggestion == nil || interpreted.Suggestion.CaseKey != "second" || interpreted.Suggestion.State != "confirmation_required" || executionModelCalls.Load() != 1 {
		t.Fatalf("natural message did not return a bounded suggestion: %+v", interpreted)
	}
	jsonCall(http.MethodGet, suggestTaskPath, nil, &progress, http.StatusOK)
	if progress.PlanRevision != 1 || len(progress.Cases) != 2 || progress.Cases[1].Phase != "queued" {
		t.Fatalf("model suggestion silently applied control: %+v", progress)
	}
	var beforeControl work.ConversationEventPage
	jsonCall(http.MethodGet, conversationFeed, nil, &beforeControl, http.StatusOK)
	last := beforeControl.Events[len(beforeControl.Events)-1]
	if last.Kind != "benchmark.execution_suggested" || last.RunID != interpreted.RunID || last.Status != "confirmation_required" {
		t.Fatalf("suggestion unavailable to reconnect: %+v", last)
	}
	a.close()
	a = start(false)
	suggestRun := interpreted.RunID
	jsonCall(http.MethodPost, messagePath, natural, &interpreted, http.StatusAccepted)
	if interpreted.RunID != suggestRun || executionModelCalls.Load() != 1 {
		t.Fatalf("restart repeated model call: %+v", interpreted)
	}
	actionPath := "/benchmark-execution-suggestions/" + suggestRun + "/confirm"
	confirmControl := map[string]string{"action": "skip_this_run"}
	jsonCall(http.MethodPost, "/api/v1/conversations/"+suggestion.ConversationID+actionPath, confirmControl, nil, http.StatusNotFound)
	var applied work.BenchmarkExecutionControlReceipt
	for range 2 {
		jsonCall(http.MethodPost, "/api/v1/conversations/"+chat.ConversationID+actionPath, confirmControl, &applied, http.StatusOK)
	}
	var later work.ConversationEventPage
	jsonCall(http.MethodGet, conversationFeed+"?cursor="+beforeControl.Cursor, nil, &later, http.StatusOK)
	if len(later.Events) != 1 || later.Events[0].Kind != "benchmark.execution_skipped" || later.Events[0].StepID != applied.StepID || provider.starts.Load() != 1 || executionModelCalls.Load() != 1 {
		t.Fatalf("repeat confirmed twice, reinterpreted, or dispatched: %+v", later)
	}

	// This candidate is still waiting, but a bound profile may not be
	// contacted after its credential changed, nor may the default replace it.
	const execRolePath = "/api/v1/settings/model/roles/benchmark-execution"
	var execRole model.BenchmarkAdmissionRoleView
	jsonCall(http.MethodPut, execRolePath, binding, &execRole, http.StatusOK)
	if execRole.Role != "benchmark_execution" || execRole.Status != "locally_configured" {
		t.Fatalf("execution role isn't independently available: %+v", execRole)
	}
	t.Setenv("CLUSBOT_HTTP_FIXTURE_REVISION_KEY", "must-not-be-used")
	natural["client_request_id"] = "unavailable-execution-model"
	for range 2 {
		var unavailable map[string]string
		jsonCall(http.MethodPost, messagePath, natural, &unavailable, http.StatusConflict)
		if unavailable["code"] != "benchmark_execution_model_binding_unavailable" {
			t.Fatalf("unavailable model not actionable: %+v", unavailable)
		}
	}
	t.Setenv("CLUSBOT_HTTP_FIXTURE_REVISION_KEY", "rotated-fixture-revision-key")
	var lost map[string]string
	jsonCall(http.MethodPost, messagePath, natural, &lost, http.StatusConflict)
	if lost["code"] != "benchmark_execution_model_binding_unavailable" {
		t.Fatalf("restoring the profile reattempted the old request: %+v", lost)
	}
	if executionModelCalls.Load() != 1 || profileCalls.Load() != 1 || provider.starts.Load() != 1 {
		t.Fatal("credential change contacted a model or dispatched")
	}

}
