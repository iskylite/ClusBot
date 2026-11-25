package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationReceiptCorrelation(t *testing.T) {
	s, _, op := setupJobsStore(t, "v31-receipts")
	stamp := recordsTestNow()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	oldAttempt := dispatchJobAttempt(t, tx, op, stamp)
	attempt := dispatchJobAttempt(t, tx, op, stamp+1)
	if _, err := normalizeJobStart(tx, op, "job-1", "key-1", "7d", stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeJobStart(tx, op, "job-1", "wrong", "7d", stamp); !errors.Is(err, ErrConflict) {
		t.Fatalf("job lookup silently changed identity: %v", err)
	}
	base := OperationReceipt{
		OperationID: op, AttemptID: attempt, TargetRef: "job-1",
		ExternalVersion: 2, QueryGeneration: 3, PayloadDigest: digest("closed"), Source: "poll",
	}
	receive := func(r OperationReceipt) ReceiveResult {
		t.Helper()
		got, err := receiveOperationReceipt(tx, r, stamp)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	bad := base
	bad.AttemptID = "other-attempt"
	if got := receive(bad); !got.Quarantined || got.Event.EventID != "" {
		t.Fatalf("unknown attempt accepted: %+v", got)
	}
	bad = base
	bad.TargetRef = "other-job"
	if got := receive(bad); !got.Quarantined || got.Event.EventID != "" {
		t.Fatalf("unknown target accepted: %+v", got)
	}
	apply := func(r ReceiveResult, gen int, want bool) ApplyReceiptResult {
		t.Helper()
		got, err := applyOperationReceipt(tx, r.Event.EventID, gen, stamp)
		if err != nil || got.Applied != want {
			t.Fatalf("apply %s = %+v, %v; want %v", r.Event.EventID, got, err, want)
		}
		return got
	}
	older := base
	older.AttemptID = oldAttempt
	apply(receive(older), 3, false)
	future := base
	future.ExternalVersion, future.QueryGeneration = 9, 4
	apply(receive(future), 3, false)
	accepted := receive(base)
	apply(accepted, 3, true)
	duplicate := receive(base)
	if !duplicate.Duplicate || duplicate.Event.EventID != accepted.Event.EventID {
		t.Fatalf("duplicate receipt created a new identity: %+v", duplicate)
	}
	bad = base
	bad.PayloadDigest = digest("contradiction")
	if !receive(bad).Quarantined {
		t.Fatal("same-source conflicting payload not quarantined")
	}
	bad.Source = "callback"
	if got := apply(receive(bad), 3, false); !strings.Contains(got.Reason, "不一致") {
		t.Fatalf("cross-source conflict not reported: %+v", got)
	}
	older = base
	older.ExternalVersion = 1
	apply(receive(older), 3, false)
	// A second target can report the same provider version independently.
	other := base
	other.TargetRef = "asset-1"
	apply(receive(other), 3, true)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationReceiptMigrationPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DROP TABLE operation_receipts`); err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(runtimeOperationJobsSchema,
		"UNIQUE(operation_id, attempt_id, target_ref, external_version, source)",
		"UNIQUE(operation_id, external_version, source)", 1)
	if _, err := tx.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	insert := func(tx *sql.Tx, id, target string) error {
		_, err := tx.Exec(`INSERT INTO operation_receipts(receipt_id,operation_id,attempt_id,target_ref,external_version,
query_generation,payload_digest,receipt,source,received_at,state) VALUES(?,'op','attempt',?,1,1,'d','original','poll',1,'applied')`, id, target)
		return err
	}
	if err := insert(tx, "existing", "asset-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("PRAGMA user_version=91"); err != nil {
		t.Fatal(err)
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
	defer s.Close()
	var content, state string
	if err := s.db.QueryRow("SELECT receipt,state FROM operation_receipts WHERE receipt_id='existing'").Scan(&content, &state); err != nil || content != "original" || state != "applied" {
		t.Fatalf("original receipt lost across migration: %q %q %v", content, state, err)
	}
	tx, err = s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insert(tx, "second", "asset-2"); err != nil {
		t.Fatalf("different target shares a provider version: %v", err)
	}
}
