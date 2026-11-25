package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestStorageSnapshotWorksWithoutModelAndDoesNotDisclosePaths(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := setupA1(t, filepath.Join(dir, "private-sample.sqlite"))
	defer a.close()
	if _, err := a.store.Submit(t.Context(), "snapshot-fixture", "private chat content", "", "", false); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/settings/data/storage"
	r := a.request(http.MethodGet, path, "")
	r.Header.Del("Cookie")
	res, err := a.ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous diagnostics status %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, path+"?file=/not-part-of-diagnostics", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("storage hints need no model: %d %s", res.StatusCode, raw)
	}
	for _, disallowed := range []string{"private-sample.sqlite", "/not-part-of-diagnostics", dir, "private chat content"} {
		if strings.Contains(string(raw), disallowed) {
			t.Fatalf("sensitive data in projection: %s", raw)
		}
	}
	var result work.StorageSnapshot
	if err := json.Unmarshal(raw, &result); err != nil || result.AutomaticCleanup != "not_enabled" || result.Database.State != "available" {
		t.Fatalf("bad storage DTO: %+v %v", result, err)
	}
}

func TestStorageSnapshotUsesEachAccountStore(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(dir, "instance.db"))
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
			if stores[owner] == nil {
				path := filepath.Join(dir, fmt.Sprintf("personal-%d.db", len(stores)))
				store, err := work.Open(path)
				if err != nil {
					return nil, err
				}
				stores[owner], paths[owner] = store, path
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
	const endpoint = "/api/v1/settings/data/storage"
	w = f.request("GET", endpoint, nil, admin)
	if w.Code != http.StatusOK || len(paths) != 1 {
		t.Fatalf("admin snapshot: %d %s", w.Code, w.Body)
	}
	var adminPath string
	for _, path := range paths {
		adminPath = path
	}
	db, err := sql.Open("sqlite", adminPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE storage_fixture (content BLOB); INSERT INTO storage_fixture VALUES (zeroblob(1048576))`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	w = f.request("GET", endpoint, nil, admin)
	var adminSnapshot work.StorageSnapshot
	accountJSON(t, w, &adminSnapshot)
	w = f.request("GET", endpoint, nil, member)
	var memberSnapshot work.StorageSnapshot
	accountJSON(t, w, &memberSnapshot)
	if w.Code != http.StatusOK || len(stores) != 2 || adminSnapshot.Database.AllocatedBytes == nil || memberSnapshot.Database.AllocatedBytes == nil ||
		*adminSnapshot.Database.AllocatedBytes <= *memberSnapshot.Database.AllocatedBytes {
		t.Fatalf("account snapshots not isolated: admin=%+v member=%+v response=%d %s", adminSnapshot, memberSnapshot, w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), dir) {
		t.Fatalf("snapshot disclosed a filesystem path: %s", w.Body)
	}
}
