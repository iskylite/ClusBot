package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestGeneralRouterRoleHTTPPreservesAdmissionAndModelOff(t *testing.T) {
	const credential = "env:CLUSBOT_ROUTER_HTTP_FIXTURE"
	t.Setenv("CLUSBOT_ROUTER_HTTP_FIXTURE", "local-fixture")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "router" {
			t.Errorf("unexpected Router fixture model: %+v %v", req, err)
		}
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", `{"kind":"chat"}`)
		reported := "vendor/router-version"
		if attempt == 2 {
			reported = "local-fixture" // matches the credential; never a model identifier in evidence
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", reported)
	}))
	defer srv.Close()
	store, err := work.Open(filepath.Join(t.TempDir(), "router-role.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: store}
	ctx := t.Context()
	if _, err := settings.Initialize(ctx, work.ModelSettings{Protocol: model.ProtocolResponses, BaseURL: srv.URL, ModelID: "default", KeyRef: credential}); err != nil {
		t.Fatal(err)
	}
	profile, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: "Router", Protocol: model.ProtocolResponses, BaseURL: srv.URL, ModelID: "router", KeyRef: credential})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	api := &a1Server{ts: httptest.NewServer(businessFixtureHandler(t, server)), store: store}
	defer api.close()
	api.cookie = api.login()
	if api.cookie == "" {
		t.Fatal("unable to authenticate to fixture")
	}
	call := func(method, path, body string, status int, out any) {
		t.Helper()
		r, err := api.ts.Client().Do(api.request(method, path, body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil || r.StatusCode != status {
			t.Fatalf("%s %s: %d %s (%v)", method, path, r.StatusCode, data, err)
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var role model.BenchmarkAdmissionRoleView
	const path = "/api/v1/settings/model/roles/general-router"
	call(http.MethodGet, path, "", http.StatusOK, &role)
	if role.Role != work.GeneralRouterModelRole || len(role.EgressFields) != 5 || role.Source != "legacy_default" {
		t.Fatalf("role must disclose scope before consent: %+v", role)
	}
	call(http.MethodPut, path, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_request_text_egress":true}`, profile.ID), http.StatusOK, &role)
	if role.Source != "explicit_profile" || role.Status != "locally_configured" || calls.Load() != 0 {
		t.Fatalf("bind unexpectedly performed inference: %+v calls=%d", role, calls.Load())
	}

	const route = "/api/v1/conversations"
	const message = `{"client_request_id":"router-role-live","text":"这个问题怎么理解？"}`
	var original, replay struct {
		RunID string `json:"run_id"`
	}
	call(http.MethodPost, route, message, http.StatusAccepted, &original)
	call(http.MethodPost, route, message, http.StatusAccepted, &replay)
	if original.RunID == "" || original != replay || calls.Load() != 1 {
		t.Fatalf("rebinding Router path changed admission replay: %+v %+v calls=%d", original, replay, calls.Load())
	}

	var saved, repeated struct {
		RouterModel *work.RouterModelProvenance `json:"router_model"`
	}
	const liveReceipt = "/api/v1/admissions/router-role-live"
	call(http.MethodGet, liveReceipt, "", http.StatusOK, &saved)
	if saved.RouterModel == nil || saved.RouterModel.Source != "explicit_profile" ||
		saved.RouterModel.ProfileID != profile.ID || saved.RouterModel.RoleRevision != role.Revision ||
		saved.RouterModel.ProfileRevision != 1 || saved.RouterModel.State != "reported" ||
		saved.RouterModel.RequestedModel != "router" || saved.RouterModel.ReportedModel != "vendor/router-version" {
		t.Fatalf("missing exact classification evidence: %+v", saved.RouterModel)
	}

	base, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.ModelID = ""
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	var pending struct {
		ModelPending bool `json:"model_pending"`
	}
	call(http.MethodPost, route, `{"client_request_id":"router-role-no-model","text":"这条只应持久挂起"}`, http.StatusAccepted, &pending)
	if !pending.ModelPending || calls.Load() != 1 {
		t.Fatalf("configured role bypassed disabled default: %+v calls=%d", pending, calls.Load())
	}
	code, receipt := api.getAdmission("router-role-no-model")
	if code != http.StatusOK || receipt["state"] != "received" || receipt["router_model"] != nil {
		t.Fatalf("unclassified message did not remain in the existing durable admission: %d %+v", code, receipt)
	}
	base, err = store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.ModelID = "restored-default"
	if _, err := store.SaveModelSettings(ctx, base); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.ProcessChatAdmissions(ctx, settings.RouteSubmission); err != nil {
			t.Fatal(err)
		}
	}
	code, receipt = api.getAdmission("router-role-no-model")
	if code != http.StatusOK || receipt["state"] != "applied" || calls.Load() != 2 {
		t.Fatalf("existing durable processing did not classify once using current pinned role: %d %+v calls=%d", code, receipt, calls.Load())
	}
	var recovered struct {
		RouterModel *work.RouterModelProvenance `json:"router_model"`
	}
	call(http.MethodGet, "/api/v1/admissions/router-role-no-model", "", http.StatusOK, &recovered)
	if recovered.RouterModel == nil || recovered.RouterModel.State != "not_reported" ||
		recovered.RouterModel.RequestedModel != "router" || recovered.RouterModel.ReportedModel != "" {
		t.Fatalf("credential leaked or model evidence fabricated: %+v", recovered.RouterModel)
	}
	call(http.MethodGet, liveReceipt, "", http.StatusOK, &repeated)
	if repeated.RouterModel == nil || *repeated.RouterModel != *saved.RouterModel || calls.Load() != 2 {
		t.Fatalf("stored model evidence changed after restoring a different default: %+v", repeated.RouterModel)
	}
	// Stale credentials prevent selection before any outgoing model request.
	t.Setenv("CLUSBOT_ROUTER_HTTP_FIXTURE", "another-local-fixture")
	call(http.MethodPost, route, `{"client_request_id":"router-role-stale","text":"这条不能回退到其他模型"}`, http.StatusAccepted, nil)
	var stageWait struct {
		State       string                      `json:"state"`
		Reason      string                      `json:"reason"`
		RouterModel *work.RouterModelProvenance `json:"router_model"`
	}
	call(http.MethodGet, "/api/v1/admissions/router-role-stale", "", http.StatusOK, &stageWait)
	if stageWait.State != "received" || !strings.Contains(stageWait.Reason, "路由模型") || stageWait.RouterModel != nil || calls.Load() != 2 {
		t.Fatalf("invalid binding was rejected, rerouted, or misrepresented as a call: %+v calls=%d", stageWait, calls.Load())
	}

}
