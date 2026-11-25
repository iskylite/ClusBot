package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestBenchmarkDraftDependencyPreview(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "draft-dependency.sqlite"))
	defer a.close()
	ctx := t.Context()
	actor := businessFixturePrincipal(t, a.store)
	const key = "profile-secret"
	const spoken = "让 deep 在 bandwidth 之后运行"
	const route = "/api/v1/domain/benchmark-profiles/" + key + "/draft/preview-dependency-edit"
	c := work.BenchmarkCase{Key: "deep", Binding: work.BenchmarkBinding{Connection: "private-connection", Revision: 1, Tool: "private-tool", Validator: "structured-metrics@1"},
		Targets: []string{"private-target"}, Parameters: map[string]string{"gpu_count": "40"}, ActualConfig: map[string]string{"gpu_count": "40"}, NeedsRawEvidence: true, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	b := c
	b.Key = "bandwidth"
	cases := []work.BenchmarkCase{b, c}
	if err := a.store.PublishBenchmarkProfile(ctx, actor, work.BenchmarkProfile{Key: key, Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	draft, err := a.store.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	in := work.BenchmarkDraftDependencyInput{BaseVersion: 1, ExpectedRevision: draft.Revision, CaseKey: "deep", PrerequisiteKey: "bandwidth", Text: spoken, ConfirmTextEgress: true}
	post := func(in work.BenchmarkDraftDependencyInput) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return a.post(route, string(raw))
	}
	var calls, scenario atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input []struct {
				Role    string
				Content string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Input) != 1 || req.Input[0].Role != "user" {
			t.Errorf("bad model request: %+v %v", req, err)
			return
		}
		var data map[string]string
		if err := json.Unmarshal([]byte(req.Input[0].Content), &data); err != nil || len(data) != 3 ||
			!((data["case_key"] == "deep" && data["prerequisite_key"] == "bandwidth") ||
				(data["case_key"] == "bandwidth" && data["prerequisite_key"] == "deep")) {
			t.Errorf("unexpected egress: %v %v", data, err)
		}
		answer := fmt.Sprintf(`{"action":"add_dependency","quote":%q}`, data["text"])
		switch scenario.Load() {
		case 1:
			answer = fmt.Sprintf(`{"action":"add_dependency","quote":%q,"prerequisite_key":"other"}`, data["text"])
		case 2:
			answer = fmt.Sprintf(`{"action":"remove_dependency","quote":%q}`, data["text"])
		case 3:
			if _, err := a.store.SaveBenchmarkDraft(r.Context(), actor, key, work.BenchmarkDraftRequest{ExpectedRevision: draft.Revision + 1, BaseVersion: 1, Cases: cases}); err != nil {
				t.Errorf("competing edit: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer provider.Close()
	settings := &model.Settings{Store: a.store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: provider.URL, ModelID: "fixture", Protocol: model.ProtocolResponses, KeyRef: "env:CLUSBOT_UNUSED_DRAFT_FIXTURE_KEY"}); err != nil {
		t.Fatal(err)
	}
	noConsent := in
	noConsent.ConfirmTextEgress = false
	if code, _ := post(noConsent); code != 400 || calls.Load() != 0 {
		t.Fatalf("consent status=%d calls=%d", code, calls.Load())
	}
	scenario.Store(1)
	if code, _ := post(in); code != 502 {
		t.Fatalf("out-of-contract target change status %d", code)
	}
	scenario.Store(0)
	code, body := post(in)
	if code != 200 {
		t.Fatalf("preview status %d: %v", code, body)
	}
	asJSON, _ := json.Marshal(body)
	var preview work.BenchmarkDraftDependencyPreview
	if err := json.Unmarshal(asJSON, &preview); err != nil || preview.Action != "add_dependency" || preview.State != "preview_only" ||
		preview.CaseKey != "deep" || preview.PrerequisiteKey != "bandwidth" || !slices.Equal(preview.Changes.Changed, []string{"deep"}) ||
		!slices.Equal(preview.ProposedProfile.Cases[1].DependsOn, []string{"bandwidth"}) {
		t.Fatalf("unexpected preview: %+v %v", preview, err)
	}
	current, err := a.store.ReadBenchmarkDraft(ctx, actor, key)
	if err != nil || current.Revision != draft.Revision || len(current.Profile.Cases[1].DependsOn) != 0 {
		t.Fatalf("preview wrote to draft: %+v %v", current, err)
	}
	saved, err := a.store.SaveBenchmarkDraft(ctx, actor, key, work.BenchmarkDraftRequest{BaseVersion: preview.BaseVersion, ExpectedRevision: preview.ExpectedRevision, Cases: preview.ProposedProfile.Cases})
	if err != nil || saved.Published || !slices.Equal(saved.Profile.Cases[1].DependsOn, []string{"bandwidth"}) {
		t.Fatalf("explicit CAS save: %+v %v", saved, err)
	}
	in.ExpectedRevision = saved.Revision
	reverse := in
	reverse.CaseKey, reverse.PrerequisiteKey = in.PrerequisiteKey, in.CaseKey
	reverse.Text = "让 bandwidth 在 deep 之后运行"
	if code, _ := post(reverse); code != 400 {
		t.Fatalf("cycle status %d", code)
	}
	if code, _ := post(in); code != 400 {
		t.Fatalf("no-op add status %d", code)
	}
	scenario.Store(2)
	in.Text = "让 deep 不再依赖 bandwidth"
	code, body = post(in)
	if code != 200 || body["action"] != "remove_dependency" {
		t.Fatalf("remove preview status %d: %v", code, body)
	}
	asJSON, _ = json.Marshal(body)
	preview = work.BenchmarkDraftDependencyPreview{}
	if err := json.Unmarshal(asJSON, &preview); err != nil || len(preview.ProposedProfile.Cases[1].DependsOn) != 0 {
		t.Fatalf("remove preview: %+v %v", preview, err)
	}
	scenario.Store(3)
	if code, _ := post(in); code != 409 {
		t.Fatalf("stale in-flight model output status %d", code)
	}
	if _, err := a.store.SaveBenchmarkDraft(ctx, actor, key, work.BenchmarkDraftRequest{
		BaseVersion: preview.BaseVersion, ExpectedRevision: preview.ExpectedRevision, Cases: preview.ProposedProfile.Cases,
	}); !errors.Is(err, work.ErrConflict) {
		t.Fatalf("stale preview still applied: %v", err)
	}
	published, err := a.store.ReadBenchmarkProfileDetails(ctx, actor, key, 1, 0)
	if err != nil || len(published.Profile.Cases[1].DependsOn) != 0 {
		t.Fatalf("published original changed: %+v %v", published, err)
	}
}
