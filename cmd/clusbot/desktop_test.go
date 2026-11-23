package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"clusbot/internal/work"
)

func TestDesktopDataModePreservesExistingInstallations(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		kind string
		want string
	}{
		{"fresh", "fresh", "server"},
		{"legacy local", "local", "local"},
		{"existing accounts", "accounts", "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "clusbot.db")
			if tc.kind != "fresh" {
				db, err := work.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if tc.kind == "accounts" {
					if err := db.EnsureAccountMode(ctx, true); err != nil {
						t.Fatal(err)
					}
					if _, err := db.BootstrapAccount(ctx, "admin", "Admin", "existing-hash"); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("existing-token"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			existing, err := desktopExistingData(dir)
			if err != nil {
				t.Fatal(err)
			}
			db, err := work.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mode, err := desktopDataMode(ctx, db, existing, dir)
			if err != nil || mode != tc.want {
				t.Fatalf("mode = %q, err = %v; want %q", mode, err, tc.want)
			}
			if tc.kind == "accounts" {
				required, err := db.BootstrapRequired(ctx)
				if err != nil || required {
					t.Fatalf("existing account was reset: required = %v, err = %v", required, err)
				}
			}
			if tc.kind == "local" {
				token, err := os.ReadFile(filepath.Join(dir, "access-token"))
				if err != nil || string(token) != "existing-token" {
					t.Fatalf("legacy token changed: %q, %v", token, err)
				}
			}
		})
	}
}

func TestDesktopRejectsOrphanedAccountData(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "account-data"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := desktopExistingData(dir); err == nil {
		t.Fatal("missing account database was treated as a fresh installation")
	}
	db, err := work.Open(filepath.Join(dir, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := desktopDataMode(context.Background(), db, true, dir); err == nil {
		t.Fatal("unrelated local database was accepted with orphaned account data")
	}
}

func TestDesktopInterruptedFirstLaunchStillRequiresBootstrap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "clusbot.db")
	existing, err := desktopPrepareData(dir)
	if err != nil || existing {
		t.Fatalf("prepare fresh directory: existing = %v, err = %v", existing, err)
	}
	// Simulate interruption after the schema was created but before account
	// mode was saved. The next start must still initialize an administrator.
	db, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	existing, err = desktopPrepareData(dir)
	if err != nil || existing {
		t.Fatalf("resume initialization: existing = %v, err = %v", existing, err)
	}
	db, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mode, err := desktopDataMode(ctx, db, existing, dir)
	if err != nil || mode != "server" {
		t.Fatalf("resumed mode = %q, err = %v", mode, err)
	}
	if err = db.EnsureAccountMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = desktopFinishInitialization(dir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(dir, desktopInitializing)); !os.IsNotExist(err) {
		t.Fatalf("initialization marker remains: %v", err)
	}
	required, err := db.BootstrapRequired(ctx)
	if err != nil || !required {
		t.Fatalf("bootstrap required = %v, err = %v", required, err)
	}
	existing, err = desktopPrepareData(dir)
	if err != nil || !existing {
		t.Fatalf("prepare established account database: existing = %v, err = %v", existing, err)
	}
	mode, err = desktopDataMode(ctx, db, existing, dir)
	if err != nil || mode != "server" {
		t.Fatalf("established mode = %q, err = %v", mode, err)
	}
}

func TestDesktopResumesBeforeDatabaseLink(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, desktopInitializing)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	existing, err := desktopPrepareData(dir)
	if err != nil || existing {
		t.Fatalf("resume before link: existing = %v, err = %v", existing, err)
	}
	markerInfo, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	dbInfo, err := os.Stat(filepath.Join(dir, "clusbot.db"))
	if err != nil || !os.SameFile(markerInfo, dbInfo) {
		t.Fatalf("pending database was not linked: %v", err)
	}
}

func TestDesktopInterruptedInitializationProtectsExistingData(t *testing.T) {
	t.Run("database replaced", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := desktopPrepareData(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "clusbot.db")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		// A restored local database must not be claimed by the stale marker.
		db, err := work.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = desktopPrepareData(dir); err == nil {
			t.Fatal("accepted an unrelated database as a fresh installation")
		}
		if err = db.EnsureAccountMode(context.Background(), false); err != nil {
			t.Fatalf("restored local database was changed: %v", err)
		}
	})
	t.Run("local data appeared", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := desktopPrepareData(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "access-token")
		if err := os.WriteFile(path, []byte("existing-token"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := desktopPrepareData(dir); err == nil {
			t.Fatal("accepted local data during pending initialization")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "existing-token" {
			t.Fatalf("local data changed: %q, %v", content, err)
		}
	})
}
