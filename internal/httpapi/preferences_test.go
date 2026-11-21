package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestGeneralPreferencesAccountIsolationAndValidation(t *testing.T) {
	instance, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	var mu sync.Mutex
	stores := make(map[string]*work.Store)
	var srv Server
	srv = Server{
		Store: instance, ServerMode: true, Origin: "https://ops.example", Hosts: []string{"ops.example"},
		PersonalAPI: func(owner string) (http.Handler, error) {
			mu.Lock()
			defer mu.Unlock()
			if stores[owner] == nil {
				store, err := work.Open(filepath.Join(t.TempDir(), "personal.db"))
				if err != nil {
					return nil, err
				}
				stores[owner] = store
				t.Cleanup(func() { _ = store.Close() })
			}
			return srv.PersonalHandler(stores[owner]), nil
		},
	}
	f := &accountFixture{t: t, store: instance, handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	w := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username": "member", "display_name": "成员", "password": "member password 123",
	}, admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", w.Code, w.Body)
	}
	member := f.login("member", "member password 123")

	w = f.request("GET", "/api/v1/preferences", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/preferences", nil, admin)
	var p work.GeneralPreferences
	accountJSON(t, w, &p)
	if w.Code != http.StatusOK || p != work.DefaultGeneralPreferences() {
		t.Fatalf("read defaults: %d %s", w.Code, w.Body)
	}
	p.Timezone = "Asia/Shanghai"
	p.DesktopNotifications = true
	w = f.request("PUT", "/api/v1/preferences", p, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("save preferences: %d %s", w.Code, w.Body)
	}
	w = f.request("PUT", "/api/v1/preferences", p, admin)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "preferences_revision") {
		t.Fatalf("stale save: %d %s", w.Code, w.Body)
	}
	w = f.request("PUT", "/api/v1/preferences", map[string]any{"revision": 1}, admin)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial replacement: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/preferences", nil, member)
	if w.Code != http.StatusOK {
		t.Fatalf("member read: %d %s", w.Code, w.Body)
	}
	var memberPreferences work.GeneralPreferences
	if err := json.Unmarshal(w.Body.Bytes(), &memberPreferences); err != nil {
		t.Fatal(err)
	}
	if memberPreferences != work.DefaultGeneralPreferences() {
		t.Fatalf("account preferences leaked: %+v", memberPreferences)
	}
}
