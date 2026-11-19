package work

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttentionFeedDurableResumeAndScope(t *testing.T) {
	ctx, stamp := t.Context(), now()
	path := filepath.Join(t.TempDir(), "attention-feed.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	first, err := s.ReadAttentionFeed(ctx, "owner:a", "", 1)
	if err != nil || first.Cursor == "" || first.Events == nil || len(first.Events) != 0 || first.HasMore {
		t.Fatalf("initial cursor: %+v %v", first, err)
	}
	add := func(actor string, commit bool) Approval {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		a, err := requestApproval(tx, Approval{Subject: actor, Action: "maintain", ObjectRef: "asset", ParamsDigest: "never-export"}, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	a := add("owner:a", true)
	b := add("owner:b", true)
	add("owner:a", false) // No event or sequence hole after rollback.
	if _, err := s.db.Exec("UPDATE approvals SET decision='approved',decided_at=? WHERE approval_id=?", stamp+1, a.ApprovalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE approvals SET decision='approved' WHERE approval_id=?", a.ApprovalID); err != nil {
		t.Fatal(err)
	}
	p, err := s.ReadAttentionFeed(ctx, "owner:a", first.Cursor, 1)
	if err != nil || len(p.Events) != 1 || p.Events[0].Sequence != 1 || p.Events[0].ItemID != "approval:"+a.ApprovalID ||
		p.Events[0].Kind != "approval_created" || !p.HasMore {
		t.Fatalf("first replay: %+v %v", p, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.ReadAttentionFeed(ctx, "owner:a", p.Cursor, 1)
	if err != nil || len(q.Events) != 1 || q.Events[0].Sequence != 2 || q.Events[0].State != "approved" ||
		q.Events[0].Kind != "approval_decided" || q.HasMore {
		t.Fatalf("restart replay: %+v %v", q, err)
	}
	end, err := s.ReadAttentionFeed(ctx, "owner:a", q.Cursor, 100)
	if err != nil || len(end.Events) != 0 || end.Cursor != q.Cursor {
		t.Fatalf("same-state decision produced duplicate: %+v %v", end, err)
	}
	r, err := s.ReadAttentionFeed(ctx, "owner:b", "", 100)
	if err != nil || len(r.Events) != 1 || r.Events[0].ItemID != "approval:"+b.ApprovalID || r.Events[0].Sequence != 1 {
		t.Fatalf("actor's stream leaked event/count: %+v %v", r, err)
	}
	if _, err := s.ReadAttentionFeed(ctx, "owner:b", q.Cursor, 10); !errors.Is(err, ErrAttentionCursorReset) {
		t.Fatalf("foreign cursor accepted: %v", err)
	}
	if _, err := s.ReadAttentionFeed(ctx, "owner:a", strings.Replace(q.Cursor, ":2:", ":23:", 1), 10); !errors.Is(err, ErrAttentionCursorReset) {
		t.Fatalf("future cursor accepted: %v", err)
	}
	// A stale cursor must not treat a different fork of a restored database's
	// event sequence as the same acknowledged row.
	if _, err := s.db.Exec("UPDATE attention_feed_events SET proof='different' WHERE actor='owner:a' AND sequence=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAttentionFeed(ctx, "owner:a", q.Cursor, 10); !errors.Is(err, ErrAttentionCursorReset) {
		t.Fatalf("restored/incompatible cursor accepted: %v", err)
	}
}
