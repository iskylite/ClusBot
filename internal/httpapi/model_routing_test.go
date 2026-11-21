package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestModelRoutingHTTPUsesAuthenticatedReadOnlyPreview(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "routing-api.sqlite"))
	defer a.close()
	const route = "/api/v1/settings/model/routing"
	request := a.request(http.MethodGet, route, "")
	request.Header.Del("Cookie")
	res, err := a.ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous routing status: %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, route, ""))
	if err != nil {
		t.Fatal(err)
	}
	var overview model.ModelRoutingView
	err = json.NewDecoder(res.Body).Decode(&overview)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || len(overview.Roles) != 7 || !overview.OrderedFallbackSupported || !overview.BusinessOverridesSupported {
		t.Fatalf("routing overview: %+v, %d, %v", overview, res.StatusCode, err)
	}
	if code, body := a.post(route+"/preview", `{"role":"deterministic"}`); code != http.StatusOK || body["source"] != "go" {
		t.Fatalf("Go stage preview: %d %+v", code, body)
	}
	if code, _ := a.post(route+"/preview", `{"role":"fake_role"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown role accepted: %d", code)
	}
}

func TestModelRoutingPolicyHTTPWriteReadPreviewAndConflict(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "routing-policy-api.sqlite"))
	defer a.close()
	settings := &model.Settings{Store: a.store}
	profile, err := settings.SaveProfile(t.Context(), "", model.ProfileInput{Name: "Routing fixture", Protocol: model.ProtocolResponses, BaseURL: "http://127.0.0.1:1/v1", ModelID: "fixture-model", KeyRef: "none"})
	if err != nil {
		t.Fatal(err)
	}
	const route = "/api/v1/settings/model/routing"
	path := route + "/policies/conversation/conversation_reply"
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		res, err := a.ts.Client().Do(a.request(method, path, body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s %s: %d %s %v", method, path, res.StatusCode, data, err)
		}
		return data
	}
	body := fmt.Sprintf(`{"revision":0,"candidates":[{"profile_id":%q,"profile_revision":1}],"confirm_egress":true}`, profile.ID)
	var saved model.RoutingPolicyView
	if err := json.Unmarshal(call(http.MethodPut, path, body, http.StatusOK), &saved); err != nil || saved.Revision != 1 || len(saved.Candidates) != 1 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	call(http.MethodPut, path, body, http.StatusConflict)
	data := call(http.MethodGet, route+"/policies", "", http.StatusOK)
	var list struct {
		Policies []model.RoutingPolicyView `json:"policies"`
	}
	if err := json.Unmarshal(data, &list); err != nil || len(list.Policies) != 1 || list.Policies[0].Candidates[0].ModelID != "fixture-model" {
		t.Fatalf("list: %s %v", data, err)
	}
	code, preview := a.post(route+"/preview", fmt.Sprintf(`{"role":%q,"business":"conversation"}`, work.ConversationReplyModelRole))
	if code != http.StatusOK || preview["source"] != "business_override" || preview["model_id"] != "fixture-model" {
		t.Fatalf("preview: %d %+v", code, preview)
	}
	call(http.MethodPut, path, `{"revision":1,"candidates":[]}`, http.StatusOK)
}
