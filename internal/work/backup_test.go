package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestBackupRestoreRetainsEvidenceWithoutReplayingWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "external-workspace")
	db, err := Open(filepath.Join(data, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.OpenWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	material := filepath.Join(workspace, "materials", "会议.md")
	if err = os.WriteFile(material, []byte("会议决定"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, "access-token"), []byte("old-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SaveMemory(ctx, "", Memory{Kind: "preference", Title: "周报", Content: "先写结论"}, 0); err != nil {
		t.Fatal(err)
	}
	connection, err := db.SaveConnection(ctx, "", Connection{Name: "测试资料", Enabled: true,
		Config: mcpclient.Config{Transport: "http", Endpoint: "http://127.0.0.1:4444/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	finishTask := func(key string, delivery Delivery) Detail {
		t.Helper()
		run, err := db.SubmitWithDelivery(ctx, key, "整理会议", "", "", true, delivery)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.claim(ctx); err != nil {
			t.Fatal(err)
		}
		if err = db.delta(ctx, run.ID, "# 会议"); err != nil {
			t.Fatal(err)
		}
		if err = db.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		detail, err := db.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	name := "报告.md"
	delivered := finishTask("backup-delivery", Delivery{Filename: &name})
	publication := requestWrite(t, db, delivered, name, "backup-write")
	if p, err := db.DecidePublication(ctx, publication.ID, "approve", publication.taskRevision); err != nil || p.Status != "succeeded" {
		t.Fatalf("write: %+v, %v", p, err)
	}
	waiting := finishTask("backup-pending", Delivery{})
	pending := requestWrite(t, db, waiting, "草稿.md", "backup-approval")
	queued, err := db.Submit(ctx, "backup-queued", "稍后处理", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(base, "snapshot")
	lock, err := LockDataDir(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = Backup(ctx, data, workspace, backup); err == nil {
		t.Fatal("backup ignored live instance lock")
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = Backup(ctx, data, workspace, backup); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(backup, backupManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if string(manifest["version"]) != "1" || manifest["accounts"] != nil {
		t.Fatalf("local backup format changed: %s", manifestBytes)
	}
	if _, err = os.Stat(filepath.Join(backup, "access-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("access credential copied: %v", err)
	}
	if err = Restore(ctx, backup, data); err == nil {
		t.Fatal("restore replaced existing data directory")
	}

	restored := filepath.Join(base, "restored")
	if err = Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	newDB, err := Open(filepath.Join(restored, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()
	if err = newDB.OpenWorkspace(filepath.Join(restored, "workspace")); err != nil {
		t.Fatal(err)
	}
	d, err := newDB.Detail(delivered.Task.ID, true)
	if err != nil || d.Publication == nil || d.Publication.ID != publication.ID {
		t.Fatalf("lost receipt: %+v, %v", d.Publication, err)
	}
	if err = newDB.Accept(ctx, d.Task.ID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatalf("restored output cannot be accepted: %v", err)
	}
	d, err = newDB.Detail(waiting.Task.ID, true)
	if err != nil || d.Publication == nil || d.Publication.ID != pending.ID || d.Publication.Status != "expired" {
		t.Fatalf("old approval still active: %+v, %v", d.Publication, err)
	}
	d, err = newDB.Detail(queued.TaskID, true)
	if err != nil || d.Run.Status != "interrupted" || d.Task.Status != "interrupted" {
		t.Fatalf("queued work could replay: %+v, %v", d.Run, err)
	}
	memories, err := newDB.Memories(ctx)
	if err != nil || len(memories) != 1 || memories[0].Content != "先写结论" {
		t.Fatalf("lost preference: %+v, %v", memories, err)
	}
	c, err := newDB.Connection(ctx, connection.ID)
	if err != nil || c.Enabled || c.Config.Transport != connection.Config.Transport || c.Config.Endpoint != connection.Config.Endpoint {
		t.Fatalf("connection restore: %+v, %v", c, err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "workspace", "materials", "会议.md"))
	if err != nil || string(got) != "会议决定" {
		t.Fatalf("lost workspace input: %s, %v", got, err)
	}

	if err = os.WriteFile(filepath.Join(backup, "workspace", "materials", "会议.md"), []byte("文件被改写"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(base, "corrupt-restore")
	if err = Restore(ctx, backup, bad); err == nil {
		t.Fatal("restore accepted a modified backup file")
	}
	if _, err = os.Stat(bad); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published a target: %v", err)
	}
}
