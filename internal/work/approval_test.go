package work

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExpiredPublicationRequiresFreshApproval(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		name := "decision-after-deadline"
		if sweep {
			name = "background-expiry"
		}
		t.Run(name, func(t *testing.T) {
			s, _, root, detail := publicationFixture(t)
			ctx := t.Context()
			filename := "纪要.md"
			target := filepath.Join(root, "outputs", filename)
			if err := os.WriteFile(target, []byte("原始纪要"), 0600); err != nil {
				t.Fatal(err)
			}
			p := requestWrite(t, s, detail, filename, "original-publication")
			if _, err := s.db.Exec("UPDATE publications SET expires_at=0 WHERE id=?", p.ID); err != nil {
				t.Fatal(err)
			}
			if sweep {
				if err := s.ExpireApprovals(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DecidePublication(ctx, p.ID, "approve", p.taskRevision); !errors.Is(err, ErrApprovalExpired) {
				t.Fatalf("late approval returned %v", err)
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != "原始纪要" {
				t.Fatalf("expired approval changed the target: %q, %v", body, err)
			}
			expired, err := s.Detail(detail.Task.ID, true)
			if err != nil || expired.Publication == nil || expired.Publication.Status != "expired" || expired.Publication.Decision != "" ||
				expired.Task.Status != "waiting_user" || expired.Task.StatusReason != "acceptance_required" || expired.Artifact.ID != detail.Artifact.ID {
				t.Fatalf("expired approval or task state: %+v, %v", expired, err)
			}
			if err = s.ExpireApprovals(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := s.Detail(detail.Task.ID, true)
			if err != nil || again.Task.Revision != expired.Task.Revision {
				t.Fatalf("repeated expiry changed task revision: %+v, %v", again.Task, err)
			}
			if err = os.WriteFile(target, []byte("更新后的原文件"), 0600); err != nil {
				t.Fatal(err)
			}
			fresh := requestWrite(t, s, expired, filename, "fresh-publication")
			if fresh.ID == p.ID || fresh.PreviousDigest != digest("更新后的原文件") {
				t.Fatalf("approval didn't capture the current target: %+v", fresh)
			}
			if _, payload, err := s.beginPublication(ctx, fresh.ID, "approve", fresh.taskRevision); err != nil || payload == nil {
				t.Fatalf("fresh approval failed: %v", err)
			}
			if _, err = s.db.Exec("UPDATE publications SET expires_at=0 WHERE id=?", fresh.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.ExpireApprovals(ctx); err != nil {
				t.Fatal(err)
			}
			dispatched, err := s.Detail(detail.Task.ID, true)
			if err != nil || dispatched.Publication.Status != "dispatched" || dispatched.Publication.Decision != "approve" {
				t.Fatalf("expiry altered a dispatched write: %+v, %v", dispatched.Publication, err)
			}
		})
	}
}
