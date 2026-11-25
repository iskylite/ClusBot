package work

import (
	"database/sql"
	"errors"
	"testing"
)

func TestApprovalDecisionOwnerAndSnapshot(t *testing.T) {
	s := setupApprovalsStore(t, "approval-identity.sqlite")
	const owner, other = "user:alice", "user:bob"
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, actor := range []string{owner, other} {
		if _, err := grantAuthorization(tx, Authorization{Subject: actor, Action: "confirm", ObjectRef: "asset-x", ValidFrom: stamp - 1, Owner: actor}, stamp-1); err != nil {
			t.Fatal(err)
		}
	}
	add := func(key, subject, approver string, expiry int64) Approval {
		t.Helper()
		a := approvalFixture(key, subject, "write", "asset-x", stamp-10)
		a.DecidedBy, a.ExpiresAt = approver, expiry
		out, err := requestApproval(tx, a, stamp-10)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	human := add("human", owner, owner, stamp+60000)
	named := add("named", owner, other, stamp+60000)
	elapsed := add("elapsed", owner, owner, stamp)
	if _, err := decideApproval(tx, human.RequestKey, "approved", owner, Approval{PolicyVersion: 2}, stamp); !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("wrong policy version should not mutate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	decide := func(a Approval, actor string, want error) {
		t.Helper()
		if _, err := s.DecideApproval(t.Context(), a.ApprovalID, "approved", actor); !errors.Is(err, want) {
			t.Fatalf("decide(%s, %s): got %v, want %v", a.RequestKey, actor, err, want)
		}
	}
	decide(human, other, sql.ErrNoRows)
	decide(named, owner, ErrAuthorizationDenied)
	decide(elapsed, other, sql.ErrNoRows)
	var state string
	if err := s.db.QueryRow("SELECT decision FROM approvals WHERE approval_id=?", elapsed.ApprovalID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("foreign caller persisted expiry: %q %v", state, err)
	}
	decide(human, owner, nil)
	decide(human, other, sql.ErrNoRows)
	decide(elapsed, owner, ErrApprovalExpired)
	decide(elapsed, other, sql.ErrNoRows)
	var effects, changes int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM operations) + (SELECT count(*) FROM outbox_commands),
(SELECT count(*) FROM attention_feed_events WHERE actor=? AND kind='approval_decided')`, owner).Scan(&effects, &changes); err != nil || effects != 0 || changes != 2 {
		t.Fatalf("effects=%d decisions=%d err=%v", effects, changes, err)
	}
}
