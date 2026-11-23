package work

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAssistantInventoryExportRequiresExactCurrentGrants(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "authorization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Now()
	stamp := at.UnixMilli()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(id, action, ref, constraints string, from, expires, revoked int64) {
		t.Helper()
		exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,param_constraints,valid_from,expires_at,revoked_at,owner,created_at,updated_at)
			VALUES(?,'user:one',?,?,?,?,?,?,'owner',0,0)`, id, action, ref, constraints, from, expires, revoked)
	}
	grant("read-a", "read", "node-a", "{}", stamp-1, 0, 0)
	grant("read-b", "read", "node-b", "{}", stamp-1, 0, 0)
	grant("restricted-a", "export_model", "node-a", `{"destination":"somewhere"}`, stamp-1, 0, 0)
	grant("expired-b", "export_model", "node-b", "{}", stamp-10, stamp-1, 0)
	check := func(principal string, refs []string, want bool) {
		t.Helper()
		got, err := store.AuthorizeAssistantInventoryExport(t.Context(), principal, refs, at)
		if err != nil || got != want {
			t.Fatalf("authorize %s %v: got %v, err %v", principal, refs, got, err)
		}
	}
	check("user:one", []string{"node-a", "node-b"}, false)
	grant("export-a", "export_model", "node-a", "{}", stamp-1, 0, 0)
	grant("export-b", "export_model", "node-b", "{}", stamp-1, 0, 0)
	check("user:one", []string{"node-a", "node-b", "node-a"}, true)
	check("user:other", []string{"node-a"}, false)
	check("user:one", []string{"node-a", "missing"}, false)
	exec(`UPDATE authorizations SET revoked_at=? WHERE auth_id='export-b'`, stamp)
	check("user:one", []string{"node-a", "node-b"}, false)
	exec(`UPDATE authorizations SET revoked_at=? WHERE auth_id='read-a'`, stamp)
	check("user:one", []string{"node-a"}, false)
	var decisions int
	if err := store.db.QueryRow(`SELECT count(*) FROM authorization_decisions WHERE action='export_model'`).Scan(&decisions); err != nil || decisions == 0 {
		t.Fatalf("export decisions were not recorded: %d %v", decisions, err)
	}
}
