package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestClusterInventoryHTTPAccountAuthentication(t *testing.T) {
	c, store := setupA2HTTP(t, filepath.Join(t.TempDir(), "local.db"))
	defer c.close()
	defer store.Close()
	if code, _ := c.do(c.request("GET", "/api/v1/domain/cluster-inventory", "", false)); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %d", code)
	}
	code, result := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?limit=1", "", true))
	items, ok := result["items"].([]any)
	if code != http.StatusOK || !ok || len(items) != 1 {
		t.Fatalf("authenticated request: %d %v", code, result)
	}
	if code, _ := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?limit=101", "", true)); code != http.StatusBadRequest {
		t.Fatalf("invalid page size: %d", code)
	}
}

func TestClusterInventoryPODFilterRespectsAuthorization(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pod-filter.db")
	c, store := setupA2HTTP(t, dbPath)
	defer c.close()
	defer store.Close()
	entries := []work.DomainAssetEntry{
		{ObjectRef: "pod-visible", Kind: "pod", Identity: "Visible POD", Read: true},
		{ObjectRef: "pod-hidden", Kind: "pod", Identity: "Hidden POD"},
		{ObjectRef: "visible-a", Kind: "node", Identity: "Visible A", PodRef: "pod-visible", Read: true},
		{ObjectRef: "visible-b", Kind: "node", Identity: "Visible B", PodRef: "pod-visible", Read: true},
		{ObjectRef: "private-node", Kind: "node", Identity: "Private Node", PodRef: "pod-visible"},
		{ObjectRef: "hidden-parent-node", Kind: "node", Identity: "Hidden Parent Node", PodRef: "pod-hidden", Read: true},
	}
	if err := store.ImportDomainAssetsForAccount(context.Background(), businessFixturePrincipal(t, store), entries); err != nil {
		t.Fatal(err)
	}

	code, first := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?pod_ref=pod-visible&limit=1", "", true))
	items, _ := first["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["object_ref"] != "visible-a" || items[0].(map[string]any)["pod_ref"] != "pod-visible" || first["next_cursor"] != "visible-a" {
		t.Fatalf("first authorized POD page: %d %v", code, first)
	}
	code, second := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?pod_ref=pod-visible&limit=1&after=visible-a", "", true))
	items, _ = second["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["object_ref"] != "visible-b" || second["next_cursor"] != nil {
		t.Fatalf("second authorized POD page: %d %v", code, second)
	}
	for _, page := range []map[string]any{first, second} {
		summary, _ := page["summary"].(map[string]any)
		if summary["total_nodes"] != float64(4) || summary["topology_unknown_nodes"] != float64(2) {
			t.Fatalf("filter changed global summary: %v", summary)
		}
	}
	for _, ref := range []string{"pod-hidden", "missing-pod"} {
		code, result := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?pod_ref="+ref, "", true))
		if code != http.StatusNotFound || strings.Contains(fmt.Sprint(result), "Hidden POD") || strings.Contains(fmt.Sprint(result), "hidden-parent-node") {
			t.Fatalf("inaccessible POD must not expose identity or members: %d %v", code, result)
		}
	}
	code, receipt := c.do(c.request("POST", "/api/v1/assistant/tools", `{"tool":"get_cluster_inventory","query":{"pod_ref":"pod-visible","limit":"1"}}`, true))
	result, _ := receipt["result"].(map[string]any)
	items, _ = result["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["object_ref"] != "visible-a" {
		t.Fatalf("assistant POD filter: %d %v", code, receipt)
	}
}

func TestAssistantClusterInventoryUsesAuthorizedRead(t *testing.T) {
	c, store := setupA2HTTP(t, filepath.Join(t.TempDir(), "assistant-inventory.db"))
	defer c.close()
	defer store.Close()
	if code, _ := c.do(c.request("POST", "/api/v1/assistant/tools", `{"tool":"get_cluster_inventory"}`, false)); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated assistant request: %d", code)
	}
	code, direct := c.do(c.request("GET", "/api/v1/domain/cluster-inventory?limit=1", "", true))
	if code != http.StatusOK {
		t.Fatalf("direct inventory request: %d %v", code, direct)
	}
	code, receipt := c.do(c.request("POST", "/api/v1/assistant/tools", `{"tool":"get_cluster_inventory","query":{"limit":"1"}}`, true))
	result, ok := receipt["result"].(map[string]any)
	if code != http.StatusOK || receipt["state"] != "completed" || !ok {
		t.Fatalf("assistant inventory request: %d %v", code, receipt)
	}
	directItems, directOK := direct["items"].([]any)
	items, itemsOK := result["items"].([]any)
	if !directOK || !itemsOK || len(directItems) != 1 || len(items) != 1 {
		t.Fatalf("unexpected inventory items: direct=%v assistant=%v", direct, result)
	}
	directNode, _ := directItems[0].(map[string]any)
	node, _ := items[0].(map[string]any)
	if directNode["object_ref"] != node["object_ref"] || directNode["state"] != node["state"] {
		t.Fatalf("assistant result differs from authorized read: direct=%v assistant=%v", directNode, node)
	}
}

func TestAssistantClusterInventoryAccountIsolation(t *testing.T) {
	instance, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	stores := make(map[string]*work.Store)
	var srv Server
	srv = Server{Store: instance, Origin: "https://ops.example", Hosts: []string{"ops.example"},
		PersonalAPI: func(owner string) (http.Handler, error) {
			if stores[owner] == nil {
				return nil, fmt.Errorf("missing personal store")
			}
			return srv.PersonalHandler(stores[owner]), nil
		},
	}
	f := &accountFixture{t: t, store: instance, handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
	admin := f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")
	created := f.request("POST", "/api/v1/admin/users", map[string]any{"username": "member", "password": "member password 1234"}, adminCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", created.Code, created.Body)
	}
	var response struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, created, &response)
	member := response.User.PrincipalID
	memberCookie := f.login("member", "member password 1234")
	for owner, ref := range map[string]string{admin: "admin-node", member: "member-node"} {
		path := filepath.Join(t.TempDir(), "business.db")
		store, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.SeedAssetForTest(ref, work.AssetStateInService); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `UPDATE authorizations SET subject=? WHERE subject='user:session'`, owner); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()
		stores[owner] = store
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		want   string
	}{{adminCookie, "admin-node"}, {memberCookie, "member-node"}} {
		w := f.request("POST", "/api/v1/assistant/tools", map[string]any{"tool": "get_cluster_inventory", "query": map[string]string{"scope": "all", "principal": admin}}, tc.cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("assistant inventory: %d %s", w.Code, w.Body)
		}
		var receipt struct {
			State  string `json:"state"`
			Result struct {
				Items []struct {
					ObjectRef string `json:"object_ref"`
				} `json:"items"`
				Summary struct {
					TotalNodes int `json:"total_nodes"`
					Exceptions []struct {
						ObjectRef string `json:"object_ref"`
					} `json:"exceptions"`
				} `json:"summary"`
			} `json:"result"`
		}
		accountJSON(t, w, &receipt)
		if receipt.State != "completed" || len(receipt.Result.Items) != 1 || receipt.Result.Items[0].ObjectRef != tc.want ||
			receipt.Result.Summary.TotalNodes != 1 || len(receipt.Result.Summary.Exceptions) != 1 || receipt.Result.Summary.Exceptions[0].ObjectRef != tc.want {
			t.Fatalf("inventory escaped account authorization: %s", w.Body)
		}
		direct := f.request("GET", "/api/v1/domain/cluster-inventory?scope=all&principal="+admin, nil, tc.cookie)
		var page struct {
			Summary struct {
				TotalNodes int `json:"total_nodes"`
				Exceptions []struct {
					ObjectRef string `json:"object_ref"`
				} `json:"exceptions"`
			} `json:"summary"`
		}
		accountJSON(t, direct, &page)
		if direct.Code != http.StatusOK || page.Summary.TotalNodes != 1 || len(page.Summary.Exceptions) != 1 || page.Summary.Exceptions[0].ObjectRef != tc.want {
			t.Fatalf("direct summary escaped account authorization: %d %s", direct.Code, direct.Body)
		}
	}
}
