package work

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestClusterInventoryAuthorizationAndPagination(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, asset := range []struct{ ref, kind string }{
		{"node-a", "node"}, {"node-b", "GPU 节点"}, {"node-private", "node"}, {"node-revoked", "node"}, {"gpu", "gpu"},
	} {
		exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,created_at,updated_at) VALUES(?,?,?,'在役',1,1)`, asset.ref, asset.kind, asset.ref)
	}
	for _, grant := range []struct {
		ref     string
		revoked int
	}{
		{"node-a", 0}, {"node-b", 0}, {"node-revoked", 1}, {"gpu", 0},
	} {
		exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,revoked_at,owner,created_at,updated_at) VALUES(?, 'user:test','read',?,0,?,'owner',0,0)`, "auth-"+grant.ref, grant.ref, grant.revoked)
	}
	first, err := store.ClusterInventory(context.Background(), "user:test", "", 1, now)
	if err != nil || len(first.Items) != 1 || first.Items[0].ObjectRef != "node-a" || first.NextCursor != "node-a" {
		t.Fatalf("first page: %+v err=%v", first, err)
	}
	second, err := store.ClusterInventory(context.Background(), "user:test", first.NextCursor, 1, now)
	if err != nil || len(second.Items) != 1 || second.Items[0].ObjectRef != "node-b" || second.NextCursor != "" {
		t.Fatalf("second page: %+v err=%v", second, err)
	}
	other, err := store.ClusterInventory(context.Background(), "user:other", "", 24, now)
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("other principal: %+v err=%v", other, err)
	}
	if _, err := store.ClusterInventory(context.Background(), "", "", 24, now); err == nil {
		t.Fatal("empty principal accepted")
	}
	exec(`UPDATE authorizations SET revoked_at=1 WHERE object_ref='node-b'`)
	revoked, err := store.ClusterInventory(context.Background(), "user:test", "", 24, now)
	if err != nil || len(revoked.Items) != 1 || revoked.Items[0].ObjectRef != "node-a" {
		t.Fatalf("revocation: %+v err=%v", revoked, err)
	}
}
