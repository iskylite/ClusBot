package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/work"
)

func TestCatalogSharedProviderDefaultAndFailClosed(t *testing.T) {
	ctx := t.Context()
	store, err := work.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetConnectionCredentialKey("fixture-catalog-key")
	var calls atomic.Int32
	var expectedKey atomic.Value
	expectedKey.Store("first-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+expectedKey.Load().(string) {
			t.Errorf("wrong credential on %s", r.URL.Path)
			http.Error(w, "denied", 401)
			return
		}
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"alpha"},{"id":"beta"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/responses" || !strings.Contains(string(body), `"model":"alpha"`) {
			t.Errorf("wrong runtime destination or model: %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	t.Setenv("CATALOG_FIRST_KEY", "first-secret")
	t.Setenv("CATALOG_SECOND_KEY", "second-secret")
	s := &Settings{Store: store}
	if _, err := s.Initialize(ctx, work.ModelSettings{Protocol: ProtocolResponses, BaseURL: "https://legacy.invalid", ModelID: "legacy", KeyRef: "env:LEGACY_CATALOG_KEY"}); err != nil {
		t.Fatal(err)
	}
	// Legacy runtime overrides must never redirect an explicitly selected profile.
	s.BaseURLOverride, s.ModelIDOverride = "https://override.invalid", "override"
	input := ProviderInput{Name: "shared", Protocol: ProtocolResponses, BaseURL: server.URL, AuthMode: "environment", KeyRef: "env:CATALOG_FIRST_KEY"}
	provider, err := s.SaveProvider(ctx, "", input)
	if err != nil || provider.Revision != 1 || provider.CredentialStatus != "configured" {
		t.Fatalf("provider: %+v %v", provider, err)
	}
	newModel := func(name string) ProfileView {
		p, err := s.SaveCatalogModel(ctx, "", CatalogModelInput{Name: name, ProviderID: provider.ID, ProviderRevision: provider.Revision, ModelID: name})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := newModel("alpha"), newModel("beta")
	if a.ProviderID != provider.ID || b.ProviderID != provider.ID {
		t.Fatal("models are not attached to the same provider")
	}
	list, err := s.DiscoverProvider(ctx, provider.ID, 1)
	if err != nil || len(list) != 2 {
		t.Fatalf("discovery: %+v %v", list, err)
	}
	diag, err := s.DiagnoseProvider(ctx, provider.ID, 1)
	if err != nil || diag.Connection != "reachable" || diag.Authentication != "not_rejected" {
		t.Fatalf("diagnostic: %+v %v", diag, err)
	}
	views, err := s.Providers(ctx)
	if err != nil || len(views) != 1 || views[0].ModelCount != 2 {
		t.Fatalf("provider list: %+v %v", views, err)
	}
	encoded, _ := json.Marshal([]any{provider, views, a, b})
	if strings.Contains(string(encoded), "CATALOG_FIRST_KEY") || strings.Contains(string(encoded), "first-secret") {
		t.Fatalf("credentials leaked: %s", encoded)
	}
	if _, err := s.SetDefaultModel(ctx, DefaultModelInput{Source: "profile", ProfileID: a.ID, ProfileRevision: a.Revision, Revision: 0}); err != nil {
		t.Fatal(err)
	}
	p, err := s.provider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stream(ctx, []work.Message{{Role: "user", Text: "hi"}}, false, 100, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	bound, err := s.SetBenchmarkAdmissionRole(ctx, BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: b.ID, ProfileRevision: b.Revision, ConfirmRequestTextEgress: true})
	if err != nil || bound.Status != "locally_configured" {
		t.Fatalf("bind: %+v %v", bound, err)
	}
	credential, err := s.IsolatedReplyCredential(ctx, b.ID, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	pin := work.IsolatedReplyPin{ProfileID: b.ID, ProfileRevision: b.Revision, CredentialBinding: credential}
	input.Revision, input.KeyRef = 1, "env:CATALOG_SECOND_KEY"
	provider, err = s.SaveProvider(ctx, provider.ID, input)
	if err != nil || provider.Revision != 2 {
		t.Fatalf("provider update: %+v %v", provider, err)
	}
	expectedKey.Store("second-secret")
	p, err = s.provider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Stream(ctx, []work.Message{{Role: "user", Text: "hi"}}, false, 100, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	items, err := s.Profiles(ctx)
	if err != nil || len(items) != 2 || items[0].Revision != 2 || items[1].Revision != 2 {
		t.Fatalf("provider update did not advance both models: %+v %v", items, err)
	}
	role, err := s.ReadBenchmarkAdmissionRole(ctx)
	if err != nil || role.Status != "profile_revision_changed" {
		t.Fatalf("role must require reconfirmation: %+v %v", role, err)
	}
	if _, err := s.IsolatedReplyProvider(ctx, pin); !errors.Is(err, work.ErrIsolatedReplyUnavailable) {
		t.Fatalf("stale isolated reply: %v", err)
	}
	before := calls.Load()
	t.Setenv("CATALOG_SECOND_KEY", "")
	if _, err := s.provider(ctx); err == nil {
		t.Fatal("missing credential made default available")
	}
	projection, err := s.RuntimeProjection(ctx)
	if err != nil || projection.Availability != "credential_missing" || projection.DefaultProfileID != a.ID {
		t.Fatalf("missing credential projection: %+v %v", projection, err)
	}
	diag, err = s.DiagnoseProvider(ctx, provider.ID, 2)
	if err != nil || diag.Authentication != "credential_missing" || diag.Connection != "not_checked" || calls.Load() != before {
		t.Fatalf("missing credential caused a request: %+v %v", diag, err)
	}
	off := false
	input.Revision, input.KeyRef, input.Enabled = 2, "", &off
	provider, err = s.SaveProvider(ctx, provider.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.provider(ctx); !errors.Is(err, work.ErrModelDefaultUnavailable) {
		t.Fatalf("disabled default did not fail closed: %v", err)
	}
	role, err = s.ReadBenchmarkAdmissionRole(ctx)
	if err != nil || role.Status != "provider_disabled" {
		t.Fatalf("disabled role: %+v %v", role, err)
	}
	if _, _, err := s.benchmarkAdmissionProvider(ctx); !errors.Is(err, work.ErrBenchmarkAdmissionModelBinding) {
		t.Fatalf("disabled role fallback: %v", err)
	}
	if _, err := s.IsolatedReplyProvider(ctx, pin); !errors.Is(err, work.ErrIsolatedReplyUnavailable) {
		t.Fatalf("disabled isolated reply: %v", err)
	}
	projection, err = s.RuntimeProjection(ctx)
	if err != nil || projection.Availability != "default_unavailable" || projection.DefaultProfileID != a.ID || calls.Load() != before {
		t.Fatalf("disabled default projection or request: %+v %v", projection, err)
	}
	selection, err := s.DefaultModel(ctx)
	if err != nil || selection.ProfileID != a.ID || selection.Status != "provider_disabled" {
		t.Fatalf("default reference was lost: %+v %v", selection, err)
	}
}

func TestLocalProviderCanExplicitlyRunWithoutAuthentication(t *testing.T) {
	ctx := t.Context()
	store, err := work.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected authentication")
		}
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"local"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	s := &Settings{Store: store}
	if _, err := s.Initialize(ctx, work.ModelSettings{Protocol: ProtocolResponses, BaseURL: "https://legacy.invalid", KeyRef: "env:LEGACY_LOCAL_KEY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveProvider(ctx, "", ProviderInput{Name: "remote", Protocol: ProtocolResponses, BaseURL: "https://remote.invalid", AuthMode: "none"}); err == nil {
		t.Fatal("allowed anonymous remote provider")
	}
	provider, err := s.SaveProvider(ctx, "", ProviderInput{Name: "local", Protocol: ProtocolResponses, BaseURL: server.URL, AuthMode: "none"})
	if err != nil || provider.CredentialStatus != "not_required" {
		t.Fatalf("local provider: %+v %v", provider, err)
	}
	m, err := s.SaveCatalogModel(ctx, "", CatalogModelInput{Name: "local model", ProviderID: provider.ID, ProviderRevision: 1, ModelID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDefaultModel(ctx, DefaultModelInput{Source: "profile", ProfileID: m.ID, ProfileRevision: 1}); err != nil {
		t.Fatal(err)
	}
	p, err := s.provider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Stream(ctx, []work.Message{{Role: "user", Text: "hello"}}, false, 100, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IsolatedReplyCredential(ctx, m.ID, 1); err != nil {
		t.Fatalf("anonymous local isolated reply unavailable: %v", err)
	}
	diag, err := s.DiagnoseProvider(ctx, provider.ID, 1)
	if err != nil || diag.Authentication != "not_used" || diag.Connection != "reachable" || calls.Load() != 2 {
		t.Fatalf("local diagnostic: %+v %v", diag, err)
	}
}
