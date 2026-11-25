package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

const assetImportFixtureName = "asset-operator"

func openAssetImportInstanceFixture(t *testing.T, root string) (*work.Store, work.Account) {
	t.Helper()
	instance, err := openAccountInstance(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	account, err := instance.BootstrapAccount(t.Context(), assetImportFixtureName, "Asset Operator", "unused-test-hash")
	if err != nil {
		t.Fatal(err)
	}
	return instance, account
}

func accountAssetImportFixtureDB(t *testing.T, root, owner string) string {
	t.Helper()
	accountRoot := filepath.Join(root, "account-data")
	dir := filepath.Join(accountRoot, strings.TrimPrefix(owner, "user:"))
	for _, name := range []string{accountRoot, dir} {
		if err := accountDirectory(name); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "clusbot.db")
}

func prepareAssetImportFixtureStore(t *testing.T, root, owner string) string {
	t.Helper()
	path := accountAssetImportFixtureDB(t, root, owner)
	business, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer business.Close()
	if err = business.BindAccountBusinessStore(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportDomainAssetsPODYAML(t *testing.T) {
	root := t.TempDir()
	_, account := openAssetImportInstanceFixture(t, root)
	private := prepareAssetImportFixtureStore(t, root, account.PrincipalID)
	manifest := filepath.Join(root, "assets.yaml")
	contents := []byte(`assets:
  - object_ref: node-a
    kind: node
    identity: serial-a
    pod_ref: group-a
    allow_read: true
  - object_ref: group-a
    kind: pod
    identity: Group A
    allow_read: true
`)
	if err := os.WriteFile(manifest, contents, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runImportDomainAssets([]string{"--account", account.Username, "--file", manifest, "--data-dir", root}); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	store, err := work.Open(private)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err := store.ClusterInventory(context.Background(), account.PrincipalID, "", 24, time.Now())
	if err != nil || page.Summary.TotalNodes != 1 || len(page.Summary.Pods) != 1 || page.Summary.Pods[0].ObjectRef != "group-a" || page.Summary.Pods[0].TotalNodes != 1 {
		t.Fatalf("YAML import not reflected in inventory: %+v err=%v", page.Summary, err)
	}
	rootDB, err := sql.Open("sqlite", filepath.Join(root, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rootDB.Close()
	var assets, grants int
	if err := rootDB.QueryRow(`SELECT (SELECT count(*) FROM ops_assets),(SELECT count(*) FROM authorizations)`).Scan(&assets, &grants); err != nil || assets != 0 || grants != 0 {
		t.Fatalf("import touched shared instance DB: assets=%d grants=%d (%v)", assets, grants, err)
	}
	privateDB, err := sql.Open("sqlite", private)
	if err != nil {
		t.Fatal(err)
	}
	defer privateDB.Close()
	if err = privateDB.QueryRow(`SELECT count(*) FROM authorizations WHERE subject=? AND owner=?`, account.PrincipalID, account.PrincipalID).Scan(&grants); err != nil || grants != 2 {
		t.Fatalf("read grants did not bind to the selected account idempotently: %d, %v", grants, err)
	}
}

func TestImportDomainAssetsRejectsUnselectedOrUnownedStores(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"missing account flag", "--account"},
		{"ambiguous accounts", "只能指定一次"},
		{"unknown account", "不存在或未启用"},
		{"disabled account", "不存在或未启用"},
		{"missing private DB", "未初始化"},
		{"unbound private DB", "归属无法确认"},
		{"other owner", "归属无法确认"},
		{"symlinked DB", "未初始化或无效"},
		{"instance already running", "已由另一个"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			instance, admin := openAssetImportInstanceFixture(t, root)
			manifest := filepath.Join(root, "assets.yaml")
			if err := os.WriteFile(manifest, []byte(`assets:
  - object_ref: should-not-import
    kind: node
    identity: forbidden-fixture
    allow_read: true
`), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--file", manifest, "--data-dir", root}
			private := accountAssetImportFixtureDB(t, root, admin.PrincipalID)
			switch tc.name {
			case "missing account flag":
			case "ambiguous accounts":
				args = append(args, "--account", admin.Username, "--account", "different-user")
			case "unknown account":
				args = append(args, "--account", "missing-user")
			case "disabled account":
				ctx := t.Context()
				if err := instance.CreateAccountSession(ctx, admin.PrincipalID, "unused-test-hash", "asset-import-test-digest", time.Now().Add(time.Hour).UnixMilli()); err != nil {
					t.Fatal(err)
				}
				member, err := instance.CreateAccount(ctx, "asset-import-test-digest", "disabled-member", "Disabled", "another-test-hash", false)
				if err != nil {
					t.Fatal(err)
				}
				disabled := "disabled"
				if _, err = instance.UpdateAccount(ctx, "asset-import-test-digest", member.PrincipalID, work.AccountPatch{Status: &disabled}); err != nil {
					t.Fatal(err)
				}
				private = prepareAssetImportFixtureStore(t, root, member.PrincipalID)
				args = append(args, "--account", member.Username)
			default:
				args = append(args, "--account", admin.Username)
			}
			switch tc.name {
			case "unbound private DB", "other owner":
				db, err := work.Open(private)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "other owner" {
					if err := db.BindAccountBusinessStore(t.Context(), "user:"+strings.Repeat("e", 32)); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			case "symlinked DB":
				if err := os.Symlink(filepath.Join(root, "clusbot.db"), private); err != nil {
					t.Fatal(err)
				}
			case "instance already running":
				lock, err := work.LockDataDir(root)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if err := runImportDomainAssets(args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected import decision: %v, want %q", err, tc.want)
			}
			fi, err := os.Lstat(private)
			if errors.Is(err, os.ErrNotExist) || err == nil && !fi.Mode().IsRegular() {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", private)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow("SELECT count(*) FROM ops_assets").Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed import wrote to the database: count=%d (%v)", count, err)
			}
		})
	}
}
