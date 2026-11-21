package httpapi

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestComposerOptionsServerModeAndBrandInference(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	settings := &model.Settings{Store: store}
	ctx := context.Background()
	if _, err := settings.Initialize(ctx, work.ModelSettings{
		BaseURL:  "https://api.deepseek.com/v1",
		ModelID:  "deepseek-chat",
		Protocol: model.ProtocolResponses,
		KeyRef:   "env:DEEPSEEK_API_KEY_TEST",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY_TEST", "dummy")

	var mu sync.Mutex
	stores := map[string]*work.Store{}
	openPersonal := func(owner string) (*work.Store, error) {
		mu.Lock()
		defer mu.Unlock()
		if existing := stores[owner]; existing != nil {
			return existing, nil
		}
		ps, err := work.Open(filepath.Join(t.TempDir(), strings.ReplaceAll(owner, ":", "_")+".db"))
		if err != nil {
			return nil, err
		}
		stores[owner] = ps
		t.Cleanup(func() { _ = ps.Close() })
		return ps, nil
	}

	var srv Server
	srv = Server{
		Store:      store,
		Settings:   settings,
		ServerMode: true,
		Origin:     "https://ops.example",
		Hosts:      []string{"ops.example"},
		Token:      "legacy-token",
		AdminTaskStores: func() map[string]*work.Store {
			mu.Lock()
			defer mu.Unlock()
			out := make(map[string]*work.Store, len(stores))
			for k, v := range stores {
				out[k] = v
			}
			return out
		},
		PersonalAPI: func(owner string) (http.Handler, error) {
			ps, err := openPersonal(owner)
			if err != nil {
				return nil, err
			}
			return srv.PersonalHandler(ps), nil
		},
	}

	f := &accountFixture{
		t:       t,
		store:   store,
		handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}}),
	}
	adminID := f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")

	w := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username":     "chen",
		"display_name": "陈工",
		"password":     "member password 123",
	}, adminCookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", w.Code, w.Body)
	}
	var created struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, w, &created)
	memberCookie := f.login("chen", "member password 123")

	adminPS, err := openPersonal(adminID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminPS.Submit(ctx, "cmp-admin", "超管任务", "", "", true); err != nil {
		t.Fatal(err)
	}
	memberPS, err := openPersonal(created.User.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memberPS.Submit(ctx, "cmp-member", "成员任务", "", "", true); err != nil {
		t.Fatal(err)
	}

	// Regular member only gets their own tasks, plus active mentionable users and slash commands
	w = f.request("GET", "/api/v1/assistant/composer-options?scope=all", nil, memberCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("member composer-options: %d %s", w.Code, w.Body)
	}
	var memberResp composerOptionsResponse
	accountJSON(t, w, &memberResp)
	if len(memberResp.SlashCommands) < 3 {
		t.Fatalf("expected the supported composer shortcuts, got %+v", memberResp.SlashCommands)
	}
	for _, command := range memberResp.SlashCommands {
		if command.Command == "/benchmark" {
			t.Fatal("advertised benchmark dispatch without a benchmark provider")
		}
	}
	if len(memberResp.ReplyStrategies) != 3 || !memberResp.ReplyStrategies[0].Default || memberResp.ReplyStrategies[1].CallsModel {
		t.Fatalf("unexpected reply_strategies: %+v", memberResp.ReplyStrategies)
	}
	if len(memberResp.Tasks) != 1 || memberResp.Tasks[0].Title != "成员任务" {
		t.Fatalf("member should only see own tasks, got %+v", memberResp.Tasks)
	}
	if len(memberResp.Users) != 2 {
		t.Fatalf("expected 2 mentionable users, got %+v", memberResp.Users)
	}
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "dummy") {
		t.Fatalf("composer-options leaked sensitive fields: %s", w.Body)
	}

	// Superadmin with scope=all sees both tasks
	w = f.request("GET", "/api/v1/assistant/composer-options?scope=all", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("admin composer-options: %d %s", w.Code, w.Body)
	}
	var adminResp composerOptionsResponse
	accountJSON(t, w, &adminResp)
	if len(adminResp.Tasks) != 2 {
		t.Fatalf("admin scope=all expected 2 tasks, got %+v", adminResp.Tasks)
	}

	if got := inferModelBrand("claude-sonnet-4-5", ""); got != "anthropic" {
		t.Fatalf("claude brand = %q, want anthropic", got)
	}
	if got := inferModelBrand("Qwen3.5-235B", ""); got != "qwen" {
		t.Fatalf("qwen brand = %q, want qwen", got)
	}
}
