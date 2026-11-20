package work

import (
	"errors"
	"os"
	"sync"
	"testing"
)

func explicitTaskAdmissionDBFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "clusbot-explicit-admission-*.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test database: %v", err)
			}
		}
	})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExplicitTaskAdmissionCapacityIsDurableAndBeforeAccept(t *testing.T) {
	path := explicitTaskAdmissionDBFile(t)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	must(err)
	defer func() { s.Close() }()
	const actor, input = "user:session", "write a local report"
	admit := func(key, text string, limit int) (AdmissionResult, error) {
		return s.AdmitExplicitTask(ctx, key, actor, text, "", Delivery{}, nil, false, limit)
	}
	exec := func(stmt string, args ...any) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, stmt, args...)
		must(err)
	}
	blocked := func(key string) {
		t.Helper()
		if _, err := admit(key, input, 1); !errors.Is(err, ErrExplicitTaskQueueFull) {
			t.Fatalf("admission %s should be limited, got %v", key, err)
		}
		var n int
		must(s.db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM inbox_events WHERE source='chat' AND event_id=?)
 +(SELECT count(*) FROM submissions WHERE request_key=?)
 +(SELECT count(*) FROM submission_classifications WHERE request_key=?)`, key, key, key).Scan(&n))
		if n != 0 {
			t.Fatalf("rejected key acquired durable state: %s", key)
		}
	}

	type outcome struct {
		key string
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, key := range []string{"explicit-a", "explicit-b"} {
		wg.Go(func() {
			<-start
			receipt, err := admit(key, input, 1)
			if err == nil && (!receipt.ModelPending || receipt.Receipt.State != "received") {
				t.Errorf("expected durable model-off receipt: %+v", receipt)
			}
			results <- outcome{key, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var accepted, rejected string
	for r := range results {
		switch {
		case r.err == nil:
			accepted = r.key
		case errors.Is(r.err, ErrExplicitTaskQueueFull):
			rejected = r.key
		default:
			t.Fatal(r.err)
		}
	}
	if accepted == "" || rejected == "" {
		t.Fatalf("same-txn cap did not accept exactly one: accepted=%q rejected=%q", accepted, rejected)
	}
	blocked(rejected)
	duplicate, err := admit(accepted, input, 1)
	must(err)
	if !duplicate.Receipt.Duplicate {
		t.Fatalf("full queue did not replay existing receipt: %+v", duplicate)
	}
	if _, err := admit(accepted, input+" changed", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("fingerprint conflict masked by full queue: %v", err)
	}
	exec("UPDATE inbox_events SET state='applying' WHERE source='chat' AND event_id=?", accepted)
	blocked(rejected)
	exec("UPDATE inbox_events SET state='received' WHERE source='chat' AND event_id=?", accepted)

	// Model-off recovery must retain the original pending slot. The generic
	// conversation route remains independent even when the slot is occupied.
	must(s.Close())
	reopened, err := Open(path)
	must(err)
	s = reopened
	_, err = s.RecoverV3(ctx, now())
	must(err)
	blocked(rejected)
	_, err = s.AdmitChat(ctx, "generic-message", actor, "", "what is our status?", "", false, Delivery{}, nil, false)
	must(err)
	run, err := s.SubmitWithDelivery(ctx, accepted, input, "", "", true, Delivery{})
	must(err)
	blocked(rejected) // linking must not make the occupied slot disappear
	if r, err := s.claim(ctx); err != nil || r.ID != run.ID {
		t.Fatalf("failed to advance original accepted run: %+v %v", r, err)
	}
	blocked(rejected) // running work still contributes
	must(s.finish(run, "failed", "fixture terminal outcome", runAccounting{}))
	duplicate, err = admit(accepted, input, 1)
	must(err)
	if !duplicate.Receipt.Duplicate || duplicate.Receipt.RunID != run.ID {
		t.Fatalf("terminal replay changed request identity: %+v", duplicate)
	}
	if admitted, err := admit(rejected, input, 1); err != nil || admitted.Receipt.Duplicate {
		t.Fatalf("same rejected key must remain reusable when slot releases: %+v %v", admitted, err)
	}
}

func TestExplicitTaskAdmissionZeroAndMigrationDoNotRelabelLegacy(t *testing.T) {
	path := explicitTaskAdmissionDBFile(t)
	ctx := t.Context()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	const actor = "user:session"
	if _, err := s.AdmitChat(ctx, "old-receipt", actor, "", "old task", "", true, Delivery{}, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP INDEX inbox_explicit_task_waiting;
DROP INDEX inbox_explicit_task_run;
ALTER TABLE inbox_events DROP COLUMN explicit_task_admission;
PRAGMA user_version=122`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	admit := func(key, text string, limit int) (AdmissionResult, error) {
		return s.AdmitExplicitTask(ctx, key, actor, text, "", Delivery{}, nil, false, limit)
	}
	if r, err := admit("old-receipt", "old task", 1); err != nil || !r.Receipt.Duplicate {
		t.Fatalf("legacy explicit replay failed: %+v %v", r, err)
	}
	for _, key := range []string{"zero-a", "zero-b"} {
		if _, err := admit(key, "new measured lane", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admit("new-task", "limit enabled", 2); !errors.Is(err, ErrExplicitTaskQueueFull) {
		t.Fatalf("pre-enable marked work should count after limit is enabled: %v", err)
	}
	if _, err := admit("new-task", "limit enabled", 3); err != nil {
		t.Fatalf("unlabeled legacy receipt must not be counted as explicit: %v", err)
	}
	for _, invalid := range []int{-1, MaxExplicitTaskAdmissionLimit + 1} {
		if _, err := admit("invalid-limit", "preserve disabled definition", invalid); err == nil {
			t.Fatalf("accepted out-of-range limit %d", invalid)
		}
	}
}
