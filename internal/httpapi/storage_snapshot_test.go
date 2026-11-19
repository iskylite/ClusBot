package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

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
