package httpapi

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestClusterInventoryHTTPLocalAuthentication(t *testing.T) {
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
