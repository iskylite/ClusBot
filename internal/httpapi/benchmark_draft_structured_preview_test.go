package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"clusbot/internal/work"
)

func TestBenchmarkDraftStructuredPreview(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "draft-structured.sqlite"))
	defer a.close()
	ctx := t.Context()
	const actor, key = "user:session", "pod7"
	const draftURL = "/api/v1/domain/benchmark-profiles/" + key + "/draft"
	const route = draftURL + "/preview-structured-edit"
	newCase := func(key string, deps ...string) work.BenchmarkCase {
		return work.BenchmarkCase{Key: key, DependsOn: deps,
			Binding: work.BenchmarkBinding{Connection: "fixture", Revision: 1, Tool: "case", Validator: "structured-metrics@1"},
			Targets: []string{"local"}, Parameters: map[string]string{"cards": "40"}, ActualConfig: map[string]string{"cards": "40"},
			RequiredMetrics: map[string]string{"rate": "GB/s"}, Threshold: &work.BenchmarkMetricThreshold{Metric: "rate", Minimum: 100}}
	}
	original := []work.BenchmarkCase{newCase("bandwidth"), newCase("obsolete"), newCase("deep", "obsolete")}
	if err := a.store.PublishBenchmarkProfile(ctx, actor, work.BenchmarkProfile{Key: key, Version: 1, Cases: original}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetBenchmarkChatTarget(ctx, actor, "POD7", &work.BenchmarkProfileRef{Profile: key, Version: 1}); err != nil {
		t.Fatal(err)
	}
	// Saving incomplete work is permitted. The read-only preview can show
	// how the new valid batch repairs it, without mutating the stored draft.
	incomplete := []work.BenchmarkCase{original[0], original[2]}
	first, err := a.store.SaveBenchmarkDraft(ctx, actor, key, work.BenchmarkDraftRequest{BaseVersion: 1, Cases: incomplete})
	if err != nil || first.ValidationError == "" {
		t.Fatalf("save incomplete draft: %+v %v", first, err)
	}
	bandwidth := newCase("bandwidth", "preflight")
	bandwidth.NeedsRawEvidence = true
	bandwidth.RequiredMetrics = map[string]string{"rate": "GB/s", "p99": "ms"}
	bandwidth.Threshold.Minimum = 125
	preflight := newCase("preflight")
	req := work.BenchmarkDraftRequest{BaseVersion: 1, ExpectedRevision: first.Revision,
		Cases: []work.BenchmarkCase{preflight, bandwidth}}
	send := func(method, path string, in work.BenchmarkDraftRequest, authenticated bool) (int, string, []byte) {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		request := a.request(method, path, string(raw))
		if !authenticated {
			request.Header.Del("Cookie")
		}
		resp, err := a.ts.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Cache-Control"), payload
	}
	if status, _, _ := send(http.MethodPost, route, req, false); status != 401 {
		t.Fatalf("unauthenticated preview = %d", status)
	}
	badEvidence := req
	badEvidence.Cases = slices.Clone(req.Cases)
	badEvidence.Cases[1].Threshold = &work.BenchmarkMetricThreshold{Metric: "not-required", Minimum: 125}
	if status, _, _ := send(http.MethodPost, route, badEvidence, true); status != 400 {
		t.Fatalf("invalid evidence/threshold preview = %d", status)
	}
	badCycle := req
	badCycle.Cases = slices.Clone(req.Cases)
	badCycle.Cases[0].DependsOn = []string{"bandwidth"}
	if status, _, _ := send(http.MethodPost, route, badCycle, true); status != 400 {
		t.Fatalf("cycle preview = %d", status)
	}
	status, cache, raw := send(http.MethodPost, route, req, true)
	var preview work.BenchmarkDraftStructuredPreview
	if status != 200 || cache != "no-store" {
		t.Fatalf("preview %d cache=%q: %s", status, cache, raw)
	}
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.State != "preview_only" || preview.BaseVersion != 1 || preview.ExpectedRevision != first.Revision ||
		!slices.Equal(preview.Changes.Added, []string{"preflight"}) || !slices.Equal(preview.Changes.Removed, []string{"deep"}) ||
		!slices.Equal(preview.Changes.Changed, []string{"bandwidth"}) || preview.Changes.OrderChanged {
		t.Fatalf("wrong delta: %+v", preview)
	}
	old, proposed := preview.CurrentProfile.Cases[0], preview.ProposedProfile.Cases[1]
	if old.NeedsRawEvidence || old.Threshold.Minimum != 100 || !proposed.NeedsRawEvidence || proposed.Threshold.Minimum != 125 ||
		old.RequiredMetrics["p99"] != "" || proposed.RequiredMetrics["p99"] != "ms" || !slices.Equal(proposed.DependsOn, []string{"preflight"}) {
		t.Fatalf("lost exact evidence, threshold or dependency before/after: %+v -> %+v", old, proposed)
	}
	current, err := a.store.ReadBenchmarkDraft(ctx, actor, key)
	if err != nil || current.ValidationError == "" || current.Revision != first.Revision || len(current.Profile.Cases[0].DependsOn) != 0 {
		t.Fatalf("preview changed draft: %+v %v", current, err)
	}
	save := work.BenchmarkDraftRequest{BaseVersion: preview.BaseVersion, ExpectedRevision: preview.ExpectedRevision, Cases: preview.ProposedProfile.Cases}
	if status, _, body := send(http.MethodPut, draftURL, save, true); status != 200 {
		t.Fatalf("save = %d: %s", status, body)
	}
	if status, _, _ := send(http.MethodPost, route, req, true); status != 409 {
		t.Fatalf("stale preview request = %d", status)
	}
	if status, _, _ := send(http.MethodPut, draftURL, save, true); status != 409 {
		t.Fatalf("stale preview save = %d", status)
	}
	if _, err := a.store.ReadBenchmarkProfileDetails(ctx, actor, key, 2, 0); err == nil {
		t.Fatal("preview/save unexpectedly published draft")
	}
	active, err := a.store.ReadBenchmarkChatTarget(ctx, actor, "POD7")
	if err != nil || active.Profile != key || active.Version != 1 {
		t.Fatalf("preview changed default: %+v %v", active, err)
	}
}
