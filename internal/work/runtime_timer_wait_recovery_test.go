package work

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestTimerDeliveryRollsBackAndRepairs(t *testing.T) {
	s, taskID := setupTimersStore(t, "atomic.db")
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, key := range []string{"a", "b", "c"} {
		if _, err := openWait(tx, Wait{WaitID: key, TaskID: taskID, Kind: WaitKindExternal, OpenedAt: stamp, DeadlineAt: stamp}); err != nil {
			t.Fatal(err)
		}
		if _, err := createTimer(tx, Timer{TimerID: key, WaitID: key, WaitVersion: 1, DueAt: stamp, CreatedAt: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER injected_inbox_failure BEFORE INSERT ON inbox_events
WHEN NEW.source='timer' BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if _, err := scanDueTimers(s.db, stamp, 2, time.Second); err == nil {
		t.Fatal("expected fault")
	}
	count := func(q string, want int) {
		t.Helper()
		var got int
		if err := s.db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d want %d, err=%v", q, got, want, err)
		}
	}
	count("SELECT count(*) FROM timers WHERE state='pending' AND inbox_event_id=''", 3)
	count("SELECT count(*) FROM inbox_events WHERE source='timer'", 0)
	exec("DROP TRIGGER injected_inbox_failure")
	exec("UPDATE timers SET state='delivered' WHERE timer_id='a'") // Pre-V3.1 split commit.
	r, err := recoverDueTimers(s.db, stamp, 2)
	if err != nil || r.Delivered != 3 {
		t.Fatalf("delivery %+v, %v", r, err)
	}
	count("SELECT count(*) FROM timers WHERE state='delivered' AND inbox_event_id<>''", 3)
	count("SELECT count(*) FROM inbox_events WHERE source='timer' AND state='received'", 3)
	r, err = scanDueTimers(s.db, stamp, 2, time.Second)
	if err != nil || r.Delivered != 0 {
		t.Fatalf("duplicate delivery %+v, %v", r, err)
	}
}

func TestTimerBootSucceedsOnceAndDoesNotFailExternalJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	ctx := context.Background()
	run, err := s.Submit(ctx, "timer-restart", "等待外部作业结果", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, key := range []string{"cur", "stale", "closed"} {
		if _, err := openWait(tx, Wait{WaitID: key, TaskID: run.TaskID, Kind: WaitKindExternal, OpenedAt: stamp, DeadlineAt: stamp}); err != nil {
			t.Fatal(err)
		}
		if _, err := createTimer(tx, Timer{TimerID: key, WaitID: key, WaitVersion: 1, DueAt: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("UPDATE waits SET wait_version=2 WHERE wait_id='stale'"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWait(tx, "closed", 1, "verified-provider", "J17", "provider_done", "", "system", stamp); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = scanDueTimers(s.db, stamp, 2, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{3, 0} {
		r, err := s.RecoverRuntime(ctx, stamp)
		if err != nil || r.AppliedTimerEvents != want {
			t.Fatalf("boot %d: %+v, %v", i, r, err)
		}
	}
	var disposition, state string
	if err = s.db.QueryRow("SELECT state,resolution FROM waits WHERE wait_id='cur'").Scan(&state, &disposition); err != nil || state != "open" || disposition != "" {
		t.Fatalf("timeout resolved external wait: %s/%s, %v", state, disposition, err)
	}
	if err = s.db.QueryRow("SELECT kind FROM outbox_commands WHERE target_id='cur'").Scan(&disposition); err != nil || disposition != "wait_timeout_reconcile" {
		t.Fatalf("missing reconcile disposition: %s, %v", disposition, err)
	}
	if err = s.db.QueryRow("SELECT state FROM waits WHERE wait_id='stale'").Scan(&state); err != nil || state != "open" {
		t.Fatalf("new wait closed: %s, %v", state, err)
	}
	if err = s.db.QueryRow("SELECT resolution FROM waits WHERE wait_id='closed'").Scan(&disposition); err != nil || disposition != "provider_done" {
		t.Fatalf("overwrote receipt: %s, %v", disposition, err)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM outbox_commands WHERE backend='runtime-wait'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("follow-up count=%d, %v", n, err)
	}
}

func TestTimerInboxRollbackIncludesWaitAndFollowUp(t *testing.T) {
	s, taskID := setupTimersStore(t, "wait-rollback.db")
	stamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := openWait(tx, Wait{WaitID: "atomic", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: stamp}); err != nil {
		t.Fatal(err)
	}
	if _, err := createTimer(tx, Timer{TimerID: "atomic", WaitID: "atomic", WaitVersion: 1, DueAt: stamp}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := scanDueTimers(s.db, stamp, 2, time.Second); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER follow_up_failure BEFORE INSERT ON outbox_commands
WHEN NEW.backend='runtime-wait' BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if _, err := s.consumeTimerInbox(context.Background(), stamp, 2); err == nil {
		t.Fatal("missing injected error")
	}
	var wait, inbox string
	if err := s.db.QueryRow(`SELECT w.state,e.state FROM waits w JOIN inbox_events e ON e.wait_id=w.wait_id
WHERE w.wait_id='atomic'`).Scan(&wait, &inbox); err != nil || wait != "open" || inbox != "received" {
		t.Fatalf("%s/%s %v", wait, inbox, err)
	}
	exec("DROP TRIGGER follow_up_failure")
	if n, err := s.consumeTimerInbox(context.Background(), stamp, 2); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestBusinessTimeoutNeverSuppliesEvidenceOrDecision(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   WaitKind
		policy string
		want   string
	}{
		{"external", WaitKindExternal, "fail", "wait_timeout_reconcile"},
		{"approval", WaitKindAcceptance, "fail", "wait_timeout_notify"},
		{"publication", WaitKindPublication, "reconcile", "wait_timeout_notify"},
		{"dependency", WaitKindDependency, "fail", "wait_timeout_fail"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, task := setupTimersStore(t, tt.name+".sqlite")
			ctx, stamp := t.Context(), now()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := openWait(tx, Wait{WaitID: "business", TaskID: task, Kind: tt.kind, OpenedAt: stamp,
				DeadlineAt: stamp, TimeoutPolicy: tt.policy}); err != nil {
				t.Fatal(err)
			}
			if _, err := createTimer(tx, Timer{TimerID: "due", WaitID: "business", WaitVersion: 1, DueAt: stamp}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := recoverDueTimers(s.db, stamp, 10); err != nil {
				t.Fatal(err)
			}
			if n, err := s.consumeTimerInbox(ctx, stamp, 10); err != nil || n != 1 {
				t.Fatalf("applied=%d %v", n, err)
			}
			if n, err := s.consumeTimerInbox(ctx, stamp, 10); err != nil || n != 0 {
				t.Fatalf("reapplied=%d %v", n, err)
			}
			if n, err := s.consumeTimerWaitFollowups(ctx, stamp, 10); err != nil || n != 0 {
				t.Fatalf("plain wake unexpectedly applied=%d %v", n, err)
			}
			var kind, state, resolution string
			var closed int
			err = s.db.QueryRow(`SELECT c.kind,w.state,w.resolution,e.closed_wait
FROM outbox_commands c JOIN waits w ON w.wait_id=c.target_id
JOIN wait_events e ON e.wait_id=w.wait_id AND e.source='timer'
WHERE c.command_id='timer:due:follow-up'`).Scan(&kind, &state, &resolution, &closed)
			if err != nil || kind != tt.want || state != "open" || resolution != "" || closed != 0 {
				t.Fatalf("timer supplied missing result: %s %s %s %d, %v", kind, state, resolution, closed, err)
			}
			// The regular caller can still record a genuine result on this same
			// version. No timer is allowed to usurp the result's close.
			tx, err = s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			r, err := resolveWait(tx, "business", 1, "verified-result", "late", "resolved", "evidence-ref", "system", stamp+1)
			if err != nil || !r.Closed {
				t.Fatalf("late genuine result rejected: %+v %v", r, err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
