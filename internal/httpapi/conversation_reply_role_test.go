package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestConversationReplyRoleHTTPRequiresDistinctConsent(t *testing.T) {
	const ref = "env:CLUSBOT_REPLY_HTTP_FIXTURE"
	t.Setenv("CLUSBOT_REPLY_HTTP_FIXTURE", "not-a-real-credential")
	const endpoint = "http://127.0.0.1:1"
	store, err := work.Open(filepath.Join(t.TempDir(), "conversation-reply-role.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: endpoint, ModelID: "default", Protocol: model.ProtocolResponses, KeyRef: ref}); err != nil {
		t.Fatal(err)
	}
	profile, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: "Reply", BaseURL: endpoint, ModelID: "reply", Protocol: model.ProtocolResponses, KeyRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Token: "a1-token", Hosts: []string{"127.0.0.1"}}
	api := &a1Server{ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})), store: store}
	defer api.close()
	api.cookie = api.login()
	if api.cookie == "" {
		t.Fatal("fixture login failed")
	}
	call := func(method, body string, status int) model.BenchmarkAdmissionRoleView {
		t.Helper()
		res, err := api.ts.Client().Do(api.request(method, "/api/v1/settings/model/roles/conversation-reply", body))
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
	if v.Role != work.ConversationReplyModelRole || v.Source != "legacy_default" || len(v.EgressFields) != 5 {
		t.Fatalf("reply capability not disclosed: %+v", v)
	}
	call(http.MethodPut, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_request_text_egress":true}`, profile.ID), http.StatusBadRequest)
	v = call(http.MethodPut, fmt.Sprintf(`{"action":"bind_profile","profile_id":%q,"profile_revision":1,"confirm_conversation_context_egress":true}`, profile.ID), http.StatusOK)
	if v.Status != "locally_configured" || v.Revision != 1 || v.ProfileID != profile.ID {
		t.Fatalf("invalid opt-in receipt: %+v", v)
	}
}
