package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestAccountInstanceCreatesFirstAdminAndPreservesAccount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, first := range []bool{true, false} {
		db, err := openAccountInstance(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		required, err := db.BootstrapRequired(ctx)
		if err != nil || required != first {
			t.Fatalf("bootstrap required = %v, err = %v; want %v", required, err, first)
		}
		if first {
			if _, err = db.BootstrapAccount(ctx, "admin", "Admin", "existing-hash"); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertNoStartupPath(t, dir, "access-token")
	assertNoStartupPath(t, dir, accountInitializing)
}

func TestAccountStartupRejectsLegacyBeforeDatabaseUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, artifact := range []string{"without old login file", "with old login file", "with account marker"} {
		t.Run(artifact, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "clusbot.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			schema := `PRAGMA user_version=1; CREATE TABLE conversations (id TEXT); INSERT INTO conversations VALUES ('original');`
			if artifact == "with account marker" {
				schema += `CREATE TABLE account_instance (singleton INT, mode TEXT); INSERT INTO account_instance VALUES (1,'server');`
			}
			if _, err := db.ExecContext(ctx, schema); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if artifact == "with old login file" {
				// A symlink is just an old marker; startup must never read or
				// create its destination as part of authentication.
				if err := os.Symlink(filepath.Join(dir, "do-not-read"), filepath.Join(dir, "access-token")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, option := range [][]string{
				nil, {"--mode", "loopback"}, {"--mode", "server", "--origin", "https://ops.example"},
			} {
				previous := os.Args
				os.Args = append([]string{"clusbot", "serve", "--data-dir", dir, "--listen", "127.0.0.1:0"}, option...)
				err := run()
				os.Args = previous
				if !errors.Is(err, errUnownedData) || !strings.Contains(err.Error(), "backup") {
					t.Fatalf("legacy startup %v: %v", option, err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("legacy DB changed: %v", err)
			}
			for _, name := range []string{"account-data", "connection-key", accountInitializing, "do-not-read"} {
				assertNoStartupPath(t, dir, name)
			}
		})
	}
}

func assertNoStartupPath(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected %s: %v", name, err)
	}
}

func TestAccountStartupRejectsOrphanedData(t *testing.T) {
	for _, name := range []string{"account-data", "connection-key", "access-token", "uploads", "workspace", "clusbot.db-wal"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := openAccountInstance(context.Background(), dir); err == nil {
				t.Fatal("orphaned data treated as new account instance")
			}
			assertNoStartupPath(t, dir, "clusbot.db")
			assertNoStartupPath(t, dir, accountInitializing)
		})
	}
}

func TestAccountStartupResumesOnlyItsOwnInitialization(t *testing.T) {
	ctx := context.Background()
	t.Run("before database link", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, accountInitializing), nil, 0600); err != nil {
			t.Fatal(err)
		}
		db, err := openAccountInstance(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		assertNoStartupPath(t, dir, accountInitializing)
	})
	t.Run("after schema before account marker", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "clusbot.db")
		if pending, err := prepareAccountData(ctx, dir, path); err != nil || !pending {
			t.Fatalf("initialization pending = %v: %v", pending, err)
		}
		db, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = openAccountInstance(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		required, err := db.BootstrapRequired(ctx)
		if err != nil || !required {
			t.Fatalf("bootstrap required = %v, err = %v", required, err)
		}
		assertNoStartupPath(t, dir, accountInitializing)
	})
	t.Run("database replaced", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "clusbot.db")
		if _, err := prepareAccountData(ctx, dir, path); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		db, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = openAccountInstance(ctx, dir); err == nil {
			t.Fatal("an unrelated database was reinitialized")
		}
		if err = db.EnsureAccountMode(ctx, false); err != nil {
			t.Fatalf("restored database was changed: %v", err)
		}
	})
}

func TestCredentialKeyIsNotALoginSecret(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "access-token")
	// A broken symlink (neither a usable secret nor a readable file).
	if err := os.Symlink(filepath.Join(dir, "not-used"), legacyPath); err != nil {
		t.Fatal(err)
	}
	key, err := connectionCredentialKey(dir)
	if err != nil || len(key) != 64 {
		t.Fatalf("credential key not created: %v", err)
	}
	next, err := connectionCredentialKey(dir)
	if err != nil || next != key {
		t.Fatalf("credential key not stable: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "connection-key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential key permission: %v %v", info, err)
	}
	assertNoStartupPath(t, dir, "not-used")
}

func TestRemovedStartupModesAndFlagsDoNotCreateData(t *testing.T) {
	for _, option := range [][]string{
		{"--mode", "local"}, {"--mode", "desktop"}, {"--demo"}, {"--workspace-root", "unused"},
		{"--listen", "0.0.0.0:8080"}, {"--mode", "server", "--origin", "http://ops.example"},
	} {
		t.Run(strings.Join(option, " "), func(t *testing.T) {
			dir := t.TempDir()
			previous := os.Args
			os.Args = append([]string{"clusbot", "serve", "--data-dir", filepath.Join(dir, "data")}, option...)
			defer func() { os.Args = previous }()
			if err := run(); err == nil {
				t.Fatalf("unsafe or removed input accepted: %v", option)
			}
			assertNoStartupPath(t, dir, "data")
		})
	}
}

func TestPublicHTTPSOrigin(t *testing.T) {
	for _, opt := range []struct{ origin, want string }{
		{"https://Ops.Example:443/", "https://ops.example"},
		{"https://Ops.Example:00443/", "https://ops.example"},
		{"https://ops.example:8443", "https://ops.example:8443"},
		{"https://[::1]:8443", "https://[::1]:8443"},
		{"http://ops.example", ""},
		{"https://user@ops.example", ""},
		{"https://ops.example/a", ""},
		{"https://ops.example/?", ""},
		{"https://ops.example/#", ""},
		{"https://ops.example:", ""},
		{"https://ops.example:99999", ""},
		{"https://2001:db8::1", ""},
	} {
		got, err := publicHTTPSOrigin(opt.origin)
		if got != opt.want || (err == nil) != (opt.want != "") {
			t.Errorf("publicHTTPSOrigin(%q) = %q, %v; want %q", opt.origin, got, err, opt.want)
		}
	}
}
