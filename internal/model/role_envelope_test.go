package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clusbot/internal/work"
)

const roleEnvelopeFixtureKey = "env:CLUSBOT_INTENT_TEST_KEY"

func setupRoleEnvelopeFixture(t *testing.T, router bool) (*work.Store, *Settings, string, <-chan responseRequest) {
	t.Helper()
	t.Setenv("CLUSBOT_INTENT_TEST_KEY", "synthetic-envelope-fixture")
	requests := make(chan responseRequest, 8)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		requests <- req
		answer := "fixture"
		if router {
			answer = `{"kind":"chat"}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	t.Cleanup(model.Close)
	store, s := classificationSettings(t, model.URL)
	return store, s, model.URL, requests
}

func assertModelRequestOmitted(t *testing.T, req responseRequest, model string, omitted string) {
	t.Helper()
	reqBytes, err := json.Marshal(req.Input)
	if err != nil || req.Model != model || strings.Contains(string(reqBytes), omitted) {
		t.Fatalf("request model=%s unexpectedly retained earlier text %q, err=%v", req.Model, omitted, err)
	}
}

func TestRoleEnvelopeReplyDoesNotReaddUnconfiguredHistoryExcerpts(t *testing.T) {
	store, s, endpoint, requests := setupRoleEnvelopeFixture(t, false)
	ctx := t.Context()
	s.MaxRequestBytes = 4096
	const omitted = "SYNTHETIC_OLD_REPLY_TEXT"
	in := []work.Message{
		{ID: 1, Role: "user", Text: omitted + strings.Repeat("a", 1900)},
		{ID: 2, Role: "assistant", Text: strings.Repeat("b", 1600)},
		{ID: 3, Role: "user", Text: strings.Repeat("current ", 140), RequiredInput: true},
	}
	call := func() responseRequest {
		t.Helper()
		provider, _, err := s.TextRunProvider(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Stream(ctx, in, false, 256, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return <-requests
	}
	assertModelRequestOmitted(t, call(), "fixture", omitted)
	p, err := s.SaveProfile(ctx, "", ProfileInput{Name: "Reply", ModelID: "alt-rep", Protocol: ProtocolResponses, BaseURL: endpoint, KeyRef: roleEnvelopeFixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConversationReplyRole(ctx, ConversationReplyRoleInput{Action: "bind_profile", ProfileID: p.ID, ProfileRevision: 1, ConfirmConversationContextEgress: true}); err != nil {
		t.Fatal(err)
	}
	assertModelRequestOmitted(t, call(), "alt-rep", omitted)
	// A default that already uses a configured context window does permit
	// bounded excerpts. The selected role can keep that inherited excerpt.
	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	knownWindow := DefaultContextTokens
	base.Attributes = &work.ModelAttributes{ContextTokens: &knownWindow}
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	allowed, err := json.Marshal(call().Input)
	if err != nil || !strings.Contains(string(allowed), omitted) {
		t.Fatalf("role discarded a permissible excerpt that fits: %v", err)
	}
}

func TestRoleEnvelopeRouterLowerOutputDoesNotReaddHistory(t *testing.T) {
	store, s, endpoint, requests := setupRoleEnvelopeFixture(t, true)
	ctx := t.Context()
	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const requestedOutput = 256
	const omitted = "SYNTHETIC_OLD_ROUTER_TEXT"
	history := []work.Message{{Role: "user", Text: omitted + strings.Repeat("a", 1050)}, {Role: "assistant", Text: strings.Repeat("b", 230)}}
	const current = "今天天气如何？"
	// Pick a valid explicit window that would fit this exact packet if only
	// the reply reservation were lower. The default must omit one whole turn.
	packet, err := json.Marshal(struct {
		History []work.Message `json:"history"`
		Current string         `json:"current"`
	}{history, current})
	if err != nil {
		t.Fatal(err)
	}
	req := responseRequest{Model: "fixture", Input: []any{responseMessage{Type: "message", Role: "user", Content: string(packet)}}, Instructions: routerInstructions, Stream: true, MaxOutputTokens: requestedOutput}
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	estimate := (len(wire)+1)/2 + 16 + 8*len(req.Input)
	window := (estimate + 100) * 10 / 9
	base.Attributes = &work.ModelAttributes{ContextTokens: &window}
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	call := func() responseRequest {
		t.Helper()
		result, err := s.RouteSubmission(ctx, current, history, nil, requestedOutput)
		if err != nil || result.Route == nil {
			t.Fatalf("router call: %+v %v", result, err)
		}
		return <-requests
	}
	assertModelRequestOmitted(t, call(), "fixture", omitted)
	smallOutput := 16
	p, err := s.SaveProfile(ctx, "", ProfileInput{Name: "Router", ModelID: "alt-rtr", Protocol: ProtocolResponses, BaseURL: endpoint, KeyRef: roleEnvelopeFixtureKey,
		Attributes: &work.ModelAttributes{ContextTokens: &window, MaxOutputTokens: &smallOutput}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGeneralRouterRole(ctx, BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: p.ID, ProfileRevision: 1, ConfirmRequestTextEgress: true}); err != nil {
		t.Fatal(err)
	}
	request := call()
	if request.MaxOutputTokens != smallOutput {
		t.Fatalf("role output cap not applied: %d", request.MaxOutputTokens)
	}
	assertModelRequestOmitted(t, request, "alt-rtr", omitted)
}

func TestRoleEnvelopeSmallerReplyCanStillDropWholeTurns(t *testing.T) {
	store, s, endpoint, requests := setupRoleEnvelopeFixture(t, false)
	ctx := t.Context()
	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defaultWindow, roleWindow := 16000, 1500
	base.Attributes = &work.ModelAttributes{ContextTokens: &defaultWindow}
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	p, err := s.SaveProfile(ctx, "", ProfileInput{Name: "Reply", ModelID: "alt-rep", Protocol: ProtocolResponses, BaseURL: endpoint, KeyRef: roleEnvelopeFixtureKey,
		Attributes: &work.ModelAttributes{ContextTokens: &roleWindow}})
	if err != nil {
		t.Fatal(err)
	}
	const largeEarlier = "SYNTHETIC_EARLIER_OVERSIZE"
	const recent = "SYNTHETIC_RECENT_PERMITTED"
	const current = "SYNTHETIC_REQUIRED_INPUT"
	input := []work.Message{
		{ID: 1, Role: "user", Text: largeEarlier + strings.Repeat("x", 3200)},
		{ID: 2, Role: "assistant", Text: strings.Repeat("y", 2100)},
		{ID: 3, Role: "user", Text: recent},
		{ID: 4, Role: "assistant", Text: "short recent reply"},
		{ID: 5, Role: "user", Text: current, RequiredInput: true},
	}
	call := func() responseRequest {
		t.Helper()
		model, _, err := s.TextRunProvider(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.Stream(ctx, input, false, 128, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return <-requests
	}
	baseline, err := json.Marshal(call().Input)
	if err != nil || !strings.Contains(string(baseline), largeEarlier) {
		t.Fatalf("default fixture was not allowed full input: %v", err)
	}
	if _, err := s.SetConversationReplyRole(ctx, ConversationReplyRoleInput{Action: "bind_profile", ProfileID: p.ID, ProfileRevision: 1, ConfirmConversationContextEgress: true}); err != nil {
		t.Fatal(err)
	}
	selected := call()
	assertModelRequestOmitted(t, selected, "alt-rep", largeEarlier)
	data, err := json.Marshal(selected.Input)
	if err != nil || !strings.Contains(string(data), recent) || !strings.Contains(string(data), current) ||
		strings.Count(selected.Instructions, "较早对话已省略") != 1 {
		t.Fatalf("explicit role could not shrink while retaining required and recent input: %v", err)
	}
}
