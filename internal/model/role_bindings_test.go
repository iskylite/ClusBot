package model

import (
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

func TestBenchmarkAdmissionRoleRoutesExactlyAndFailsClosed(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "model-role.sqlite")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetConnectionCredentialKey("stable-fixture-store-key")
	defer func() { store.Close() }()
	s := &Settings{Store: store}
	const privateAsset = "private-asset-ref"
	var defaultCalls, profileCalls atomic.Int32
	var profileFailure atomic.Bool
	fixture := func(bound bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if r.URL.Path != "/responses" {
				t.Error("unexpected endpoint")
			}
			if bound {
				profileCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer profile-test-key" || !strings.Contains(string(raw), "pinned-model") ||
					strings.Contains(string(raw), privateAsset) || strings.Contains(string(raw), "visible_asset_refs") || strings.Contains(string(raw), "tools") {
					t.Error("explicit role sent unapproved data or used incorrect config")
					http.Error(w, "bad fixture", http.StatusBadRequest)
					return
				}
				if profileFailure.Load() {
					http.Error(w, "fixture denied", http.StatusForbidden)
					return
				}
			} else {
				defaultCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer default-test-key" || !strings.Contains(string(raw), "original-default") {
					t.Error("default model route changed")
				}
			}
			if !strings.Contains(string(raw), "POD7") {
				t.Error("missing bounded candidate")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture response\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
		}))
	}
	oldServer, newServer := fixture(false), fixture(true)
	defer oldServer.Close()
	defer newServer.Close()
	t.Setenv("OLD_ROLE_KEY", "default-test-key")
	t.Setenv("NEW_ROLE_KEY", "profile-test-key")
	if _, err := s.Initialize(ctx, work.ModelSettings{ModelID: "original-default", BaseURL: oldServer.URL, Protocol: ProtocolResponses, KeyRef: "env:OLD_ROLE_KEY"}); err != nil {
		t.Fatal(err)
	}
	p := ProfileInput{Name: "explicit destination", ModelID: "pinned-model", BaseURL: newServer.URL, Protocol: ProtocolResponses, KeyRef: "env:NEW_ROLE_KEY"}
	profile, err := s.SaveProfile(ctx, "", p)
	if err != nil {
		t.Fatal(err)
	}
	input := work.BenchmarkAdmissionModelInput{Text: "POD7 的基准安排起来", Candidates: []work.BenchmarkAdmissionCandidate{{
		Target: "POD7", Profile: work.BenchmarkProfileRef{Profile: "full", Version: 1}, AssetRefs: []string{privateAsset},
	}}}
	invoke := func() error {
		_, err := s.InterpretBenchmarkAdmission(ctx, input, 128)
		return err
	}
	if err := invoke(); err != nil || defaultCalls.Load() != 1 || profileCalls.Load() != 0 {
		t.Fatalf("saving a profile changed active model: %v", err)
	}
	bind := BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: profile.ID, ProfileRevision: 1}
	if _, err := s.SetBenchmarkAdmissionRole(ctx, bind); err == nil {
		t.Fatal("destination selected without explicit egress confirmation")
	}
	bind.ConfirmRequestTextEgress = true
	view, err := s.SetBenchmarkAdmissionRole(ctx, bind)
	if err != nil || view.Source != "explicit_profile" || view.Status != "locally_configured" || view.Revision != 1 || len(view.EgressFields) != 3 {
		t.Fatalf("bad role view: %+v %v", view, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetConnectionCredentialKey("stable-fixture-store-key")
	s.Store = store
	if err := invoke(); err != nil || defaultCalls.Load() != 1 || profileCalls.Load() != 1 {
		t.Fatalf("role did not survive restart without fallback: %v", err)
	}
	profileFailure.Store(true)
	if err := invoke(); err == nil || defaultCalls.Load() != 1 || profileCalls.Load() != 2 {
		t.Fatalf("remote failure silently fell back: %v", err)
	}
	profileFailure.Store(false)
	t.Setenv("NEW_ROLE_KEY", "fresh-key")
	if err := invoke(); !errors.Is(err, work.ErrBenchmarkAdmissionModelBinding) || defaultCalls.Load() != 1 || profileCalls.Load() != 2 {
		t.Fatalf("changed credential was contacted or fallback occurred: %v", err)
	}
	t.Setenv("NEW_ROLE_KEY", "profile-test-key")
	p.Revision = 1
	p.Name = "updated"
	if _, err := s.SaveProfile(ctx, profile.ID, p); err != nil {
		t.Fatal(err)
	}
	if view, err := s.ReadBenchmarkAdmissionRole(ctx); err != nil || view.Status != "profile_revision_changed" {
		t.Fatalf("role drift not visible: %+v %v", view, err)
	}
	if err := invoke(); !errors.Is(err, work.ErrBenchmarkAdmissionModelBinding) || defaultCalls.Load() != 1 || profileCalls.Load() != 2 {
		t.Fatalf("new revision was selected implicitly: %v", err)
	}
	if err := s.DeleteProfile(ctx, profile.ID, 2); err != nil {
		t.Fatal(err)
	}
	if view, err := s.ReadBenchmarkAdmissionRole(ctx); err != nil || view.Status != "profile_missing" {
		t.Fatalf("missing profile not visible: %+v %v", view, err)
	}
	if err := invoke(); !errors.Is(err, work.ErrBenchmarkAdmissionModelBinding) || defaultCalls.Load() != 1 {
		t.Fatalf("deleted profile fell back: %v", err)
	}
	if _, err := s.SetBenchmarkAdmissionRole(ctx, BenchmarkAdmissionRoleInput{Action: "use_existing_default", Revision: 0}); !errors.Is(err, work.ErrModelSettingsRevision) {
		t.Fatalf("stale explicit routing change accepted: %v", err)
	}
	if _, err := s.SetBenchmarkAdmissionRole(ctx, BenchmarkAdmissionRoleInput{Action: "use_existing_default", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); err != nil || defaultCalls.Load() != 2 || profileCalls.Load() != 2 {
		t.Fatalf("explicit clear did not restore existing model: %v", err)
	}
}
