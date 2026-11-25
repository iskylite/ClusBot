package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestTaskWorkerRoleHTTPDisclosesConsentAndRejectsOtherReferences(t *testing.T) {
	const ref = "env:CLUSBOT_WORKER_HTTP_FIXTURE"
	t.Setenv("CLUSBOT_WORKER_HTTP_FIXTURE", "local-test-only")
	t.Setenv("CLUSBOT_WORKER_HTTP_DIFFERENT_REF", "local-test-only")
	const endpoint = "http://127.0.0.1:1"
	store, err := work.Open(filepath.Join(t.TempDir(), "task-worker-role.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: endpoint, ModelID: "default", Protocol: model.ProtocolResponses, KeyRef: ref}); err != nil {
		t.Fatal(err)
	}
	save := func(name, ep, key, protocol string) model.ProfileView {
		t.Helper()
		p, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: name, BaseURL: ep, ModelID: name, Protocol: protocol, KeyRef: key})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	profile := save("role", endpoint, ref, model.ProtocolResponses)
	also := []model.ProfileView{
		save("otherEndpoint", endpoint+"/different", ref, model.ProtocolResponses),
		save("sameSecretDifferentRef", endpoint, "env:CLUSBOT_WORKER_HTTP_DIFFERENT_REF", model.ProtocolResponses),
		save("differentProtocol", endpoint, ref, model.ProtocolChat),
	}
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	api := &a1Server{ts: httptest.NewServer(businessFixtureHandler(t, server)), store: store}
	defer api.close()
	api.cookie = api.login()
	if api.cookie == "" {
		t.Fatal("fixture login failed")
	}
	call := func(method, body string, status int) model.BenchmarkAdmissionRoleView {
		t.Helper()
		res, err := api.ts.Client().Do(api.request(method, "/api/v1/settings/model/roles/task-worker", body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("%s: %d %s %v", method, res.StatusCode, data, err)
		}
		var v model.BenchmarkAdmissionRoleView
		if status == http.StatusOK {
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	v := call(http.MethodGet, "", http.StatusOK)
	if v.Role != work.TaskWorkerModelRole || v.Source != "legacy_default" || len(v.EgressFields) < 8 {
		t.Fatalf("Worker context not disclosed before consent: %+v", v)
	}
	call(http.MethodPut, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_request_text_egress":true}`, profile.ID), http.StatusBadRequest)
	call(http.MethodPut, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_conversation_context_egress":true}`, profile.ID), http.StatusBadRequest)
	for _, p := range also {
		call(http.MethodPut, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_task_context_egress":true}`, p.ID), http.StatusBadRequest)
	}
	body := fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_task_context_egress":true}`, profile.ID)
	v = call(http.MethodPut, body, http.StatusOK)
	if v.Status != "locally_configured" || v.Revision != 1 || v.ProfileID != profile.ID {
		t.Fatalf("invalid opt-in receipt: %+v", v)
	}
	call(http.MethodPut, body, http.StatusConflict)
}
