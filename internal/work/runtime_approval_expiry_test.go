package work

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestApprovalExpiryRuntimeBoundedWithoutModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval-expiry.sqlite")
	ctx, stamp := t.Context(), now()-1000
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < approvalExpiryBatch+2; i++ {
		if _, err := requestApproval(tx, Approval{Subject: "owner", Action: "write", ObjectRef: "asset", ParamsDigest: "digest",
			DecidedBy: "intended-approver", ExpiresAt: stamp, RequestKey: fmt.Sprintf("due-%d", i)}, stamp-100); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	for i, expires := range []int64{0, stamp + 120_000} {
		if _, err := requestApproval(tx, Approval{Subject: "owner", Action: "write", ObjectRef: "asset", ParamsDigest: "digest",
			ExpiresAt: expires, RequestKey: fmt.Sprintf("live-%d", i)}, stamp-100); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverRuntime(ctx, stamp)
	if err != nil || report.ExpiredApprovals != approvalExpiryBatch {
		t.Fatalf("startup must process one bounded batch: %+v %v", report, err)
	}
	count := func(state string, want int) {
		t.Helper()
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM approvals WHERE decision=?", state).Scan(&n); err != nil || n != want {
			t.Fatalf("state %s got %d wanted %d: %v", state, n, want, err)
		}
	}
	count("pending", 4)
	runtime := NewRuntimeLoop(s, func() bool { return false })
	runtime.tick(ctx)
	runtime.tick(ctx)
	count("pending", 2)
	count("expired", approvalExpiryBatch+2)
	var attributed, effects, notices, feedRows int
	for _, q := range []struct {
		sql  string
		dest *int
	}{
		{"SELECT count(*) FROM approvals WHERE decision='expired' AND (decided_by<>'' OR decided_at<>expires_at)", &attributed},
		{"SELECT count(*) FROM operations", &effects},
		{"SELECT count(*) FROM outbox_commands", &notices},
		{"SELECT count(*) FROM attention_feed_events WHERE actor='owner' AND kind='approval_decided' AND state='expired'", &feedRows},
	} {
		if err := s.db.QueryRow(q.sql).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	if attributed != 0 || effects != 0 || notices != 0 || feedRows != approvalExpiryBatch+2 {
		t.Fatalf("synthetic decision/effect or duplicate feed: %d %d %d %d", attributed, effects, notices, feedRows)
	}
}

func TestApprovalExpiryPreservesDecisionAndWinsLateRace(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "approval-race.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stamp := now()
	const actor = "user:session"
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := grantAuthorization(tx, Authorization{Subject: actor, Action: "confirm", ObjectRef: "asset", ValidFrom: stamp - 10000, Owner: actor}, stamp-10000); err != nil {
		t.Fatal(err)
	}
	add := func(key string, expires int64) Approval {
		t.Helper()
		a, err := requestApproval(tx, Approval{Subject: actor, Action: "write", ObjectRef: "asset", ParamsDigest: "digest", RequestKey: key, ExpiresAt: expires, DecidedBy: actor}, stamp-500)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	early := add("early", stamp)
	if _, err := decideApproval(tx, early.RequestKey, "approved", actor, Approval{}, stamp-1); err != nil {
		t.Fatal(err)
	}
	late := add("late", stamp)
	lateOnly := add("late-only", stamp)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DecideApproval(ctx, lateOnly.ApprovalID, "rejected", actor); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("late attempt must return typed conflict: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM attention_feed_events WHERE item_id=? AND state='expired'", "approval:"+lateOnly.ApprovalID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("late attempt did not commit expiry: %d %v", n, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		_, err := s.expirePendingApprovals(context.Background(), stamp, 50)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := s.DecideApproval(context.Background(), late.ApprovalID, "approved", actor)
		if !errors.Is(err, ErrApprovalExpired) {
			err = fmt.Errorf("late decision did not return expiry: %v", err)
		} else {
			err = nil
		}
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		a    Approval
		want string
	}{{early, "approved"}, {late, "expired"}} {
		got, err := s.ReadAttention(ctx, actor, "approval:"+tt.a.ApprovalID, now())
		if err != nil || got.State != tt.want {
			t.Fatalf("decision state %+v expected %s: %v", got, tt.want, err)
		}
		if err := s.db.QueryRow("SELECT count(*) FROM attention_feed_events WHERE item_id=? AND kind='approval_decided'", "approval:"+tt.a.ApprovalID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("duplicate expiry/decision signal: %d %v", n, err)
		}
	}
}
