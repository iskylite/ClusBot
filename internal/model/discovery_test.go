package model

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clusbot/internal/work"
)

func TestDiscoverParsesMaxOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected discovery request %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"data":[
			{"id":"top-provider","context_length":200000,"top_provider":{"max_completion_tokens":8192},"max_output_tokens":4096,"architecture":{"input_modalities":["text","image"]},"supported_parameters":["reasoning"]},
			{"id":"flat-field","context_length":100000,"max_output_tokens":2048},
			{"id":"no-output","context_length":50000}
		]}`)
	}))
	defer server.Close()

	store, err := work.Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InitModelSettings(t.Context(), work.ModelSettings{
		BaseURL: server.URL + "/v1", ModelID: "top-provider", Protocol: ProtocolResponses, KeyRef: "env:TEST_KEY",
	}); err != nil {
		t.Fatal(err)
	}
	settings := &Settings{Store: store}
	draft := SettingsInput{ModelSettings: work.ModelSettings{
		BaseURL: server.URL + "/v1", Protocol: ProtocolResponses, KeyRef: "env:TEST_KEY",
	}}
	models, err := settings.Discover(t.Context(), draft)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	byID := map[string]DiscoveredModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	// top_provider.max_completion_tokens wins over flat max_output_tokens.
	if out := byID["top-provider"].Attributes.MaxOutputTokens; out == nil || *out != 8192 {
		t.Errorf("top-provider max output = %v, want 8192", out)
	}
	// Falls back to the flat max_output_tokens field.
	if out := byID["flat-field"].Attributes.MaxOutputTokens; out == nil || *out != 2048 {
		t.Errorf("flat-field max output = %v, want 2048", out)
	}
	// Absent output stays unknown (nil), not zero.
	if out := byID["no-output"].Attributes.MaxOutputTokens; out != nil {
		t.Errorf("no-output max output = %v, want nil", out)
	}
	// Context/vision/reasoning parsing unaffected.
	if ctx := byID["top-provider"].Attributes.ContextTokens; ctx == nil || *ctx != 200000 {
		t.Errorf("top-provider context = %v, want 200000", ctx)
	}
	if v := byID["top-provider"].Attributes.Vision; v == nil || !*v {
		t.Errorf("top-provider vision = %v, want true", v)
	}
}
