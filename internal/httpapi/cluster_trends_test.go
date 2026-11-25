package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestClusterTrendsHTTPAccountIsolation(t *testing.T) {
	instance, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	stores := make(map[string]*work.Store)
	paths := make(map[string]string)
	var srv Server
	srv = Server{
		Store: instance, Origin: "https://ops.example", Hosts: []string{"ops.example"},
		PersonalAPI: func(owner string) (http.Handler, error) {
			store := stores[owner]
			if store == nil {
				return nil, fmt.Errorf("missing personal store")
			}
			return srv.PersonalHandler(store), nil
		},
	}
	f := &accountFixture{t: t, store: instance, handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
	admin := f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")
	created := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username": "member", "password": "member password 1234",
	}, adminCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", created.Code, created.Body)
	}
	var response struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, created, &response)
	member := response.User.PrincipalID
	memberCookie := f.login("member", "member password 1234")

	setupStore := func(owner string, count int) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "business.db")
		store, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		for i := range count {
			if err := store.SeedAssetForTest(fmt.Sprintf("node-%d", i), work.AssetStateInService); err != nil {
				t.Fatal(err)
			}
		}
		// The reusable seed helper grants the local-mode principal. Bind those
		// fixtures to the authenticated account for the server-mode test.
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `UPDATE authorizations SET subject=? WHERE subject='user:session'`, owner); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()
		stores[owner], paths[owner] = store, path
	}
	setupStore(admin, 1)
	setupStore(member, 2)

	if w := f.request("GET", "/api/v1/domain/cluster-trends", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d %s", w.Code, w.Body)
	}
	check := func(cookie *http.Cookie, path string, total, size int) {
		t.Helper()
		w := f.request("GET", path, nil, cookie)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("request %s: %d %s", path, w.Code, w.Body)
		}
		var got work.ClusterTrends
		accountJSON(t, w, &got)
		if len(got.Buckets) != size || got.Buckets[size-1].TotalNodes != total || got.Buckets[size-1].InServiceNodes != total {
			t.Fatalf("request %s: %+v", path, got)
		}
	}
	check(adminCookie, "/api/v1/domain/cluster-trends", 1, 7)
	check(memberCookie, "/api/v1/domain/cluster-trends?range=30d&scope=all", 2, 30)
	check(memberCookie, "/api/v1/domain/cluster-trends?range=24h&principal="+admin, 2, 24)
	w := f.request("GET", "/api/v1/domain/cluster-trends?range=1y", nil, memberCookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid range: %d %s", w.Code, w.Body)
	}
	// A revoked grant takes effect on the next request, even in the same
	// account and when asking about earlier dates.
	db, err := sql.Open("sqlite", paths[member])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE authorizations SET revoked_at=1 WHERE object_ref='node-0'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	check(memberCookie, "/api/v1/domain/cluster-trends?range=7d", 1, 7)
}

func TestClusterTrendsHTTPLocalAuthentication(t *testing.T) {
	c, store := setupA2HTTP(t, filepath.Join(t.TempDir(), "local.db"))
	defer c.close()
	defer store.Close()
	code, _ := c.do(c.request("GET", "/api/v1/domain/cluster-trends", "", false))
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated local request: %d", code)
	}
	code, result := c.do(c.request("GET", "/api/v1/domain/cluster-trends", "", true))
	if code != http.StatusOK || result["range"] != "7d" {
		t.Fatalf("authenticated local request: %d %v", code, result)
	}
}
