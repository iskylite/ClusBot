package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestSkillProposalPagesSupportDirectNavigation(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := Server{Store: store, Hosts: []string{"127.0.0.1"}}
	handler := server.Handler(fstest.MapFS{"index.html": {Data: []byte("personal agent")}})
	for _, path := range []string{"/memory/candidates", "/memory/skill-proposals"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		if w.Code != 200 || w.Body.String() != "personal agent" {
			t.Fatalf("direct navigation to %s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestPersonalAgentHasNoDomainRoutes(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "work.db")
	store, err := work.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := Server{Store: store, Hosts: []string{"127.0.0.1"}}
	businessFixturePrincipal(t, store)
	handler := productionFixtureHandler(t, server, "http://127.0.0.1", fstest.MapFS{"index.html": {Data: []byte("personal agent")}})
	cookie := businessFixtureLogin(t, handler, "http://127.0.0.1")
	identifier := strings.Repeat("a", 32)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/monitors"},
		{"POST", "/api/v1/monitors"},
		{"PATCH", "/api/v1/monitors/" + identifier},
		{"POST", "/api/v1/monitors/" + identifier + "/check"},
		{"POST", "/api/v1/monitors/" + identifier + "/events"},
		{"POST", "/api/v1/tasks/" + identifier + "/scale-changes"},
		{"POST", "/api/v1/tasks/" + identifier + "/scale-changes/" + identifier + "/decision"},
		{"POST", "/api/v1/tasks/" + identifier + "/scale-changes/" + identifier + "/reconcile"},
		{"POST", "/api/v1/tasks/" + identifier + "/jobs"},
		{"POST", "/api/v1/tasks/" + identifier + "/jobs/" + identifier + "/cancel"},
		{"POST", "/api/v1/tasks/" + identifier + "/jobs/" + identifier + "/cancel/retry"},
		{"POST", "/api/v1/tasks/" + identifier + "/jobs/" + identifier + "/resume"},
		{"GET", "/monitors"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, strings.NewReader(`{}`))
			r.Header.Set("Origin", "http://127.0.0.1")
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatalf("removed route returned %d: %s", w.Code, w.Body.String())
			}
		})
	}
	for _, path := range []string{"/api/v1/conversations", "/api/v1/connections", "/api/v1/overview"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("personal endpoint %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
