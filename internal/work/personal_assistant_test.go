package work

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestPersonalAssistantIdentityIsStableAndIsolated(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "assistant.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	account, err := store.BootstrapAccount(ctx, "operator-a", "Operator A", "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	owner := account.PrincipalID
	var first, second PersonalAssistant
	var firstErr, secondErr error
	var workers sync.WaitGroup
	workers.Go(func() { first, firstErr = store.EnsurePersonalAssistant(ctx, owner) })
	workers.Go(func() { second, secondErr = store.EnsurePersonalAssistant(ctx, owner) })
	workers.Wait()
	if firstErr != nil || secondErr != nil || first.ID == "" || first != second {
		t.Fatalf("concurrent initialization gave %+v (%v), %+v (%v)", first, firstErr, second, secondErr)
	}
	const otherOwner = "user:operator-b"
	if _, err := store.db.ExecContext(ctx, `INSERT INTO principals(principal_id,kind,identity_digest,created_at) VALUES(?,'user','fixture-b',0)`, otherOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts(principal_id,username,display_name,status,created_at) VALUES(?,'operator-b','Operator B','active',0)`, otherOwner); err != nil {
		t.Fatal(err)
	}
	other, err := store.EnsurePersonalAssistant(ctx, otherOwner)
	if err != nil || other.ID == "" || first.ID == other.ID {
		t.Fatalf("cross-account identity: %+v (%v)", other, err)
	}
	if _, err := store.EnsurePersonalAssistant(ctx, "user:session"); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("legacy shared account accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET status='disabled' WHERE principal_id=?`, otherOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsurePersonalAssistant(ctx, otherOwner); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("disabled account accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.EnsurePersonalAssistant(ctx, owner)
	if err != nil || restored != first {
		t.Fatalf("identity after restart: %+v (%v), want %+v", restored, err, first)
	}
}
