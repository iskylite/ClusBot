package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func publicationFixture(t *testing.T) (*Store, string, string, Detail) {
	t.Helper()
	base := t.TempDir()
	db, root := filepath.Join(base, "test.db"), filepath.Join(base, "workspace")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.OpenWorkspace(root); err != nil {
		t.Fatal(err)
	}
	r, err := s.Submit(context.Background(), "publication-task", "整理纪要", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.delta(context.Background(), r.ID, "# 项目纪要\n"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(r, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(r.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, root, d
}

func requestWrite(t *testing.T, s *Store, d Detail, name, key string) Publication {
	t.Helper()
	p, err := s.RequestPublication(context.Background(), d.Task.ID, d.Artifact.ID, name, key, d.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublicationRequiresApprovalAndCannotOverwrite(t *testing.T) {
	s, _, root, d := publicationFixture(t)
	ctx := context.Background()
	if _, err := s.RequestPublication(ctx, d.Task.ID, d.Artifact.ID, "../escape.md", "invalid", d.Task.Revision); !errors.Is(err, ErrFilename) {
		t.Fatalf("path outside outputs: %v", err)
	}
	p := requestWrite(t, s, d, "纪要.md", "deny-request")
	target := filepath.Join(root, "outputs", "纪要.md")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote before approval: %v", err)
	}
	if err := s.Accept(ctx, d.Task.ID, d.Artifact.ID, p.taskRevision); !errors.Is(err, ErrPublicationOpen) {
		t.Fatalf("accept bypassed approval: %v", err)
	}
	denied, err := s.DecidePublication(ctx, p.ID, "deny", p.taskRevision)
	if err != nil || denied.Status != "denied" {
		t.Fatalf("deny: %+v %v", denied, err)
	}
	if _, err = os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denial wrote target: %v", err)
	}
	if _, err = s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); !errors.Is(err, ErrState) {
		t.Fatalf("reused denied approval: %v", err)
	}
	d, err = s.Detail(d.Task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	p = requestWrite(t, s, d, "纪要.md", "race-request")
	if err = os.WriteFile(target, []byte("其他程序的内容"), 0600); err != nil {
		t.Fatal(err)
	}
	conflict, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision)
	if err != nil || conflict.Status != "conflict" {
		t.Fatalf("new target must conflict: %+v %v", conflict, err)
	}
	bytes, err := os.ReadFile(target)
	if err != nil || string(bytes) != "其他程序的内容" {
		t.Fatalf("overwrote target: %q %v", bytes, err)
	}
}

func TestRequiredFileNeedsPublicationForCurrentArtifact(t *testing.T) {
	s, _, _, first := publicationFixture(t)
	ctx := context.Background()
	filename := "纪要.md"
	p := requestWrite(t, s, first, filename, "first-version")
	if approved, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); err != nil || approved.Status != "succeeded" {
		t.Fatalf("write initial version: %+v, %v", approved, err)
	}
	current, err := s.Detail(first.Task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	delivery := Delivery{Filename: &filename, Revision: current.Task.Revision}
	run, err := s.SubmitWithDelivery(ctx, "revised-delivery", "补充决策人", "", current.Task.ConversationID, false, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.delta(ctx, run.ID, first.Artifact.Content); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	revised, err := s.Detail(first.Task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Accept(ctx, revised.Task.ID, revised.Artifact.ID, revised.Task.Revision); !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("older publication satisfied new artifact: %v", err)
	}
}

func TestOverwriteRequiresTheApprovedVersionAndDoesNotReplay(t *testing.T) {
	s, _, root, d := publicationFixture(t)
	ctx := context.Background()
	target := filepath.Join(root, "outputs", "纪要.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(target, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	requireFile := func(want string) {
		t.Helper()
		body, err := os.ReadFile(target)
		if err != nil || string(body) != want {
			t.Fatalf("target = %q, %v; want %q", body, err, want)
		}
	}
	write("第一版")
	p := requestWrite(t, s, d, "纪要.md", "overwrite-stale")
	pending, err := s.Detail(d.Task.ID, true)
	if err != nil || pending.Publication == nil || pending.Publication.PreviousContent == nil || *pending.Publication.PreviousContent != "第一版" {
		t.Fatalf("missing approved preview: %+v, %v", pending.Publication, err)
	}
	write("外部修改")
	if got, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); err != nil || got.Status != "conflict" {
		t.Fatalf("stale approval: %+v, %v", got, err)
	}
	requireFile("外部修改")
	d, err = s.Detail(d.Task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	p = requestWrite(t, s, d, "纪要.md", "overwrite-fresh")
	if got, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); err != nil || got.Status != "succeeded" {
		t.Fatalf("fresh approval: %+v, %v", got, err)
	}
	requireFile(d.Artifact.Content)
	write("批准之后的修改")
	if got, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); err != nil || got.Status != "succeeded" {
		t.Fatalf("duplicate approval: %+v, %v", got, err)
	}
	requireFile("批准之后的修改")
}

func TestOverwriteRecoveryUsesDiskEvidence(t *testing.T) {
	for _, installed := range []bool{false, true} {
		name := "restore-original"
		if installed {
			name = "verify-replacement"
		}
		t.Run(name, func(t *testing.T) {
			s, db, root, d := publicationFixture(t)
			ctx := context.Background()
			const original = "原始纪要"
			target := filepath.Join(root, "outputs", "纪要.md")
			if err := os.WriteFile(target, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			p := requestWrite(t, s, d, "纪要.md", "overwrite-crash")
			p, content, err := s.beginPublication(ctx, p.ID, "approve", p.taskRevision)
			if err != nil || content == nil {
				t.Fatalf("prepare write: %+v, %v", p, err)
			}
			if installed {
				if status, detail, _ := s.writePublication(p, *content); status != "succeeded" {
					t.Fatalf("write: %s, %s", status, detail)
				}
			} else if err = s.workspace.moveOutput(p.Filename, p.backupName()); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(db)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = reopened.OpenWorkspace(root); err != nil {
				t.Fatal(err)
			}
			if err = reopened.RecoverPublications(); err != nil {
				t.Fatal(err)
			}
			recovered, err := reopened.Detail(d.Task.ID, true)
			want, status := original, "conflict"
			if installed {
				want, status = *content, "succeeded"
			}
			if err != nil || recovered.Publication == nil || recovered.Publication.Status != status {
				t.Fatalf("recovery: %+v, %v", recovered.Publication, err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != want {
				t.Fatalf("target after restart: %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestOverwriteRecoveryPreservesConcurrentFile(t *testing.T) {
	s, _, root, d := publicationFixture(t)
	ctx := context.Background()
	output := filepath.Join(root, "outputs")
	target := filepath.Join(output, "纪要.md")
	if err := os.WriteFile(target, []byte("原文件"), 0600); err != nil {
		t.Fatal(err)
	}
	p := requestWrite(t, s, d, "纪要.md", "overwrite-concurrent")
	p, _, err := s.beginPublication(ctx, p.ID, "approve", p.taskRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.workspace.moveOutput(p.Filename, p.backupName()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, []byte("并发写入"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReconcilePublication(p.ID); err != nil || got.Status != "unknown" {
		t.Fatalf("concurrent file was not flagged: %+v, %v", got, err)
	}
	current, err := os.ReadFile(target)
	if err != nil || string(current) != "并发写入" {
		t.Fatalf("concurrent file was changed: %q, %v", current, err)
	}
	if err = os.Rename(target, filepath.Join(output, "并发版本.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReconcilePublication(p.ID); err != nil || got.Status != "conflict" {
		t.Fatalf("original was not restored: %+v, %v", got, err)
	}
	restored, err := os.ReadFile(target)
	if err != nil || string(restored) != "原文件" {
		t.Fatalf("wrong restored file: %q, %v", restored, err)
	}
}

func TestPublicationRecoveryChecksInodeAndDoesNotReplay(t *testing.T) {
	s, db, root, d := publicationFixture(t)
	ctx := context.Background()
	p := requestWrite(t, s, d, "纪要.md", "crash-request")
	p, content, err := s.beginPublication(ctx, p.ID, "approve", p.taskRevision)
	if err != nil || content == nil {
		t.Fatalf("begin: %+v %v", p, err)
	}
	status, _, _ := s.writePublication(p, *content)
	if status != "succeeded" {
		t.Fatalf("write: %s", status)
	}
	// Simulate process loss after the file is linked and before result commit.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.OpenWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverPublications(); err != nil {
		t.Fatal(err)
	}
	d, err = s.Detail(d.Task.ID, true)
	if err != nil || d.Publication == nil || d.Publication.Status != "succeeded" {
		t.Fatalf("recover: %+v %v", d.Publication, err)
	}
	target := filepath.Join(root, "outputs", "纪要.md")
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	repeat, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision)
	if err != nil || repeat.Status != "succeeded" {
		t.Fatalf("replayed receipt: %+v %v", repeat, err)
	}
	if _, err = os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate rewrote file: %v", err)
	}
}

func TestPublicationRecoveryKeepsUnprovenResultUnknown(t *testing.T) {
	s, _, root, d := publicationFixture(t)
	ctx := context.Background()
	p := requestWrite(t, s, d, "纪要.md", "unknown-request")
	p, _, err := s.beginPublication(ctx, p.ID, "approve", p.taskRevision)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outputs", p.Filename)
	if err = os.WriteFile(target, []byte(d.Artifact.Content), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := s.ReconcilePublication(p.ID)
	if err != nil || result.Status != "unknown" {
		t.Fatalf("hash alone is not proof: %+v %v", result, err)
	}
	if _, err = s.Submit(ctx, "followup", "继续", "", d.Task.ConversationID, false); !errors.Is(err, ErrPublicationOpen) {
		t.Fatalf("new Run bypassed unknown operation: %v", err)
	}
}
