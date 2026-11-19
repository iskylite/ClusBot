package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestListAttentionApprovalScopeAndExpiry(t *testing.T) {
	ctx, stamp := t.Context(), now()
	s, err := Open(filepath.Join(t.TempDir(), "attention.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(actor, object string, expiry, created int64) Approval {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		a, err := requestApproval(tx, Approval{Subject: actor, Action: "maintain", ObjectRef: object, ParamsDigest: "digest-never-expose", ExpiresAt: expiry}, created)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return a
	}
	late := add("actor:a", "asset:a", stamp, stamp-500)
	pending := add("actor:a", "asset:a", stamp+100, stamp-200)
	another := add("actor:b", "asset:b", stamp+100, stamp-100)

	p, err := s.ListAttention(ctx, "actor:a", stamp, 1, AttentionCursor{})
	if err != nil || len(p.Items) != 1 || p.Items[0].ApprovalID != late.ApprovalID || p.Items[0].State != "expired" || p.Next == nil || p.SnapshotAt != stamp {
		t.Fatalf("expiry at boundary/paging: %+v %v", p, err)
	}
	q, err := s.ListAttention(ctx, "actor:a", stamp, 1, *p.Next)
	if err != nil || len(q.Items) != 1 || q.Items[0].ApprovalID != pending.ApprovalID || q.Items[0].State != "decision_required" || q.Next != nil {
		t.Fatalf("next page: %+v %v", q, err)
	}
	r, err := s.ListAttention(ctx, "actor:b", stamp, 50, AttentionCursor{})
	if err != nil || len(r.Items) != 1 || r.Items[0].ApprovalID != another.ApprovalID {
		t.Fatalf("actor B: %+v %v", r, err)
	}
	r, err = s.ListAttention(ctx, "actor:unused", stamp, 50, AttentionCursor{})
	if err != nil || r.Items == nil || len(r.Items) != 0 {
		t.Fatalf("empty: %+v %v", r, err)
	}
	if _, err = s.ListAttention(ctx, "user:anonymous", stamp, 50, AttentionCursor{}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("anonymous access: %v", err)
	}
	var state string
	if err = s.db.QueryRow("SELECT decision FROM approvals WHERE approval_id=?", late.ApprovalID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("projection must not alter approval decision: %s %v", state, err)
	}
}

func TestReadAttentionReopensDecisionAfterLeavingList(t *testing.T) {
	ctx, stamp := t.Context(), now()
	s, err := Open(filepath.Join(t.TempDir(), "attention-detail.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`INSERT INTO approvals(approval_id,subject,action,object_ref,params_digest,
contract_revision,policy_version,asset_revision,decided_by,expires_at,decision,request_key,created_at)
VALUES('confirmed','owner','maintain','asset','redacted',4,5,6,'',?,'pending','request',?)`, stamp+500, stamp-100); err != nil {
		t.Fatal(err)
	}
	const itemID = "approval:confirmed"
	check := func(actor string, stamp int64, state string) AttentionDetail {
		t.Helper()
		detail, err := s.ReadAttention(ctx, actor, itemID, stamp)
		if err != nil || detail.ID != itemID || detail.State != state || detail.SnapshotAt != stamp || detail.Source.Ref != "confirmed" ||
			detail.Source.ContractRevision != 4 || detail.Source.PolicyVersion != 5 || detail.Source.AssetRevision != 6 {
			t.Fatalf("state %s expected %s: %+v %v", detail.State, state, detail, err)
		}
		return detail
	}
	check("owner", stamp, "decision_required")
	check("owner", stamp+500, "expired")
	if _, err := s.ReadAttention(ctx, "foreign", itemID, stamp); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign detail revealed: %v", err)
	}
	if _, err := s.db.Exec("UPDATE approvals SET decision='approved',decided_at=? WHERE approval_id='confirmed'", stamp+10); err != nil {
		t.Fatal(err)
	}
	// Once approved, a card opened from a previous page remains readable even
	// after the old expiry. It must not revert to an inferred timeout.
	detail := check("owner", stamp+500, "approved")
	if detail.OccurredAt != stamp+10 || detail.Source.DecisionAt != stamp+10 {
		t.Fatalf("wrong source decision time: %+v", detail)
	}
	page, err := s.ListAttention(ctx, "owner", stamp+500, 50, AttentionCursor{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("approved decision not removed from pending list: %+v %v", page, err)
	}
}
