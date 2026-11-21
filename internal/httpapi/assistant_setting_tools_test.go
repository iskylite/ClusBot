package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/mcpclient"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAssistantSettingToolsKeepSensitiveOperationsInForms(t *testing.T) {
	names := make(map[string]bool)
	for _, tool := range assistantSettingTools() {
		if names[tool.Name] || tool.Name == "" || !strings.HasPrefix(tool.Path, "/api/v1/") {
			t.Fatalf("invalid or duplicate tool: %+v", tool)
		}
		names[tool.Name] = true
		if tool.Method != http.MethodGet && !tool.Confirm {
			t.Fatalf("mutation lacks confirmation: %+v", tool)
		}
		for _, forbidden := range []string{"/settings/model/profiles", "/settings/model/providers", "/me/password", "/permission"} {
			if strings.Contains(tool.Path, forbidden) {
				t.Fatalf("sensitive API exposed as a tool: %+v", tool)
			}
		}
		if (tool.Path == "/api/v1/connections" || tool.Path == "/api/v1/connections/{id}") ||
			strings.Contains(tool.Path, "/settings/model/roles/") {
			t.Fatalf("sensitive connection or role configuration exposed: %+v", tool)
		}
	}
	for _, name := range []string{"configure_model_provider", "configure_model_profile", "configure_model_role", "configure_mcp_connection", "configure_mcp_permission", "manage_accounts", "change_my_password"} {
		if names[name] || assistantSettingForms()[name] == "" {
			t.Fatalf("missing or executable sensitive form: %s", name)
		}
	}
}

func TestAssistantSettingProjectionsOmitCredentialsAndPrivateContext(t *testing.T) {
	ctx := t.Context()
	const envName = "CLUSBOT_ASSISTANT_PROJECTION_KEY"
	const credential = "fixture-secret-value"
	t.Setenv(envName, credential)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"vendor/reported\"}}\n\n")
	}))
	defer provider.Close()

	store, err := work.Open(filepath.Join(t.TempDir(), "assistant-projection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: provider.URL, ModelID: "default-model", Protocol: model.ProtocolResponses, KeyRef: "env:" + envName}); err != nil {
		t.Fatal(err)
	}
	profile, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: "Benchmark", BaseURL: provider.URL, ModelID: "vendor/benchmark", Protocol: model.ProtocolResponses, KeyRef: "env:" + envName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := settings.SetBenchmarkAdmissionRole(ctx, model.BenchmarkAdmissionRoleInput{Action: "bind_profile", ProfileID: profile.ID, ProfileRevision: profile.Revision, ConfirmRequestTextEgress: true}); err != nil {
		t.Fatal(err)
	}
	// An accidentally pasted credential is not safe merely because it is stored in model_id.
	if _, err := settings.SaveProfile(ctx, "", model.ProfileInput{Name: "Unsafe ID", BaseURL: provider.URL, ModelID: credential, Protocol: model.ProtocolResponses, KeyRef: "env:" + envName}); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveConnection(ctx, "", work.Connection{Name: "Fixture MCP", Enabled: true,
		Config: mcpclient.Config{Transport: "stdio", Command: "/private/command", Args: []string{"private-argument"}, Env: []string{"PRIVATE_MCP_TOKEN"}}})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Token: "a1-token", Hosts: []string{"127.0.0.1"}}
	api := &a1Server{ts: httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("fixture")}})), store: store}
	defer api.ts.Close()
	api.cookie = api.login()
	if api.cookie == "" {
		t.Fatal("login failed")
	}
	get := func(path string, authenticated bool) (int, string) {
		t.Helper()
		r := api.request(http.MethodGet, path, "")
		if !authenticated {
			r.Header.Del("Cookie")
		}
		res, err := api.ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if authenticated && res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("projection must not be cached: %s", path)
		}
		return res.StatusCode, string(body)
	}
	for _, path := range []string{"/api/v1/assistant/model-routing", "/api/v1/assistant/models", "/api/v1/assistant/connections"} {
		if code, _ := get(path, false); code != http.StatusUnauthorized {
			t.Fatalf("anonymous projection %s: %d", path, code)
		}
		code, body := get(path, true)
		if code != http.StatusOK {
			t.Fatalf("projection %s: %d %s", path, code, body)
		}
		for _, secret := range []string{provider.URL, credential, envName, "/private/command", "private-argument", "PRIVATE_MCP_TOKEN", `"config"`, `"base_url"`} {
			if strings.Contains(body, secret) {
				t.Fatalf("sensitive configuration in %s: %s", path, body)
			}
		}
		if path == "/api/v1/assistant/model-routing" && (!strings.Contains(body, "default-model") || !strings.Contains(body, "vendor/benchmark")) {
			t.Fatalf("current model routing missing selected models: %s", body)
		}
		if path == "/api/v1/assistant/connections" && !strings.Contains(body, "Fixture MCP") {
			t.Fatalf("connection projection missing metadata: %s", body)
		}
	}

	run, err := store.Submit(ctx, "assistant-model-receipt", "PRIVATE-USER-CONTEXT", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	workers := store.Start(runCtx, settings, 1, work.RunLimits{ActiveTime: 4 * time.Second, ModelTimeout: 3 * time.Second, OutputTokens: 128})
	defer func() { cancel(); workers.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err := store.RunRecord(ctx, run.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != "running" && record.Status != "queued" {
			if record.Status != "completed" {
				t.Fatalf("fixture run failed: %s %s", record.Status, record.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture run did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	path := "/api/v1/assistant/runs/" + run.ID + "/model-receipt"
	if code, _ := get(path, false); code != http.StatusUnauthorized {
		t.Fatalf("anonymous receipt: %d", code)
	}
	code, body := get(path, true)
	if code != http.StatusOK || !strings.Contains(body, `"requested_model":"default-model"`) || !strings.Contains(body, `"reported_model":"vendor/reported"`) || strings.Contains(body, "PRIVATE-USER-CONTEXT") || strings.Contains(body, "fixture answer") {
		t.Fatalf("wrong or overbroad receipt: %d %s", code, body)
	}
}

func TestAssistantModelProjectionRespectsServerAccountPermissions(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(filepath.Join(dir, "assistant-instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: "http://127.0.0.1:1", ModelID: "member-visible-model", Protocol: model.ProtocolResponses, KeyRef: "env:ASSISTANT_MISSING_KEY"}); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, ServerMode: true, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	personalStores := make(map[string]*work.Store)
	server.PersonalAPI = func(principal string) (http.Handler, error) {
		personal := personalStores[principal]
		if personal == nil {
			return nil, errors.New("missing personal fixture")
		}
		return server.PersonalHandler(personal), nil
	}
	handler := server.Handler(fstest.MapFS{})
	const hash = "fixture-password-hash"
	admin, err := store.BootstrapAccount(ctx, "admin", "Admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	session := func(a work.Account, passwordHash string) *http.Cookie {
		t.Helper()
		secret, err := makeSecret()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateAccountSession(ctx, a.PrincipalID, passwordHash, secretDigest(secret), time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: accountCookie, Value: secret}
	}
	adminCookie := session(admin, hash)
	member, err := store.CreateAccount(ctx, secretDigest(adminCookie.Value), "member", "Member", hash, false)
	if err != nil {
		t.Fatal(err)
	}
	initial := session(member, hash)
	const memberHash = "fixture-member-password-hash"
	if err := store.ChangeAccountPassword(ctx, secretDigest(initial.Value), hash, memberHash); err != nil {
		t.Fatal(err)
	}
	memberCookie := session(member, memberHash)
	for _, account := range []work.Account{admin, member} {
		personal, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "personal.sqlite"), store)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = personal.Close() })
		if err := personal.BindAccountBusinessStore(ctx, account.PrincipalID); err != nil {
			t.Fatal(err)
		}
		personalStores[account.PrincipalID] = personal
	}
	privateRun, err := personalStores[admin.PrincipalID].Submit(ctx, "admin-private-run", "PRIVATE ADMIN DATA", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "https://ops.example"+path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request("/api/v1/assistant/model-routing", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous current model: %d", got)
	}
	current := request("/api/v1/assistant/model-routing", memberCookie)
	var out model.ModelRoutingView
	if current.Code != http.StatusOK || json.Unmarshal(current.Body.Bytes(), &out) != nil || len(out.Roles) == 0 || out.Roles[0].ModelID != "member-visible-model" {
		t.Fatalf("member cannot read safe current model: %d %s", current.Code, current.Body)
	}
	for _, path := range []string{"/api/v1/settings", "/api/v1/settings/model/routing", "/api/v1/assistant/models"} {
		if got := request(path, memberCookie).Code; got != http.StatusForbidden {
			t.Fatalf("member can read restricted %s: %d", path, got)
		}
	}
	if got := request("/api/v1/assistant/models", adminCookie).Code; got != http.StatusOK {
		t.Fatalf("admin model catalog: %d", got)
	}
	privateReceipt := "/api/v1/assistant/runs/" + privateRun.ID + "/model-receipt"
	if got := request(privateReceipt, adminCookie).Code; got != http.StatusOK {
		t.Fatalf("owner cannot read own receipt: %d", got)
	}
	if got := request(privateReceipt, memberCookie).Code; got != http.StatusNotFound {
		t.Fatalf("member can read another account's receipt: %d", got)
	}
}
