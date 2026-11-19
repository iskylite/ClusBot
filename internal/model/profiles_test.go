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

	"clusbot/internal/work"
)

func TestIndependentProfilesDoNotChangeDefaultOrExposeCredential(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "provider-profiles.sqlite")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	s := &Settings{Store: store}
	if _, err := s.Initialize(ctx, work.ModelSettings{Protocol: ProtocolResponses, BaseURL: "https://default.invalid/api", ModelID: "legacy", KeyRef: "env:LEGACY_PROFILE_TEST_ENV"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current.APIKey = "unrelated-default-secret"
	if _, err := store.SaveModelSettings(ctx, current); err != nil {
		t.Fatal(err)
	}
	s.BaseURLOverride, s.ModelIDOverride = "https://override.invalid/api", "default-override"
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer profile-secret" {
			t.Errorf("profile check bound unexpected endpoint or credentials")
			http.Error(w, "bad binding", http.StatusBadRequest)
			return
		}
		var in struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Model != "probe-fixture" {
			t.Errorf("profile check used default or invalid model: %+v %v", in, err)
			http.Error(w, "bad model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(t string) { fmt.Fprintf(w, "data: %s\n\n", t) }
		switch called.Add(1) {
		case 1:
			send(`{"type":"response.output_text.delta","delta":"OK"}`)
		case 2:
			send(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"clusbot_test_value","call_id":"fixture","arguments":"{}","status":"completed"}}`)
		case 3:
			var item struct {
				Output string `json:"output"`
			}
			if len(in.Input) == 0 || json.Unmarshal(in.Input[len(in.Input)-1], &item) != nil || item.Output == "" {
				t.Error("missing real tool probe output")
				return
			}
			send(fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, item.Output))
		default:
			t.Error("unexpected extra model call")
		}
		send(`{"type":"response.completed"}`)
	}))
	defer srv.Close()
	t.Setenv("LOCAL_PROFILE_TEST_CREDENTIAL", "profile-secret")
	p := ProfileInput{Name: "isolate", Protocol: ProtocolResponses, BaseURL: srv.URL, ModelID: "probe-fixture", KeyRef: "env:LOCAL_PROFILE_TEST_CREDENTIAL"}
	first, err := s.SaveProfile(ctx, "", p)
	if err != nil || first.Revision != 1 || !first.CredentialConfigured {
		t.Fatalf("profile create: %+v %v", first, err)
	}
	p2 := p
	p2.Name = "another"
	p2.ModelID = "different"
	second, err := s.SaveProfile(ctx, "", p2)
	if err != nil || second.ID == first.ID || second.Revision != 1 {
		t.Fatalf("second independent profile: %+v %v", second, err)
	}
	if _, err := s.SaveProfile(ctx, "", p); !errors.Is(err, work.ErrConflict) {
		t.Fatalf("duplicate names: %v", err)
	}
	p.Revision = 1
	p.KeyRef = ""
	changed := p
	changed.BaseURL = "https://changed.invalid"
	if _, err := s.SaveProfile(ctx, first.ID, changed); err == nil {
		t.Fatal("moved credentials to another address without explicit reference")
	}
	p.Name = "renamed"
	edit, err := s.SaveProfile(ctx, first.ID, p)
	if err != nil || edit.Revision != 2 || !edit.CredentialConfigured {
		t.Fatalf("optimistic edit: %+v %v", edit, err)
	}
	if _, err := s.TestProfile(ctx, first.ID, 1); !errors.Is(err, work.ErrModelSettingsRevision) || called.Load() != 0 {
		t.Fatalf("stale revision was tested: %v", err)
	}
	if _, err := s.TestProfile(ctx, first.ID, 2); err != nil || called.Load() != 3 {
		t.Fatalf("selected profile capability probe failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	items, err := s.Profiles(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("read durable profiles: %+v %v", items, err)
	}
	raw, err := json.Marshal(items)
	if err != nil || strings.Contains(string(raw), "LOCAL_PROFILE_TEST_CREDENTIAL") || strings.Contains(string(raw), "profile-secret") || strings.Contains(string(raw), "unrelated-default-secret") {
		t.Fatalf("profile view leaked credentials: %s %v", raw, err)
	}
	provider, err := s.provider(context.Background())
	if err != nil || provider.endpoint != "https://override.invalid/api/responses" || provider.model != "default-override" || provider.key != "unrelated-default-secret" {
		t.Fatalf("default active instance changed after profile test/migration: %v", err)
	}
	if err := s.DeleteProfile(ctx, first.ID, 1); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.DeleteProfile(ctx, first.ID, 2); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Profiles(ctx); err != nil || len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("delete touched other profile: %+v %v", got, err)
	}
}
