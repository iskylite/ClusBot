package work

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
)

type serverBackupFixture struct {
	id, task, publication, queued, upload string
}

func TestServerBackupRestoreAccountData(t *testing.T) {
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "account-data"), 0700); err != nil {
		t.Fatal(err)
	}
	instance, err := Open(filepath.Join(data, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if err = instance.EnsureAccountMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	admin, err := instance.BootstrapAccount(ctx, "admin", "Admin", "admin-hash")
	if err != nil {
		t.Fatal(err)
	}
	const session = "sensitive-session-digest-for-backup-test"
	if err = instance.CreateAccountSession(ctx, admin.PrincipalID, "admin-hash", session, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	member, err := instance.CreateAccount(ctx, session, "member", "Member", "member-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := instance.SaveConnection(ctx, "", Connection{Name: "catalog", Enabled: true,
		Config: mcpclient.Config{Transport: "http", Endpoint: "http://127.0.0.1:4444/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, "connection-key"), []byte("private-connection-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, "access-token"), []byte("private-access-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var fixtures []serverBackupFixture
	for _, principal := range []string{admin.PrincipalID, member.PrincipalID} {
		id := strings.TrimPrefix(principal, "user:")
		dir := filepath.Join(data, "account-data", id)
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		store, err := OpenAccountBusinessStore(filepath.Join(dir, backupDB), instance)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.EnsureAccountMode(ctx, true); err != nil {
			t.Fatal(err)
		}
		if err = store.BindAccountBusinessStore(ctx, principal); err != nil {
			t.Fatal(err)
		}
		if err = store.OpenWorkspace(filepath.Join(dir, "workspace")); err != nil {
			t.Fatal(err)
		}
		if err = store.OpenUploads(filepath.Join(dir, "uploads")); err != nil {
			t.Fatal(err)
		}
		if _, err = store.SaveMemory(ctx, "", Memory{Kind: "fact", Title: "owner", Content: id}, 0); err != nil {
			t.Fatal(err)
		}
		upload, err := store.UploadMaterial(ctx, "private.txt", []byte("upload:"+id))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "workspace", "materials", "private.md"), []byte("material:"+id), 0600); err != nil {
			t.Fatal(err)
		}
		filename := "report.md"
		run, err := store.SubmitWithDelivery(ctx, "delivered", "整理", "", "", true, Delivery{Filename: &filename})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.claim(ctx); err != nil {
			t.Fatal(err)
		}
		if err = store.delta(ctx, run.ID, "output:"+id); err != nil {
			t.Fatal(err)
		}
		if err = store.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		detail, err := store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		publication := requestWrite(t, store, detail, filename, "write")
		if result, err := store.DecidePublication(ctx, publication.ID, "approve", publication.taskRevision); err != nil || result.Status != "succeeded" {
			t.Fatalf("approve output: %+v, %v", result, err)
		}
		queued, err := store.Submit(ctx, "queued", "稍后处理", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, serverBackupFixture{id, run.TaskID, publication.ID, queued.TaskID, upload.Upload.ID})
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = instance.Close(); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(base, "backup")
	// Server mode has no root workspace, including with the CLI default.
	if err = Backup(ctx, data, filepath.Join(data, "workspace"), backup); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-token", "connection-key"} {
		if _, err = os.Stat(filepath.Join(backup, secret)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied %s: %v", secret, err)
		}
	}
	backupBytes, err := os.ReadFile(filepath.Join(backup, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(backupBytes, []byte(session)) {
		t.Fatal("session digest remains in backup database pages")
	}
	backedUp, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(backup, backupDB), "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err = backedUp.QueryRowContext(ctx, "SELECT count(*) FROM account_sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("backup retained %d sessions: %v", sessions, err)
	}
	if err = backedUp.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(data, backupDB), "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	if err = original.QueryRowContext(ctx, "SELECT count(*) FROM account_sessions").Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("backup changed source sessions: %d %v", sessions, err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(backup, backupManifestName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupManifest
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 2 || len(manifest.Accounts) != len(fixtures) {
		t.Fatalf("missing account manifest entries: %+v", manifest.Accounts)
	}
	// An old or modified v1 backup containing only the server root database
	// must not silently restore an instance with all personal data missing.
	legacy := filepath.Join(base, "legacy")
	if err = os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(legacy, backupDB), backupBytes, 0600); err != nil {
		t.Fatal(err)
	}
	legacyManifest := backupManifest{Version: 1, Schema: manifest.Schema, WorkspaceID: strings.Repeat("0", 64), Files: []backupFile{manifest.Files[0]}}
	legacyBytes, err := json.Marshal(legacyManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(legacy, backupManifestName), legacyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	legacyTarget := filepath.Join(base, "legacy-restored")
	if err = Restore(ctx, legacy, legacyTarget); err == nil {
		t.Fatal("incomplete v1 server backup was accepted")
	}
	if _, err = os.Stat(legacyTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete legacy restore published destination: %v", err)
	}

	restored := filepath.Join(base, "restored")
	if err = Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-token", "connection-key"} {
		if _, err = os.Stat(filepath.Join(restored, secret)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored %s: %v", secret, err)
		}
	}
	instance, err = Open(filepath.Join(restored, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = instance.AccountForSession(ctx, session); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("restored session: %v", err)
	}
	_, passwordHash, err := instance.AccountPassword(ctx, "admin")
	if err != nil || passwordHash != "admin-hash" {
		t.Fatalf("account password hash lost: %q %v", passwordHash, err)
	}
	c, err := instance.Connection(ctx, connection.ID)
	if err != nil || c.Enabled || c.Status != "untested" {
		t.Fatalf("connection remained enabled: %+v %v", c, err)
	}
	for _, fixture := range fixtures {
		dir := filepath.Join(restored, "account-data", fixture.id)
		store, err := OpenAccountBusinessStore(filepath.Join(dir, backupDB), instance)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.BindAccountBusinessStore(ctx, "user:"+fixture.id); err != nil {
			t.Fatal(err)
		}
		if err = store.OpenWorkspace(filepath.Join(dir, "workspace")); err != nil {
			t.Fatal(err)
		}
		if err = store.OpenUploads(filepath.Join(dir, "uploads")); err != nil {
			t.Fatal(err)
		}
		memories, err := store.Memories(ctx)
		if err != nil || len(memories) != 1 || memories[0].Content != fixture.id {
			t.Fatalf("incorrect personal memory: %+v %v", memories, err)
		}
		if _, err = store.UploadMaterialPart(ctx, fixture.upload, 1); err != nil {
			t.Fatalf("missing upload: %v", err)
		}
		material, err := os.ReadFile(filepath.Join(dir, "workspace", "materials", "private.md"))
		if err != nil || string(material) != "material:"+fixture.id {
			t.Fatalf("incorrect material: %q %v", material, err)
		}
		detail, err := store.Detail(fixture.task, true)
		if err != nil || detail.Publication == nil || detail.Publication.ID != fixture.publication {
			t.Fatalf("missing output receipt: %+v %v", detail, err)
		}
		if err = store.Accept(ctx, detail.Task.ID, detail.Artifact.ID, detail.Task.Revision); err != nil {
			t.Fatalf("matching output not accepted: %v", err)
		}
		queued, err := store.Detail(fixture.queued, true)
		if err != nil || queued.Run.Status != "interrupted" || queued.Task.Status != "interrupted" {
			t.Fatalf("queued work not interrupted: %+v %v", queued, err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = instance.Close(); err != nil {
		t.Fatal(err)
	}
	// Restoration independently invalidates sessions, even if an externally
	// supplied, checksum-consistent server backup contains one.
	rootPath := filepath.Join(backup, backupDB)
	injected, err := sql.Open("sqlite", sqliteFileURI(rootPath, ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = injected.ExecContext(ctx, `INSERT INTO account_sessions(token_digest,principal_id,status_version,expires_at,created_at)
SELECT 'injected-session',principal_id,status_version,?,? FROM accounts WHERE principal_id=?`, time.Now().Add(time.Hour).UnixMilli(), time.Now().UnixMilli(), admin.PrincipalID)
	if err = errors.Join(err, injected.Close()); err != nil {
		t.Fatal(err)
	}
	injectedBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	originalRootEntry := manifest.Files[0]
	manifest.Files[0].Bytes = int64(len(injectedBytes))
	manifest.Files[0].Digest = digest(string(injectedBytes))
	injectedManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifestPath, injectedManifest, 0600); err != nil {
		t.Fatal(err)
	}
	injectedRestore := filepath.Join(base, "injected-restored")
	if err = Restore(ctx, backup, injectedRestore); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(injectedRestore, backupDB), "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	if err = check.QueryRowContext(ctx, "SELECT count(*) FROM account_sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("restore retained %d injected sessions: %v", sessions, err)
	}
	if err = check.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rootPath, backupBytes, 0600); err != nil {
		t.Fatal(err)
	}
	manifest.Files[0] = originalRootEntry
	if err = os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(data, "account-data", fixtures[0].id, "uploads", "untracked.txt")
	if err = os.WriteFile(untracked, []byte("untracked private data"), 0600); err != nil {
		t.Fatal(err)
	}
	incomplete := filepath.Join(base, "incomplete")
	if err = Backup(ctx, data, filepath.Join(data, "workspace"), incomplete); !errors.Is(err, ErrUploadChanged) {
		t.Fatalf("untracked account upload was silently skipped: %v", err)
	}
	if _, err = os.Stat(incomplete); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete account backup published: %v", err)
	}
	if err = os.Remove(untracked); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join("account-data", fixtures[0].id, "workspace", "outputs", "report.md")
	if err = os.WriteFile(filepath.Join(backup, outputPath), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(base, "bad")
	if err = Restore(ctx, backup, bad); err == nil {
		t.Fatal("restore accepted a modified account output")
	}
	if _, err = os.Stat(bad); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published destination: %v", err)
	}
	// Even if the manifest is recomputed, the publication's own digest must
	// match the restored output before its receipt can be accepted.
	for i := range manifest.Files {
		if manifest.Files[i].Path == filepath.ToSlash(outputPath) {
			manifest.Files[i].Bytes = int64(len("changed"))
			manifest.Files[i].Digest = digest("changed")
		}
	}
	manifestBytes, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err = Restore(ctx, backup, bad); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(bad, "account-data", fixtures[0].id, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.OpenWorkspace(filepath.Join(bad, "account-data", fixtures[0].id, "workspace")); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(fixtures[0].task, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Accept(ctx, detail.Task.ID, detail.Artifact.ID, detail.Task.Revision); err == nil {
		t.Fatal("accepted a restored output whose content does not match the receipt")
	}
}

func TestServerBackupRejectsInvalidAccountDirectory(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "account-data", "invalid-account"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(data, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureAccountMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(base, "backup")
	if err = Backup(ctx, data, filepath.Join(data, "workspace"), backup); err == nil {
		t.Fatal("backup silently ignored invalid personal data")
	}
	if _, err = os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete backup published: %v", err)
	}
}

func TestServerBackupRestoreWithoutAccounts(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "account-data"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(data, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureAccountMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	backup, restored := filepath.Join(base, "backup"), filepath.Join(base, "restored")
	if err = Backup(ctx, data, filepath.Join(data, "workspace"), backup); err != nil {
		t.Fatal(err)
	}
	if err = Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(restored, "account-data"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("incorrect restored account directory: %v %v", entries, err)
	}
}

func TestServerBackupRejectsLegacyRootData(t *testing.T) {
	for _, kind := range []string{"database upload", "orphan upload", "external workspace"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			data := filepath.Join(base, "data")
			if err := os.MkdirAll(filepath.Join(data, "account-data"), 0700); err != nil {
				t.Fatal(err)
			}
			store, err := Open(filepath.Join(data, backupDB))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.EnsureAccountMode(ctx, true); err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(data, "workspace")
			switch kind {
			case "database upload":
				if err = store.OpenUploads(filepath.Join(data, "uploads")); err != nil {
					t.Fatal(err)
				}
				if _, err = store.UploadMaterial(ctx, "old.txt", []byte("old private data")); err != nil {
					t.Fatal(err)
				}
			case "orphan upload":
				if err = os.Mkdir(filepath.Join(data, "uploads"), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(data, "uploads", "old.txt"), []byte("old private data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "external workspace":
				workspace = filepath.Join(base, "external")
				if err = os.MkdirAll(filepath.Join(workspace, "outputs"), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(workspace, "outputs", "old.txt"), []byte("old private data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(base, "backup")
			if err = Backup(ctx, data, workspace, backup); !errors.Is(err, errLegacyServerData) {
				t.Fatalf("legacy data not rejected: %v", err)
			}
			if _, err = os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete backup published: %v", err)
			}
		})
	}
}
