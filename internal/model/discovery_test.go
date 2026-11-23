package model

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

func TestDiscoveryUsesOnlyDeclaredCapabilitiesAndKeepsSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"declared","name":"Example Model","context_length":123456,"top_provider":{"max_completion_tokens":4096},"architecture":{"input_modalities":["text","image","file"]},"supported_parameters":["tools","structured_outputs","reasoning_effort"],"reasoning":{"supported_efforts":["low","high","max"],"default_effort":"high"}},
			{"id":"model-vision-reasoning-tools-json","capabilities":{"vision":true,"tools":true},"architecture":{"input_modalities":["future-modality"]}},
			{"id":"explicit-false","supports_tools":false,"supports_structured_output":false,"supports_reasoning":false,"supported_parameters":["tools","structured_outputs","reasoning"],"supported_reasoning_efforts":["low"]},
			{"id":"malformed","context_length":"large","reasoning":true,"supported_parameters":{},"architecture":{"input_modalities":"image"}},
			{"id":"text-only","architecture":{"input_modalities":["text"]}}
		]}`)
	}))
	defer server.Close()
	items, err := discoverModelValues(t.Context(), work.ModelSettings{BaseURL: server.URL, Protocol: ProtocolResponses}, "")
	if err != nil || len(items) != 5 {
		t.Fatalf("discovery: %+v %v", items, err)
	}
	byID := make(map[string]DiscoveredModel)
	for _, item := range items {
		byID[item.ID] = item
	}
	declared := byID["declared"]
	if declared.Name != "Example Model" || declared.Attributes.ContextTokens == nil || *declared.Attributes.ContextTokens != 123456 || declared.Attributes.MaxOutputTokens == nil || *declared.Attributes.MaxOutputTokens != 4096 {
		t.Fatalf("basic fields: %+v", declared)
	}
	a := declared.Attributes
	if a.Vision == nil || !*a.Vision || a.FileInput == nil || !*a.FileInput || a.ToolCalling == nil || !*a.ToolCalling || a.StructuredOutput == nil || !*a.StructuredOutput || a.Reasoning == nil || !*a.Reasoning || !reflect.DeepEqual(a.ReasoningLevels, []string{"low", "high", "max"}) || a.ReasoningEffort != "" {
		t.Fatalf("declared capabilities: %+v", a)
	}
	for _, key := range []string{"context_tokens", "max_output_tokens", "vision", "file_input", "tool_calling", "structured_output", "reasoning", "reasoning_levels"} {
		if a.Sources[key] != "provider" {
			t.Errorf("source for %s: %q", key, a.Sources[key])
		}
	}
	for _, id := range []string{"model-vision-reasoning-tools-json", "malformed"} {
		if raw, _ := json.Marshal(byID[id].Attributes); string(raw) != "{}" {
			t.Errorf("%s inferred unknown metadata: %s", id, raw)
		}
	}
	no := byID["explicit-false"].Attributes
	if no.ToolCalling == nil || *no.ToolCalling || no.StructuredOutput == nil || *no.StructuredOutput || no.Reasoning == nil || *no.Reasoning || len(no.ReasoningLevels) != 0 {
		t.Fatalf("explicit false lost: %+v", no)
	}
	text := byID["text-only"].Attributes
	if text.Vision == nil || *text.Vision || text.FileInput == nil || *text.FileInput || text.ToolCalling != nil || text.StructuredOutput != nil {
		t.Fatalf("text-only capabilities: %+v", text)
	}

	// Attributes and their edited provenance survive the catalog store.
	store, err := work.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := &Settings{Store: store}
	provider, err := settings.SaveProvider(t.Context(), "", ProviderInput{Name: "local", Protocol: ProtocolResponses, BaseURL: server.URL, AuthMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	noValue := false
	a.Vision, a.Sources["vision"] = &noValue, "manual"
	model, err := settings.SaveCatalogModel(t.Context(), "", CatalogModelInput{Name: declared.Name, ProviderID: provider.ID, ProviderRevision: provider.Revision, ModelID: declared.ID, Attributes: &a})
	if err != nil {
		t.Fatal(err)
	}
	models, err := settings.Profiles(t.Context())
	if err != nil || len(models) != 1 || models[0].ID != model.ID || models[0].Attributes.Vision == nil || *models[0].Attributes.Vision || models[0].Attributes.Sources["vision"] != "manual" || models[0].Attributes.Sources["tool_calling"] != "provider" {
		t.Fatalf("stored override: %+v %v", models, err)
	}
}
