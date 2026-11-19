package work

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestBenchmarkWaitTimeoutQueriesOriginalJob(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-timeout.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", Allowed: map[string][]string{"gpu_count": {"40"}}, ExpiresAt: now() + 120000}
	if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	const actor = "user:session"
	binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	spec := BenchmarkCase{Key: "one", Binding: binding, Targets: []string{"node-1"}, Parameters: map[string]string{"gpu_count": "40"},
		ActualConfig: map[string]string{"gpu_count": "40"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
	if err = s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "profile", Version: 1, Cases: []BenchmarkCase{spec}}); err != nil {
		t.Fatal(err)
	}
	p := &benchFixtureProvider{loseReply: true}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"timeout", "profile", 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := s.runBenchmarkOnce(ctx, p); err != nil || !ran {
		t.Fatalf("run initial: %t %v", ran, err)
	}
	stamp, future := now()-1000, now()+30000
	var due int64
	var waitID string
	if err = s.db.QueryRow(`SELECT t.due_at,w.wait_id FROM timers t JOIN waits w ON w.wait_id=t.wait_id WHERE w.task_id=?`, task).Scan(&due, &waitID); err != nil || due <= stamp {
		t.Fatalf("missing production recheck timer: %d, %v", due, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE benchmark_cases SET call_until=?,next_at=? WHERE task_id=?`, stamp+100, future, task)
	// A structurally valid command alone cannot schedule a provider query.
	exec(`INSERT INTO outbox_commands(command_id,kind,target_kind,target_id,target_version,task_id,payload_digest,backend,created_at)
VALUES('timer:forged:follow-up','wait_timeout_reconcile','wait',?,1,?,?,'runtime-wait',?)`,
		waitID, task, digest(fmt.Sprintf("%q:1:wait_timeout_reconcile", waitID)), stamp)
	if err := s.applyBenchmarkTimeouts(ctx, stamp); err != nil {
		t.Fatal(err)
	}
	var forged string
	if err := s.db.QueryRow("SELECT state FROM outbox_commands WHERE command_id='timer:forged:follow-up'").Scan(&forged); err != nil || forged != "dead" {
		t.Fatalf("unauthenticated timeout accepted: %s %v", forged, err)
	}

	exec(`UPDATE timers SET due_at=? WHERE state='pending'`, stamp)
	if _, err = scanDueTimers(s.db, stamp, 5, time.Second); err != nil {
		t.Fatal(err)
	}
	if n, err := s.consumeTimerInbox(ctx, stamp, 5); err != nil || n != 1 {
		t.Fatalf("timer: %d %v", n, err)
	}
	if err = s.applyBenchmarkTimeouts(ctx, stamp); err != nil {
		t.Fatal(err)
	}
	var next int64
	var command, wait string
	read := func(wantNext int64, wantCommand, wantWait string) {
		t.Helper()
		err := s.db.QueryRow(`SELECT b.next_at,c.state,w.state FROM benchmark_cases b JOIN outbox_commands c ON c.task_id=b.task_id
JOIN waits w ON w.wait_id=c.target_id WHERE b.task_id=? AND c.kind='wait_timeout_reconcile' AND c.command_id<>'timer:forged:follow-up'`, task).Scan(&next, &command, &wait)
		if err != nil || next != wantNext || command != wantCommand || wait != wantWait {
			t.Fatalf("got %d/%s/%s want %d/%s/%s: %v", next, command, wait, wantNext, wantCommand, wantWait, err)
		}
	}
	read(future, "pending", "open") // live worker lease was not pierced
	if err = s.applyBenchmarkTimeouts(ctx, stamp+100); err != nil {
		t.Fatal(err)
	}
	read(stamp+100, "acknowledged", "open")
	if err = s.applyBenchmarkTimeouts(ctx, stamp+200); err != nil {
		t.Fatal(err)
	}
	read(stamp+100, "acknowledged", "open") // no replay
	var attentionID string
	for _, subject := range []string{actor, "another:actor"} {
		page, err := s.ListAttention(ctx, subject, stamp+200, 5, AttentionCursor{})
		if err != nil {
			t.Fatal(err)
		}
		if subject == actor {
			if len(page.Items) != 1 || page.Items[0].Kind != "benchmark_wait" || page.Items[0].State != "result_pending" || page.Items[0].TaskID != task {
				t.Fatalf("benchmark timeout projection: %+v", page)
			}
			attentionID = page.Items[0].ID
			detail, err := s.ReadAttention(ctx, actor, attentionID, stamp+200)
			if err != nil || detail.State != "result_pending" || detail.Source.Kind != "verified_timer_receipt" || detail.Source.WaitVersion != 1 {
				t.Fatalf("benchmark timeout detail: %+v %v", detail, err)
			}
		} else if len(page.Items) != 0 {
			t.Fatalf("foreign benchmark appeared in attention: %+v", page)
		}
	}
	if p.started != 1 || p.looked != 0 || p.read != 0 {
		t.Fatalf("scheduling directly called provider: %+v", p)
	}
	if ran, err := s.runBenchmarkOnce(ctx, p); err != nil || !ran {
		t.Fatalf("lookup original: %t %v", ran, err)
	}
	if p.started != 1 || p.looked != 1 || p.read != 0 {
		t.Fatalf("expected only original lookup, got %+v", p)
	}
	if ran, err := s.runBenchmarkOnce(ctx, p); err != nil || !ran {
		t.Fatalf("confirm original result: %t %v", ran, err)
	}
	if p.started != 1 || p.looked != 1 || p.read != 1 {
		t.Fatalf("unexpected provider calls: %+v", p)
	}
	if err := s.db.QueryRow("SELECT state FROM waits WHERE task_id=?", task).Scan(&wait); err != nil || wait != "closed" {
		t.Fatalf("provider result did not resolve wait: %s %v", wait, err)
	}
	page, err := s.ListAttention(ctx, actor, now(), 5, AttentionCursor{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("resolved wait still appears as pending: %+v %v", page, err)
	}
	detail, err := s.ReadAttention(ctx, actor, attentionID, now())
	if err != nil || detail.State != "result_recorded" || detail.Source.OperationState != "succeeded" {
		t.Fatalf("closed item did not refresh in place: %+v %v", detail, err)
	}
	if _, err := s.ReadAttention(ctx, "another:actor", attentionID, now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign timeout detail revealed: %v", err)
	}
}
