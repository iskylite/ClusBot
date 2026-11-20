package work

import (
	"errors"
	"testing"
)

func TestExplicitTaskQueueSnapshotScopesDistinctActiveStagesAndDoesNotWrite(t *testing.T) {
	s, err := Open(explicitTaskAdmissionDBFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const actor, other = "user:session", "user:other"
	read := func(principal string) ExplicitTaskQueueSnapshot {
		t.Helper()
		value, err := s.ReadExplicitTaskQueue(ctx, principal)
		must(err)
		return value
	}
	if r := read(actor); r.PendingReceipts+r.LinkedQueued+r.LinkedRunning != 0 || r.OldestReceiptAgeMS != nil || r.OldestQueuedAgeMS != nil {
		t.Fatalf("empty snapshot should preserve null ages: %+v", r)
	}
	if _, err := s.ReadExplicitTaskQueue(ctx, ""); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("missing principal: %v", err)
	}
	create := func(who, key string, link bool) Run {
		t.Helper()
		_, err := s.AdmitExplicitTask(ctx, key, who, key, "", Delivery{}, nil, false, 0)
		must(err)
		if !link {
			return Run{}
		}
		run, err := s.Submit(ctx, key, key, "", "", true)
		must(err)
		return run
	}
	running := create(actor, "own-running", true)
	r, err := s.claim(ctx)
	must(err)
	if r.ID != running.ID {
		t.Fatalf("claimed the wrong run: %s", r.ID)
	}
	ownQueued := create(actor, "own-queued", true)
	create(actor, "own-pending", false)
	create(actor, "own-failed-receipt", false)
	must(s.FailChatAdmission(ctx, "own-failed-receipt", "failed fixture"))
	create(actor, "own-duplicate-link", false)
	terminal := create(actor, "own-terminal", true)
	detail, err := s.Detail(terminal.TaskID, true)
	must(err)
	must(s.Control(ctx, terminal.TaskID, "stop", detail.Task.Revision))
	create(other, "foreign-queued", true)
	create(other, "foreign-pending", false)
	_, err = s.AdmitChat(ctx, "legacy-untagged", actor, "", "not explicitly marked", "", true, Delivery{}, nil, false)
	must(err)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := s.db.Exec(q, args...)
		must(err)
	}
	exec("UPDATE inbox_events SET received_at=?,state='applying' WHERE source='chat' AND event_id='own-pending'", now()+600000)
	exec("UPDATE inbox_events SET received_at=? WHERE source='chat' AND event_id='foreign-pending'", now()-70000)
	exec("UPDATE inbox_events SET state='applied',run_id=? WHERE source='chat' AND event_id='own-duplicate-link'", ownQueued.ID)
	exec("UPDATE runs SET created_at=? WHERE id=?", now()-1234, ownQueued.ID)
	changes := func() int {
		t.Helper()
		var n int
		must(s.db.QueryRow("SELECT total_changes()").Scan(&n))
		return n
	}
	before := changes()
	if o := read(actor); o.PendingReceipts != 1 || o.LinkedQueued != 1 || o.LinkedRunning != 1 ||
		o.OldestReceiptAgeMS == nil || *o.OldestReceiptAgeMS != 0 ||
		o.OldestQueuedAgeMS == nil || *o.OldestQueuedAgeMS < 1234 {
		t.Fatalf("unexpected own counts/ages with stale, duplicate and foreign records: %+v", o)
	}
	if o := read(other); o.PendingReceipts != 1 || o.LinkedQueued != 1 || o.LinkedRunning != 0 ||
		o.OldestReceiptAgeMS == nil || *o.OldestReceiptAgeMS < 70000 {
		t.Fatalf("unexpected other principal counts/ages: %+v", o)
	}
	if after := changes(); after != before {
		t.Fatalf("snapshot mutated the writer connection: before=%d after=%d", before, after)
	}
}
