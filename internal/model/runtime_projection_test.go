package model

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestModelRuntimeProjectionUsesSettingsWithoutProbing(t *testing.T) {
	ctx := t.Context()
	store, err := work.Open(filepath.Join(t.TempDir(), "runtime-projection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := Settings{Store: store, MaxRequestBytes: 12 * 1024}
	read := func() RuntimeProjection {
		t.Helper()
		out, err := settings.RuntimeProjection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got.Availability != "not_configured" || got.Policy != nil {
		t.Fatalf("missing config: %+v", got)
	}
	if err := store.InitModelSettings(ctx, work.ModelSettings{BaseURL: "http://127.0.0.1:1/private-url", Protocol: ProtocolResponses,
		ModelID: "test-model", KeyRef: "env:PRIVATE_ENV_REF"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRIVATE_ENV_REF", "")
	if got := read(); got.Availability != "configured" || got.RemoteObservation != "not_checked" || got.CredentialSource != "not_resolved" ||
		got.Policy.ContextTokens != DefaultContextTokens || got.Policy.ContextWindowSource != "runtime_default" ||
		got.Capabilities.ToolCalling != "adapter_enabled" || got.Capabilities.Vision != "unknown_disabled" || got.Capabilities.Reasoning != "unknown" {
		t.Fatalf("default policy wrongly presented: %+v", got)
	}
	t.Setenv("PRIVATE_ENV_REF", "secret-by-environment")
	if got := read(); got.CredentialSource != "environment" {
		t.Fatalf("environment key provenance: %+v", got)
	}
	stored, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctxTokens, output, vision := 100, 150, false
	stored.Attributes = &work.ModelAttributes{ContextTokens: &ctxTokens, MaxOutputTokens: &output, Vision: &vision, ReasoningLevels: []string{"low", "high"}, ReasoningEffort: "high"}
	stored.APIKey = "secret-by-local-storage"
	if _, err := store.SaveModelSettings(ctx, stored); err != nil {
		t.Fatal(err)
	}
	settings.BaseURLOverride, settings.ModelIDOverride = "https://override.invalid/private-override-url", "override-model"
	got := read()
	if got.ModelID != "override-model" || got.ModelIDSource != "runtime_override" || got.EndpointSource != "runtime_override" ||
		got.CredentialSource != "local_secret" || got.Policy.MaxRequestBytes != 12*1024 || *got.Policy.EffectiveOutputUpperBound != 50 ||
		got.Capabilities.Vision != "disabled" || got.Policy.ReasoningEffort != "high" || got.Capabilities.Reasoning != "metadata_supported" {
		t.Fatalf("effective runtime policy: %+v policy=%+v", got, got.Policy)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-url", "PRIVATE_ENV_REF", "secret-by-environment", "secret-by-local-storage", "private-override-url"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("private setting leaked: %q", secret)
		}
	}
	settings.BaseURLOverride = "malformed"
	if got := read(); got.Availability != "invalid_configuration" || got.Policy != nil {
		t.Fatalf("unconstructable runtime marked available: %+v", got)
	}
	settings.Demo = true
	if got := read(); got.Mode != "demo" || got.Policy != nil || got.Capabilities.ToolCalling != "unavailable" {
		t.Fatalf("demo advertised external abilities: %+v", got)
	}
}
