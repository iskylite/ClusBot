package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestBenchmarkDraftPreviewOnlySendsExplicitFieldAndText(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "draft-parameter.sqlite"))
	defer a.close()
	ctx := t.Context()
	const actor, key = "user:session", "profile-secret"
	const spoken = "默认下一版这项卡数降到32，实测也核这个数"
	const route = "/api/v1/domain/benchmark-profiles/" + key + "/draft/preview-parameter-edit"
	c := work.BenchmarkCase{Key: "deep", Binding: work.BenchmarkBinding{Connection: "private-connection", Revision: 1, Tool: "private-tool", Validator: "structured-metrics@1"},
		Targets: []string{"private-target"}, Parameters: map[string]string{"gpu_count": "40"}, ActualConfig: map[string]string{"gpu_count": "40"},
		NeedsRawEvidence: true, RequiredMetrics: map[string]string{"rate": "GB/s"}, ComparisonKeys: []string{"gpu_count"},
		Threshold: &work.BenchmarkMetricThreshold{Metric: "rate", Minimum: 12345}}
	if err := a.store.PublishBenchmarkProfile(ctx, actor, work.BenchmarkProfile{Key: key, Version: 1, Cases: []work.BenchmarkCase{c}}); err != nil {
		t.Fatal(err)
	}
	first, err := a.store.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	input := work.BenchmarkDraftParameterInput{BaseVersion: 1, ExpectedRevision: first.Revision, CaseKey: c.Key, Parameter: "gpu_count", Text: spoken,
		ConfirmTextEgress: true, AllowExpectedActualChange: true}
	post := func(path string, in work.BenchmarkDraftParameterInput) (int, map[string]any) {
		t.Helper()
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return a.post(path, string(data))
	}
	// There is no default provider yet; no local edit is attempted.
	if code, _ := post(route, input); code != http.StatusBadGateway {
		t.Fatalf("unconfigured model status %d", code)
	}
	var calls, scenario atomic.Int32
	const answer = `{"action":"set_parameter","case_key":"deep","parameter":"gpu_count","value":"32","change_expected_actual_config":true,"quote":"` + spoken + `"}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model string
			Input []struct {
				Role    string
				Content string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "fixture" || len(req.Input) != 1 || req.Input[0].Role != "user" {
			t.Errorf("wrong model input shape: %+v %v", req, err)
			return
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(req.Input[0].Content), &payload); err != nil || len(payload) != 3 ||
			payload["text"] != spoken || payload["case_key"] != c.Key || payload["parameter"] != "gpu_count" {
			t.Errorf("unexpected data in egress: %v %v", payload, err)
		}
		switch scenario.Load() {
		case 1:
			// Same UI edits the draft while this model request is in flight.
			other := c
			other.Threshold = &work.BenchmarkMetricThreshold{Metric: "rate", Minimum: 1000}
			if _, err := a.store.SaveBenchmarkDraft(r.Context(), actor, key, work.BenchmarkDraftRequest{
				ExpectedRevision: first.Revision, BaseVersion: 1, Cases: []work.BenchmarkCase{other},
			}); err != nil {
				t.Errorf("competing draft edit: %v", err)
			}
		case 2:
			http.Error(w, "not retriable for one-shot preview", http.StatusServiceUnavailable)
			return
		}
		out := answer
		switch scenario.Load() {
		case 3:
			out = strings.Replace(answer, `"case_key":"deep"`, `"case_key":"elsewhere"`, 1)
		case 4:
			out = strings.Replace(answer, `"value":"32"`, `"value":"2"`, 1)
		case 5:
			out = `{"action":"remove_case","case_key":"deep","quote":"` + spoken + `"}`
		case 6:
			out = strings.Replace(answer, `"change_expected_actual_config":true`, `"change_expected_actual_config":false`, 1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	defer provider.Close()
	settings := &model.Settings{Store: a.store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: provider.URL, ModelID: "fixture", Protocol: model.ProtocolResponses, KeyRef: "env:CLUSBOT_UNUSED_DRAFT_FIXTURE_KEY"}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		change func(*work.BenchmarkDraftParameterInput)
		code   int
	}{
		{func(v *work.BenchmarkDraftParameterInput) { v.AllowExpectedActualChange = false }, 400},
		{func(v *work.BenchmarkDraftParameterInput) { v.ConfirmTextEgress = false }, 400},
		{func(v *work.BenchmarkDraftParameterInput) { v.ExpectedRevision++ }, 409},
		{func(v *work.BenchmarkDraftParameterInput) { v.CaseKey = "bad" }, 400},
		{func(v *work.BenchmarkDraftParameterInput) { v.Parameter = "bad" }, 400},
	} {
		v := input
		check.change(&v)
		if code, _ := post(route, v); code != check.code {
			t.Fatalf("invalid request expected %d got %d", check.code, code)
		}
	}
	req := a.request(http.MethodPost, route, `{}`)
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatal("unauthorized/invalid requests reached the model")
	}
	if code, out := post(route, input); code != 200 || out["state"] != "preview_only" {
		t.Fatalf("preview %d %+v", code, out)
	} else {
		serialized, _ := json.Marshal(out)
		var preview work.BenchmarkDraftParameterPreview
		if err := json.Unmarshal(serialized, &preview); err != nil {
			t.Fatal(err)
		}
		want := c
		want.Parameters = map[string]string{"gpu_count": "32"}
		want.ActualConfig = map[string]string{"gpu_count": "32"}
		wanted, _ := json.Marshal(want)
		got, _ := json.Marshal(preview.ProposedProfile.Cases[0])
		if string(got) != string(wanted) || preview.ExpectedRevision != first.Revision || len(preview.Changes.Changed) != 1 {
			t.Fatalf("paired-only proposal changed another field: %s versus %s", got, wanted)
		}
	}
	draft, err := a.store.ReadBenchmarkDraft(ctx, actor, key)
	if err != nil || draft.Profile.Cases[0].Parameters["gpu_count"] != "40" || draft.Revision != first.Revision || draft.Published {
		t.Fatalf("preview mutated the draft: %+v %v", draft, err)
	}
	for mode, want := range map[int32]int{2: 502, 3: 400, 4: 502, 5: 400, 6: 400} {
		scenario.Store(mode)
		old := calls.Load()
		if code, out := post(route, input); code != want || calls.Load() != old+1 {
			t.Fatalf("invalid provider mode %d HTTP %d response %+v (calls %d → %d)", mode, code, out, old, calls.Load())
		}
	}
	scenario.Store(1)
	if code, out := post(route, input); code != http.StatusConflict {
		t.Fatalf("concurrent draft change not rechecked after model: %d %+v", code, out)
	}
}
