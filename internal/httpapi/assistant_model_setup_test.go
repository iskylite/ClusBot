package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAssistantModelSetupResolvesOnlyUniqueLiveNamesWithoutWrites(t *testing.T) {
	ctx := t.Context()
	store, err := work.Open(filepath.Join(t.TempDir(), "setup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const secret = "fixture-secret-value-123"
	t.Setenv("ASSISTANT_SETUP_TEST_KEY", secret)
	settings := &model.Settings{Store: store}
	provider, err := settings.SaveProvider(ctx, "", model.ProviderInput{Name: "Anthropic", Protocol: model.ProtocolResponses, BaseURL: "https://example.invalid", AuthMode: "environment", KeyRef: "env:ASSISTANT_SETUP_TEST_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := settings.SaveCatalogModel(ctx, "", model.CatalogModelInput{Name: "Qwen3.5-235B", ProviderID: provider.ID, ProviderRevision: provider.Revision, ModelID: "qwen-235b"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.SaveCatalogModel(ctx, "", model.CatalogModelInput{Name: "Qwen3.5-235B-Long", ProviderID: provider.ID, ProviderRevision: provider.Revision, ModelID: "qwen-235b-long"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings}
	call := func(text string) (bool, *httptest.ResponseRecorder) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", nil)
		res := httptest.NewRecorder()
		return server.assistantModelSetupChat(res, req.WithContext(context.WithValue(req.Context(), accountContextKey{}, work.Account{IsSuperadmin: true})), text), res
	}
	const pastedSecret = "sk-pastedsecret123456"
	handled, response := call("把任务执行切到 Qwen3.5-235B，先预览影响；顺便把 Anthropic 凭据补上，API Key: " + pastedSecret)
	if !handled || response.Code != http.StatusOK {
		t.Fatalf("setup: %v %d %s", handled, response.Code, response.Body.String())
	}
	var batch struct {
		State string               `json:"state"`
		Items []assistantSetupForm `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil || batch.State != "batch_completed" || len(batch.Items) != 2 {
		t.Fatalf("setup receipt: %+v %v", batch, err)
	}
	role := batch.Items[0]
	if role.Tool != "role_binding_prefill" || role.Prefill.Role != work.TaskWorkerModelRole || role.Prefill.ProfileID != profile.ID || role.Prefill.ProfileRevision != profile.Revision || role.Prefill.ProviderRevision != provider.Revision {
		t.Fatalf("wrong role prefill: %+v", role)
	}
	credentials := batch.Items[1]
	if credentials.Tool != "provider_credentials_prefill" || credentials.Prefill.ProviderID != provider.ID || credentials.Prefill.ProviderRevision != provider.Revision {
		t.Fatalf("wrong provider prefill: %+v", credentials)
	}
	for _, private := range []string{pastedSecret, secret, "ASSISTANT_SETUP_TEST_KEY", "https://example.invalid"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("sensitive data in setup receipt: %q", private)
		}
	}
	policies, err := settings.ModelRoutingPolicies(ctx)
	if err != nil || len(policies) != 0 {
		t.Fatalf("preview changed role policy: %+v %v", policies, err)
	}
	providers, err := settings.Providers(ctx)
	if err != nil || len(providers) != 1 || providers[0].Revision != provider.Revision || providers[0].CredentialStatus != "configured" {
		t.Fatalf("preview changed provider: %+v %v", providers, err)
	}

	// A shared model ID is ambiguous even if each profile's display name differs.
	other, err := settings.SaveProvider(ctx, "", model.ProviderInput{Name: "Alternative provider", Protocol: model.ProtocolResponses, BaseURL: "https://other.invalid", AuthMode: "environment", KeyRef: "env:ASSISTANT_SETUP_TEST_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.SaveCatalogModel(ctx, "", model.CatalogModelInput{Name: "Alternative", ProviderID: other.ID, ProviderRevision: other.Revision, ModelID: "qwen-235b"})
	if err != nil {
		t.Fatal(err)
	}
	handled, response = call("把任务执行切到 qwen-235b")
	var ambiguous assistantSetupForm
	if err := json.Unmarshal(response.Body.Bytes(), &ambiguous); !handled || err != nil || ambiguous.Prefill.Role != work.TaskWorkerModelRole || ambiguous.Prefill.ProfileID != "" {
		t.Fatalf("ambiguous model must require a selection: %+v %v", ambiguous, err)
	}
	handled, response = call("给不存在的提供方配置凭据")
	var unknown assistantSetupForm
	if err := json.Unmarshal(response.Body.Bytes(), &unknown); !handled || err != nil || unknown.Prefill.ProviderID != "" {
		t.Fatalf("unknown provider must require a selection: %+v %v", unknown, err)
	}
	for _, text := range []string{"不要把任务执行切到 Qwen3.5-235B", "不切换任务执行到 Qwen3.5-235B", "不修改 Anthropic 凭据"} {
		if handled, _ := call(text); handled {
			t.Fatalf("negative request was treated as a change: %s", text)
		}
	}
	if handled, _ := call("解释一下任务执行角色"); handled {
		t.Fatal("ordinary question was treated as a change")
	}
}

func TestAssistantModelSetupRequiresAdminPermission(t *testing.T) {
	server := Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", nil)
	for _, account := range []work.Account{{}, {IsSuperadmin: true, MustChangePassword: true}} {
		req := request.WithContext(context.WithValue(request.Context(), accountContextKey{}, account))
		response := httptest.NewRecorder()
		if !server.assistantModelSetupChat(response, req, "把任务执行切到 example") || response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "example") {
			t.Fatalf("denied request: %d %s", response.Code, response.Body.String())
		}
	}
	request = request.WithContext(context.WithValue(request.Context(), accountContextKey{}, work.Account{IsSuperadmin: true}))
	request.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
	response := httptest.NewRecorder()
	if !server.assistantModelSetupChat(response, request, "把任务执行切到 example") || response.Code != http.StatusForbidden {
		t.Fatalf("read-only request was not denied: %d %s", response.Code, response.Body.String())
	}
}

func TestAssistantModelSetupMentionUsesIdentifierBoundaries(t *testing.T) {
	for _, test := range []struct {
		text, name string
		want       bool
	}{
		{"切到 Qwen3.5-235B", "Qwen3.5-235B", true},
		{"切到 Qwen3.5-235B-Long", "Qwen3.5-235B", false},
		{"把任务执行切到模型", "任务执行", true},
		{"切到 qwen-235b-long", "qwen-235b", false},
	} {
		if got := assistantSetupMention(test.text, test.name); got != test.want {
			t.Errorf("mention %q in %q = %v", test.name, test.text, got)
		}
	}
}
