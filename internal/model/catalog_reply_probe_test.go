package model

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"clusbot/internal/work"
)

func TestCatalogReplyProbeAcceptsTextOnlyModel(t *testing.T) {
	ctx := t.Context()
	s, err := work.Open(filepath.Join(t.TempDir(), "catalog-probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := &Settings{Store: s}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != "text-fixture" || len(input.Tools) != 0 ||
			r.Header.Get("Authorization") != "Bearer test-probe-secret" {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	t.Setenv("CLUSBOT_CATALOG_PROBE_FIXTURE", "test-probe-secret")
	profile, err := settings.SaveProfile(ctx, "", ProfileInput{
		Name: "Text fixture", Protocol: ProtocolResponses, BaseURL: server.URL, ModelID: "text-fixture",
		KeyRef: "env:CLUSBOT_CATALOG_PROBE_FIXTURE",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := settings.TestCatalogModelReply(ctx, profile.ID, profile.Revision+1); !errors.Is(err, work.ErrModelSettingsRevision) || calls.Load() != 0 {
		t.Fatalf("stale catalog trial reached provider: %v", err)
	}
	if _, err := settings.TestCatalogModelReply(ctx, profile.ID, profile.Revision); err != nil || calls.Load() != 1 {
		t.Fatalf("fixed trial did not succeed as one minimal text request: %v", err)
	}
	t.Setenv("CLUSBOT_CATALOG_PROBE_FIXTURE", "")
	if _, err := settings.TestCatalogModelReply(ctx, profile.ID, profile.Revision); err == nil || calls.Load() != 1 {
		t.Fatal("missing configured credential caused an anonymous request")
	}
}
