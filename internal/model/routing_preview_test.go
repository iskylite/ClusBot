package model

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestModelAttributesKeepUnknownAndFalseSeparate(t *testing.T) {
	ctx := t.Context()
	s, err := work.Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := &Settings{Store: s}
	yes, no := true, false
	draft := ProfileInput{
		Name: "fixture", Protocol: ProtocolResponses, BaseURL: "https://fixture.invalid/v1", ModelID: "fixture",
		KeyRef: "env:CLUSBOT_META_FIXTURE", Attributes: &work.ModelAttributes{ToolCalling: &no, StructuredOutput: &yes},
	}
	profile, err := settings.SaveProfile(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := settings.Profiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].ID != profile.ID {
		t.Fatalf("profile read: %+v %v", profiles, err)
	}
	raw, err := json.Marshal(profiles[0].Attributes)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"tool_calling":false,"structured_output":true}` {
		t.Fatalf("tri-state attributes not preserved: %s", got)
	}
}

func TestModelRoutingPreviewUsesCurrentBindingsAndNeverFallsBack(t *testing.T) {
	ctx := t.Context()
	s, err := work.Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := &Settings{Store: s}
	_, err = settings.Initialize(ctx, work.ModelSettings{
		Protocol: ProtocolResponses, BaseURL: "https://default.invalid/v1", ModelID: "global-model", KeyRef: "env:CLUSBOT_DEFAULT_ROUTE_FIXTURE",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := settings.ModelRouting(ctx)
	if err != nil || len(view.Roles) != 7 || !view.BusinessOverridesSupported || !view.OrderedFallbackSupported {
		t.Fatalf("routing view: %+v %v", view, err)
	}
	base, err := settings.PreviewModelRouting(ctx, work.ConversationReplyModelRole)
	if err != nil || base.Source != "global_default" || base.ModelID != "global-model" || base.RemoteObservation != "not_checked" {
		t.Fatalf("default role: %+v %v", base, err)
	}
	goOnly, err := settings.PreviewModelRouting(ctx, deterministicPhase)
	if err != nil || goOnly.Source != "go" || goOnly.ModelID != "" {
		t.Fatalf("deterministic role: %+v %v", goOnly, err)
	}
	t.Setenv("CLUSBOT_ROUTE_FIXTURE", "local-test-secret")
	input := ProfileInput{Name: "Bench", Protocol: ProtocolResponses, BaseURL: "https://fixture.invalid/v1",
		ModelID: "bench-model", KeyRef: "env:CLUSBOT_ROUTE_FIXTURE"}
	p, err := settings.SaveProfile(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.SetBenchmarkAdmissionRole(ctx, BenchmarkAdmissionRoleInput{
		Action: "bind_profile", ProfileID: p.ID, ProfileRevision: p.Revision, ConfirmRequestTextEgress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := settings.PreviewModelRouting(ctx, work.BenchmarkAdmissionModelRole)
	if err != nil || preview.Source != "explicit_profile" || preview.ModelID != p.ModelID || preview.ProfileID != p.ID {
		t.Fatalf("explicit role: %+v %v", preview, err)
	}
	raw, err := json.Marshal(preview)
	if err != nil || strings.Contains(string(raw), "local-test-secret") || strings.Contains(string(raw), "CLUSBOT_ROUTE_FIXTURE") {
		t.Fatal("routing preview exposed credentials")
	}
	input.Revision, input.ModelID = p.Revision, "new-model"
	if _, err := settings.SaveProfile(ctx, p.ID, input); err != nil {
		t.Fatal(err)
	}
	stale, err := settings.PreviewModelRouting(ctx, work.BenchmarkAdmissionModelRole)
	if err != nil || stale.Source != "explicit_profile" || stale.Status != "profile_revision_changed" || stale.ModelID != "" {
		t.Fatalf("stale binding fell back or was projected as usable: %+v %v", stale, err)
	}
}
