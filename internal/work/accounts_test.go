package work

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAccountBootstrapPersistsAcrossRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "accounts.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.BootstrapAccount(ctx, "admin", "admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	required, err := store.BootstrapRequired(ctx)
	if err != nil || required {
		t.Fatalf("initialization reopened after restart: required=%v err=%v", required, err)
	}
	// The initialized marker is durable even if accounts are removed out of band.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM accounts WHERE principal_id=?`, account.PrincipalID); err != nil {
		t.Fatal(err)
	}
	required, err = store.BootstrapRequired(ctx)
	if err != nil || required {
		t.Fatalf("initialization reopened after account removal: required=%v err=%v", required, err)
	}
	if _, err := store.BootstrapAccount(ctx, "other", "other", "hash"); err != ErrBootstrapComplete {
		t.Fatalf("bootstrap after account removal: %v", err)
	}
	if err := store.EnsureAccountMode(ctx, false); err != ErrServerDatabase {
		t.Fatalf("initialized server database reopened in local mode: %v", err)
	}
}

func TestResetLegacyPendingAccount(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "legacy-pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	admin, err := store.BootstrapAccount(ctx, "admin", "Admin", "admin-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccountSession(ctx, admin.PrincipalID, "admin-hash", "admin-session", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO principals(principal_id,kind,identity_digest,enabled,created_at) VALUES('user:pending','user','pending-digest',0,0);
		INSERT INTO accounts(principal_id,username,display_name,status,created_at) VALUES('user:pending','pending','Pending','pending',0)`); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetAccountPassword(ctx, "admin-session", "user:pending", "temporary-hash"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccountSession(ctx, "user:pending", "temporary-hash", "pending-session", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a, err := store.AccountForSession(ctx, "pending-session")
	if err != nil || a.Status != "active" || !a.MustChangePassword || a.LastLoginAt == 0 {
		t.Fatalf("legacy pending account after reset: %+v, %v", a, err)
	}
	if err := store.DeleteAccountSession(ctx, "pending-session"); err != nil {
		t.Fatal(err)
	}
	members, err := store.ListAccounts(ctx, "admin-session")
	if err != nil || len(members) != 2 || members[1].PrincipalID != a.PrincipalID || members[1].LastLoginAt != a.LastLoginAt {
		t.Fatalf("last login missing after logout: %+v, %v", members, err)
	}
}

func TestAccountPasswordStateMigrationPreservesLegacyData(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.BootstrapAccount(ctx, "admin", "Admin", "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO account_recovery_codes(principal_id,token_digest) VALUES(?,?)`, account.PrincipalID, "legacy-recovery"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccountSession(ctx, account.PrincipalID, "fixture-hash", "legacy-session", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE account_bootstrap; DROP TRIGGER account_last_login_after_insert;
		ALTER TABLE accounts DROP COLUMN last_login_at; ALTER TABLE accounts DROP COLUMN must_change_password; PRAGMA user_version=126`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var hash, recovery string
	var force bool
	if err := store.db.QueryRowContext(ctx, `SELECT password_hash,must_change_password FROM accounts WHERE principal_id=?`, account.PrincipalID).Scan(&hash, &force); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT token_digest FROM account_recovery_codes WHERE principal_id=?`, account.PrincipalID).Scan(&recovery); err != nil {
		t.Fatal(err)
	}
	if hash != "fixture-hash" || force || recovery != "legacy-recovery" {
		t.Fatalf("migration changed legacy data: hash=%q force=%v recovery=%q", hash, force, recovery)
	}
	migrated, err := store.AccountForSession(ctx, "legacy-session")
	if err != nil || migrated.CreatedAt != account.CreatedAt || migrated.LastLoginAt < account.CreatedAt {
		t.Fatalf("account dates missing after migration: %+v, %v", migrated, err)
	}
	required, err := store.BootstrapRequired(ctx)
	if err != nil || required {
		t.Fatalf("legacy account reopened initialization: required=%v err=%v", required, err)
	}
}
