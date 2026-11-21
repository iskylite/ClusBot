package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func routingFixture(t *testing.T, firstURL, secondURL string) (*work.Store, *Settings, ProfileView, ProfileView) {
	t.Helper()
	store, err := work.Open(filepath.Join(t.TempDir(), "routing-policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.SetConnectionCredentialKey("test-routing-persistent-key")
	s := &Settings{Store: store}
	if _, err = s.Initialize(t.Context(), work.ModelSettings{Protocol: ProtocolResponses, BaseURL: firstURL, ModelID: "legacy", KeyRef: "env:ROUTING_UNUSED"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.SaveProfile(t.Context(), "", ProfileInput{Name: "first", Protocol: ProtocolResponses, BaseURL: firstURL, ModelID: "first-model", KeyRef: "none"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveProfile(t.Context(), "", ProfileInput{Name: "second", Protocol: ProtocolChat, BaseURL: secondURL, ModelID: "second-model", KeyRef: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return store, s, first, second
}

func routingInput(revision int, profiles ...ProfileView) RoutingPolicyInput {
	in := RoutingPolicyInput{Revision: revision, AllowCrossProvider: true, ConfirmEgress: true}
	for _, p := range profiles {
		in.Candidates = append(in.Candidates, RoutingCandidateInput{ProfileID: p.ID, ProfileRevision: p.Revision})
	}
	return in
}

func TestRoutingPolicyPrecedencePersistenceAndValidation(t *testing.T) {
	store, s, first, second := routingFixture(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:2/v1")
	ctx := t.Context()
	input := routingInput(0, first, second)
	input.ConfirmEgress = false
	if _, err := s.SaveModelRoutingPolicy(ctx, "default", work.ConversationReplyModelRole, input); err == nil {
		t.Fatal("accepted unconfirmed egress")
	}
	input.ConfirmEgress, input.AllowCrossProvider = true, false
	if _, err := s.SaveModelRoutingPolicy(ctx, "default", work.ConversationReplyModelRole, input); err == nil {
		t.Fatal("accepted unapproved cross-provider chain")
	}
	input.AllowCrossProvider = true
	global, err := s.SaveModelRoutingPolicy(ctx, "default", work.ConversationReplyModelRole, input)
	if err != nil || global.Revision != 1 || len(global.Candidates) != 2 {
		t.Fatalf("save: %+v %v", global, err)
	}
	if _, err = s.SaveModelRoutingPolicy(ctx, "default", work.ConversationReplyModelRole, input); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("stale update: %v", err)
	}
	preview, err := s.PreviewModelRouting(ctx, work.ConversationReplyModelRole)
	if err != nil || preview.Source != "role_policy" || preview.ModelID != "first-model" || len(preview.Candidates) != 2 {
		t.Fatalf("default policy preview: %+v %v", preview, err)
	}
	override, err := s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, routingInput(0, second))
	if err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewModelRouting(ctx, work.ConversationReplyModelRole)
	if err != nil || preview.Source != "business_override" || preview.ModelID != "second-model" || preview.PolicyRevision != override.Revision {
		t.Fatalf("override precedence: %+v %v", preview, err)
	}
	if _, err = s.PreviewModelRoutingForBusiness(ctx, "benchmark", work.ConversationReplyModelRole); err == nil {
		t.Fatal("unsupported scope was presented as executable")
	}
	if _, err = s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, RoutingPolicyInput{Revision: override.Revision}); err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewModelRouting(ctx, work.ConversationReplyModelRole)
	if err != nil || preview.Source != "role_policy" || preview.ModelID != "first-model" {
		t.Fatalf("clear override: %+v %v", preview, err)
	}
	if _, err = s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, routingInput(override.Revision, second)); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("cleared layer accepted stale update: %v", err)
	}
	policies, err := store.ModelRoutingPolicies(ctx)
	if err != nil || len(policies) != 2 || len(policies[0].Candidates) != 0 || len(policies[1].Candidates) != 2 {
		t.Fatalf("policy store: %+v %v", policies, err)
	}
	body, _ := json.Marshal(preview)
	if strings.Contains(string(body), "credential_binding") || strings.Contains(string(body), "ROUTING_UNUSED") {
		t.Fatalf("secret metadata leaked: %s", body)
	}
}

func TestRoutingPolicyFallbackExecutesOnlySafeFailureCases(t *testing.T) {
	var status atomic.Int32
	var firstCalls, secondCalls atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		switch status.Load() {
		case 200:
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n") // interrupted stream
		default:
			w.WriteHeader(int(status.Load()))
		}
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong fallback protocol: %s", r.URL.Path)
		}
		fmt.Fprint(w, "data: {\"model\":\"vendor/actual\",\"choices\":[{\"delta\":{\"content\":\"fallback answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer secondServer.Close()
	_, s, first, second := routingFixture(t, firstServer.URL+"/v1", secondServer.URL+"/v1")
	ctx := t.Context()
	if _, err := s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, routingInput(0, first, second)); err != nil {
		t.Fatal(err)
	}
	p, _, selected, err := s.routingProvider(ctx, "conversation", work.ConversationReplyModelRole)
	if err != nil || !selected {
		t.Fatalf("select: %v", err)
	}
	request := responseRequest{Input: []any{responseMessage{Type: "message", Role: "user", Content: "hello"}}, MaxOutputTokens: 100}
	status.Store(429)
	var text strings.Builder
	result, err := p.requestStream(ctx, request, func(s string) error { text.WriteString(s); return nil })
	if err != nil || text.String() != "fallback answer" || firstCalls.Load() != 1 || secondCalls.Load() != 1 || result.actualRequested != "second-model" || result.fallbackReason != "rate_limited" || result.actualReceipt == nil || result.actualReceipt.Protocol != ProtocolChat || result.actualReceipt.ReportedModel != "vendor/actual" {
		t.Fatalf("fallback receipt: %+v text=%q calls=%d/%d err=%v", result, text.String(), firstCalls.Load(), secondCalls.Load(), err)
	}
	status.Store(401)
	if _, err = p.requestStream(ctx, request, nil); err == nil || secondCalls.Load() != 1 {
		t.Fatalf("authentication failure fell back: %v", err)
	}
	status.Store(200)
	text.Reset()
	if _, err = p.requestStream(ctx, request, func(s string) error { text.WriteString(s); return nil }); err == nil || text.String() != "partial" || secondCalls.Load() != 1 {
		t.Fatalf("partial stream fell back: %q %v", text.String(), err)
	}
	status.Store(503)
	toolRequest := request
	toolRequest.Tools = []responseFunction{{Type: "function", Name: "read"}}
	if _, err = p.requestStream(ctx, toolRequest, nil); err == nil || secondCalls.Load() != 1 {
		t.Fatalf("tool request fell back: %v", err)
	}
	// Editing a fallback profile invalidates its exact pinned revision. It does
	// not silently skip or reroute to a lower policy layer.
	if _, err = s.SaveProfile(ctx, second.ID, ProfileInput{Name: "second", Protocol: ProtocolChat, BaseURL: secondServer.URL + "/v1", ModelID: "changed", KeyRef: "none", Revision: second.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.requestStream(ctx, request, nil); err == nil || secondCalls.Load() != 1 {
		t.Fatalf("stale fallback was used: %v", err)
	}
	// Policy edits revoke future attempts even for an already resolved run.
	if _, err = s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, RoutingPolicyInput{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	before := firstCalls.Load()
	if _, err = p.requestStream(ctx, request, nil); err == nil || firstCalls.Load() != before {
		t.Fatalf("revoked policy still dispatched: %v", err)
	}
}

func TestRoutingPolicyRunPersistsAttemptsReceiptsAndBudget(t *testing.T) {
	attemptedOutput := make(chan int, 2)
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		attemptedOutput <- body.MaxOutputTokens
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxOutputTokens int `json:"max_completion_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		attemptedOutput <- body.MaxOutputTokens
		fmt.Fprint(w, "data: {\"model\":\"vendor/actual\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer secondServer.Close()
	store, s, first, second := routingFixture(t, firstServer.URL, secondServer.URL)
	ctx := t.Context()
	if _, err := s.SaveModelRoutingPolicy(ctx, "conversation", work.ConversationReplyModelRole, routingInput(0, first, second)); err != nil {
		t.Fatal(err)
	}
	run, err := store.Submit(ctx, "fallback-run", "请回答", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	workers := store.Start(runCtx, s, 1, work.RunLimits{OutputTokens: 128, ActiveTime: 10 * time.Second, ModelTimeout: 5 * time.Second})
	defer func() { cancel(); workers.Wait() }()
	done := waitConversationReplyRun(t, store, run.ID)
	if done.Status != "completed" || done.Response != "ok" {
		t.Fatalf("run did not fall back: %+v", done)
	}
	record, err := store.RunRecord(ctx, run.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.Usage == nil || record.Usage.ModelRequests != 2 || record.Usage.UnreportedRequests != 2 {
		t.Fatalf("attempt accounting: %+v", record.Usage)
	}
	if len(attemptedOutput) != 2 {
		t.Fatalf("missing budgeted attempts: %d", len(attemptedOutput))
	}
	firstLimit, secondLimit := <-attemptedOutput, <-attemptedOutput
	if firstLimit != secondLimit || firstLimit+secondLimit > 128 {
		t.Fatalf("output was not budgeted across attempts: %d, %d", firstLimit, secondLimit)
	}
	if len(record.Contexts) != 2 {
		t.Fatalf("missing attempt manifests: %+v", record.Contexts)
	}
	var latest struct {
		Requested string `json:"requested_model"`
		Reason    string `json:"fallback_reason"`
		Attempt   int    `json:"routing_attempt"`
		Role      string `json:"model_role"`
	}
	if err := json.Unmarshal(record.Contexts[0].Manifest, &latest); err != nil || latest.Requested != "second-model" || latest.Reason != "rate_limited" || latest.Attempt != 2 || latest.Role != work.ConversationReplyModelRole {
		t.Fatalf("fallback manifest: %+v %v", latest, err)
	}
	if receipt := record.Contexts[0].ModelReceipt; receipt == nil || receipt.ReportedModel != "vendor/actual" || receipt.Protocol != ProtocolChat {
		t.Fatalf("actual receipt missing: %+v", receipt)
	}
	if record.Contexts[1].ModelReceipt != nil {
		t.Fatalf("failed primary invented receipt: %+v", record.Contexts[1].ModelReceipt)
	}
}

func TestCoreRoutingAvailableRequiresEveryCoreStage(t *testing.T) {
	store, s, first, _ := routingFixture(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:2/v1")
	ctx := t.Context()
	settings, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.ModelID = ""
	if _, err = store.SaveModelSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if s.CoreRoutingAvailable(ctx) {
		t.Fatal("missing phases reported available")
	}
	for _, pair := range [][2]string{{"conversation", work.GeneralRouterModelRole}, {"conversation", work.ConversationReplyModelRole}, {"task", work.TaskWorkerModelRole}} {
		if _, err := s.SaveModelRoutingPolicy(ctx, pair[0], pair[1], routingInput(0, first)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.CoreRoutingAvailable(ctx) {
		t.Fatal("fully configured core routes not available")
	}
	if _, err := s.SaveModelRoutingPolicy(ctx, "task", work.TaskWorkerModelRole, RoutingPolicyInput{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if s.CoreRoutingAvailable(ctx) {
		t.Fatal("missing worker treated as available")
	}
}

func TestRoutingPoliciesSurviveReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "persist.db")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetConnectionCredentialKey("persistent-test-key")
	s := &Settings{Store: store}
	if _, err = s.Initialize(ctx, work.ModelSettings{Protocol: ProtocolResponses, BaseURL: "http://127.0.0.1:1", ModelID: "default", KeyRef: "env:NOT_USED"}); err != nil {
		t.Fatal(err)
	}
	profile, err := s.SaveProfile(ctx, "", ProfileInput{Name: "persistent", Protocol: ProtocolResponses, BaseURL: "http://127.0.0.1:1", ModelID: "saved-model", KeyRef: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveModelRoutingPolicy(ctx, "task", work.TaskWorkerModelRole, routingInput(0, profile)); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetConnectionCredentialKey("persistent-test-key")
	s.Store = store
	out, err := s.PreviewModelRouting(ctx, work.TaskWorkerModelRole)
	if err != nil || out.Source != "business_override" || out.Status != "locally_configured" || out.ModelID != "saved-model" {
		t.Fatalf("reopened policy: %+v %v", out, err)
	}
}

func TestRoutingPolicyRouterAndBenchmarkStageExecutions(t *testing.T) {
	var routerMode atomic.Bool
	var calls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		content := `{"action":"clarify"}`
		if routerMode.Load() {
			content = `{"kind":"chat"}`
		}
		fmt.Fprintf(w, "data: {\"model\":\"vendor/fallback\",\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", content)
	}))
	defer fallback.Close()
	store, s, first, second := routingFixture(t, primary.URL, fallback.URL)
	ctx := t.Context()
	for _, pair := range [][2]string{{"conversation", work.GeneralRouterModelRole}, {"benchmark", work.BenchmarkAdmissionModelRole}, {"benchmark", work.BenchmarkRevisionModelRole}, {"benchmark", work.BenchmarkExecutionModelRole}} {
		if _, err := s.SaveModelRoutingPolicy(ctx, pair[0], pair[1], routingInput(0, first, second)); err != nil {
			t.Fatal(err)
		}
	}
	routerMode.Store(true)
	result, err := s.RouteSubmission(ctx, "解释概念", nil, nil, 256)
	if err != nil || result.Route == nil || result.Route.Kind != "chat" || result.Requests != 2 || result.Usage != nil || result.RouterModel == nil || result.RouterModel.RequestedModel != "second-model" || result.RouterModel.ReportedModel != "vendor/fallback" || result.RouterModel.FallbackReason != "provider_unavailable" {
		t.Fatalf("router fallback: %+v %v", result, err)
	}
	// Exercise the durable admission path, including the extra request counter.
	run, err := store.SubmitRoutedChat(ctx, "route-fallback-admission", "解释概念", "", "", work.Delivery{}, s.RouteSubmission)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.RunRecord(ctx, run.ID, "")
	if err != nil || record.Usage == nil || record.Usage.ModelRequests != 2 || record.Usage.UnreportedRequests != 2 {
		t.Fatalf("classification budget: %+v %v", record.Usage, err)
	}
	routerMode.Store(false)
	if got, err := s.InterpretBenchmarkAdmission(ctx, work.BenchmarkAdmissionModelInput{Text: "示例"}, 256); err != nil || got != `{"action":"clarify"}` {
		t.Fatalf("admission stage: %q %v", got, err)
	}
	if got, err := s.InterpretBenchmarkRevision(ctx, work.BenchmarkRevisionModelInput{Text: "示例"}, 256); err != nil || got != `{"action":"clarify"}` {
		t.Fatalf("revision stage: %q %v", got, err)
	}
	if got, err := s.InterpretBenchmarkExecution(ctx, work.BenchmarkExecutionModelInput{Text: "示例"}, 256); err != nil || got != `{"action":"clarify"}` {
		t.Fatalf("execution stage: %q %v", got, err)
	}
	if calls.Load() != 5 {
		t.Fatalf("unexpected fallback call count %d", calls.Load())
	}
}

func TestRoutingPolicyAttemptsFallbackOnBoundedTimeout(t *testing.T) {
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer fallback.Close()
	_, s, first, second := routingFixture(t, blocked.URL, fallback.URL)
	if _, err := s.SaveModelRoutingPolicy(t.Context(), "conversation", work.ConversationReplyModelRole, routingInput(0, first, second)); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := s.routingProvider(t.Context(), "conversation", work.ConversationReplyModelRole)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	out, err := p.requestStream(ctx, responseRequest{Input: []any{responseMessage{Role: "user", Content: "hello"}}, MaxOutputTokens: 128}, nil)
	if err != nil || out.fallbackReason != "timeout" || fallbackCalls.Load() != 1 {
		t.Fatalf("bounded timeout: %+v %v", out, err)
	}
}

func TestRoutingPolicyRouterBudgetRejectionDoesNotRestartChain(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
	}))
	defer fallback.Close()
	_, s, first, second := routingFixture(t, primary.URL, fallback.URL)
	if _, err := s.SaveModelRoutingPolicy(t.Context(), "conversation", work.GeneralRouterModelRole, routingInput(0, first, second)); err != nil {
		t.Fatal(err)
	}
	out, err := s.RouteSubmission(t.Context(), "current", []work.Message{{Role: "user", Text: "old"}, {Role: "assistant", Text: "answer"}}, nil, 256)
	if err == nil || out.Requests != 2 || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("budget rejection restarted chain: %+v calls=%d/%d err=%v", out, primaryCalls.Load(), fallbackCalls.Load(), err)
	}
}
