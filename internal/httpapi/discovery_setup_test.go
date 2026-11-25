package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestDiscoverySetupStatusHTTP(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := Server{Store: store, Hosts: []string{"localhost"}}
	businessFixturePrincipal(t, store)
	handler := productionFixtureHandler(t, server, "http://localhost", fstest.MapFS{})
	cookie := businessFixtureLogin(t, handler, "http://localhost")
	const path = "http://localhost/api/v1/domain/discovery/setup-status"
	read := func(suffix string, auth *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+suffix, nil)
		if auth != nil {
			req.AddCookie(auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status = %d, cache = %q: %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
		}
		return rec
	}
	read("", nil, http.StatusUnauthorized)
	read("?source=only", cookie, http.StatusBadRequest)
	read("?source=a&source=b&scope=x", cookie, http.StatusBadRequest)
	if got := read("", cookie, http.StatusOK).Body.String(); got != "{\"state\":\"source_unavailable\"}\n" {
		t.Fatalf("unexpected response: %s", got)
	}
	server.DiscoverySource = &blockingDiscoveryFixture{}
	handler = productionFixtureHandler(t, server, "http://localhost", fstest.MapFS{})
	if got := read("?source=fixture-discovery&scope=POD7", cookie, http.StatusOK).Body.String(); got != "{\"state\":\"no_active_plan\"}\n" {
		t.Fatalf("unexpected response: %s", got)
	}
}
