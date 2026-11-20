package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestGeneralRouterRoleUsesOnlyDefaultTrustDestination(t *testing.T) {
	const key = "CLUSBOT_INTENT_TEST_KEY"
	t.Setenv(key, "router-local-fixture")
	t.Setenv("CLUSBOT_OTHER_ROUTER_KEY", "router-local-fixture")
	var calls atomic.Int64
	traffic := make(chan responseRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		traffic <- req
		w.Header().Set("Content-Type", "text/event-stream")
		result := `{"kind":"chat"}`
		if req.Instructions == delegationInstructions {
			result = "CHAT"
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", result)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer srv.Close()
	store, s := classificationSettings(t, srv.URL)
	ctx := t.Context()
	view, err := s.ReadGeneralRouterRole(ctx)
	if err != nil || view.Source != "legacy_default" || view.Revision != 0 {
		t.Fatalf("implicit selection changed: %+v, %v", view, err)
	}
	profile := func(name, endpoint, ref string) ProfileView {
		t.Helper()
		p, err := s.SaveProfile(ctx, "", ProfileInput{Name: name, BaseURL: endpoint, Protocol: ProtocolResponses, ModelID: name, KeyRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	candidate := profile("router", srv.URL, "env:"+key)
	otherEndpoint := profile("other-endpoint", srv.URL+"/other", "env:"+key)
	otherRef := profile("other-ref", srv.URL, "env:CLUSBOT_OTHER_ROUTER_KEY")
	bind := func(p ProfileView, consent bool, revision int) (BenchmarkAdmissionRoleView, error) {
		return s.SetGeneralRouterRole(ctx, BenchmarkAdmissionRoleInput{Action: "bind_profile",
			ProfileID: p.ID, ProfileRevision: p.Revision, ConfirmRequestTextEgress: consent, Revision: revision})
	}
	for _, trial := range []struct {
		p       ProfileView
		consent bool
	}{{candidate, false}, {otherEndpoint, true}, {otherRef, true}} {
		if _, err := bind(trial.p, trial.consent, 0); err == nil {
			t.Fatalf("bound without consent or identical trust destination: %+v", trial.p)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("settings validation called model")
	}
	text := "当前指令"
	history := []work.Message{{Role: "user", Text: "近况"}}
	tasks := []work.RouteCandidate{{ID: "local-task"}}
	route := func() error {
		_, err := s.RouteSubmission(ctx, text, history, tasks, 128)
		return err
	}
	if err := route(); err != nil {
		t.Fatal(err)
	}
	before := <-traffic
	if before.Model != "fixture" {
		t.Fatalf("legacy route chose %s", before.Model)
	}
	view, err = bind(candidate, true, 0)
	if err != nil || view.Status != "locally_configured" || len(view.EgressFields) != 5 {
		t.Fatalf("explicit role invalid: %+v %v", view, err)
	}
	if _, err := bind(candidate, true, 0); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("stale role revision accepted: %v", err)
	}
	if err := route(); err != nil {
		t.Fatal(err)
	}
	after := <-traffic
	if after.Model != "router" || !reflect.DeepEqual(before.Input, after.Input) ||
		before.Instructions != after.Instructions || before.MaxOutputTokens != after.MaxOutputTokens {
		t.Fatalf("role changed model or request boundary incorrectly: before=%+v after=%+v", before, after)
	}
	if _, err := s.ClassifyDelegation(ctx, text, history, 128); err != nil {
		t.Fatal(err)
	}
	if got := (<-traffic).Model; got != "fixture" {
		t.Fatalf("role affected legacy delegation: %s", got)
	}

	defaultValue, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defaultValue.BaseURL = srv.URL + "/changed"
	defaultValue, err = store.SaveModelSettings(ctx, defaultValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := route(); !errors.Is(err, errGeneralRouterBinding) {
		t.Fatalf("mismatched endpoint did not fail closed: %v", err)
	}
	defaultValue.BaseURL = srv.URL
	if _, err := store.SaveModelSettings(ctx, defaultValue); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key, "rotated-local-fixture")
	if err := route(); !errors.Is(err, errGeneralRouterBinding) {
		t.Fatalf("rotated credential did not fail closed: %v", err)
	}
	t.Setenv(key, "router-local-fixture")
	if err := s.DeleteProfile(ctx, candidate.ID, candidate.Revision); err != nil {
		t.Fatal(err)
	}
	if err := route(); !errors.Is(err, errGeneralRouterBinding) {
		t.Fatalf("deleted profile did not fail closed: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("invalid pins made requests: %d", calls.Load())
	}
	if _, err := s.SetGeneralRouterRole(ctx, BenchmarkAdmissionRoleInput{Action: "use_existing_default", Revision: view.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := route(); err != nil {
		t.Fatal(err)
	}
	if got := (<-traffic).Model; got != "fixture" {
		t.Fatalf("explicit reset did not restore default: %s", got)
	}
}

func TestGeneralRouterRoleFreezesInFlightSelection(t *testing.T) {
	const key = "CLUSBOT_INTENT_TEST_KEY"
	t.Setenv(key, "local-fixture")
	seen := make(chan string, 2)
	release := make(chan struct{})
	var released bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		seen <- req.Model
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", `{"kind":"chat"}`)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer srv.Close()
	defer func() {
		if !released {
			close(release)
		}
	}()
	store, s := classificationSettings(t, srv.URL)
	ctx := t.Context()
	in := ProfileInput{Name: "Router", Protocol: ProtocolResponses, BaseURL: srv.URL, ModelID: "router-v1", KeyRef: "env:" + key}
	profile, err := s.SaveProfile(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGeneralRouterRole(ctx, BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: profile.ID, ProfileRevision: 1, ConfirmRequestTextEgress: true}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.RouteSubmission(ctx, "普通问题", nil, nil, 128); done <- err }()
	select {
	case id := <-seen:
		if id != "router-v1" {
			t.Fatal(id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bound model request did not start")
	}
	in.ModelID = "router-v2"
	in.Revision = profile.Revision
	updated, err := s.SaveProfile(ctx, profile.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RouteSubmission(ctx, "普通问题", nil, nil, 128); !errors.Is(err, errGeneralRouterBinding) {
		t.Fatalf("profile revision change did not block the next call: %v", err)
	}
	if _, err := s.SetGeneralRouterRole(ctx, BenchmarkAdmissionRoleInput{Action: "bind_profile", Revision: 1, ProfileID: updated.ID, ProfileRevision: updated.Revision, ConfirmRequestTextEgress: true}); err != nil {
		t.Fatal(err)
	}
	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.ModelID = ""
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RouteSubmission(ctx, "不应被发出", nil, nil, 128); !errors.Is(err, errGeneralRouterBinding) {
		t.Fatalf("explicit profile bypassed unconfigured global model: %v", err)
	}
	demo := *s
	demo.Demo = true
	result, err := demo.RouteSubmission(ctx, "普通问题", nil, nil, 128)
	if err != nil || result.Route == nil || result.Route.Kind != "chat" || result.Requests != 0 {
		t.Fatalf("demo resorted to external routing: %+v %v", result, err)
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatalf("in-flight call changed after settings update: %v", err)
	}
	select {
	case model := <-seen:
		t.Fatalf("unexpected request/model %s", model)
	default:
	}
}

func TestGeneralRouterProfileCannotRaiseDefaultBudgets(t *testing.T) {
	window, output := 3000, 200
	largerWindow, largerOutput := 12000, 2000
	base := work.ModelSettings{Attributes: &work.ModelAttributes{ContextTokens: &window, MaxOutputTokens: &output}}
	profile := work.ModelSettings{Attributes: &work.ModelAttributes{ContextTokens: &largerWindow, MaxOutputTokens: &largerOutput, ReasoningEffort: "low"}}
	got := boundRouterProfile(profile, base).Attributes
	if got.ContextTokens == nil || *got.ContextTokens != window || got.MaxOutputTokens == nil || *got.MaxOutputTokens != output || got.ReasoningEffort != "low" ||
		*profile.Attributes.ContextTokens != largerWindow || *profile.Attributes.MaxOutputTokens != largerOutput {
		t.Fatalf("bad routing limits or mutates profile: %+v", got)
	}
}
