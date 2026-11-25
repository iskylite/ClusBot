package work

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func submitPriorityFixture(t *testing.T, s *Store, key, priority string) Run {
	t.Helper()
	r, err := s.Submit(t.Context(), key, key, "", "", priority != "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if r.TaskID != "" {
		if _, err := s.db.Exec("UPDATE tasks SET priority=? WHERE id=?", priority, r.TaskID); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestModelRunPriorityFairnessBoundsContinuousArrivals(t *testing.T) {
	s, err := Open(explicitTaskAdmissionDBFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	s.configureRuntime(2, RunLimits{ModelRunPriorityFairness: true})
	normalFirst := submitPriorityFixture(t, s, "normal-first", "normal")
	scheduled := submitPriorityFixture(t, s, "normal-scheduled", "normal")
	chat := submitPriorityFixture(t, s, "ordinary-chat", "conversation")
	normalLast := submitPriorityFixture(t, s, "normal-last", "normal")
	lowFirst := submitPriorityFixture(t, s, "low-first", "low")
	lowSecond := submitPriorityFixture(t, s, "low-second", "low")
	high := submitPriorityFixture(t, s, "high-0", "high")
	highArrival := 0
	for _, normals := range [][3]Run{{normalFirst, scheduled, lowFirst}, {chat, normalLast, lowSecond}} {
		for _, slot := range modelRunPrioritySlots {
			var expected Run
			switch slot {
			case 0:
				expected = high
			case 1:
				expected = normals[0]
				normals[0] = normals[1]
			case 2:
				expected = normals[2]
			}
			got, err := s.claim(ctx)
			if err != nil || got.ID != expected.ID {
				t.Fatalf("wrong lane/FIFO choice at slot %d: %+v want %s, %v", slot, got, expected.ID, err)
			}
			if err := s.finish(got, "failed", "fixture terminal outcome", runAccounting{}); err != nil {
				t.Fatal(err)
			}
			if slot == 0 {
				highArrival++
				high = submitPriorityFixture(t, s, fmt.Sprintf("high-%d", highArrival), "high")
			}
		}
	}
}

func TestModelRunPriorityFairnessKeepsHardDependencyAndUserOrder(t *testing.T) {
	s, err := Open(explicitTaskAdmissionDBFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	s.configureRuntime(2, RunLimits{ModelRunPriorityFairness: true})
	graph, err := s.CreateWorkflow(ctx, "priority-order-workflow", WorkflowRequest{
		Goal: "close the verified report", Criteria: "one summary", Scope: "workflow",
		Steps: []WorkflowStep{
			{Key: "prereq", Goal: "collect", Criteria: "collected"},
			{Key: "blocked", Goal: "review", Criteria: "reviewed", Depends: []string{"prereq"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var prereq string
	for _, child := range graph.Children {
		priority := "high"
		if child.Key == "prereq" {
			prereq, priority = child.TaskID, "low"
		}
		if _, err := s.db.Exec("UPDATE tasks SET priority=? WHERE id=?", priority, child.TaskID); err != nil {
			t.Fatal(err)
		}
	}
	after := submitPriorityFixture(t, s, "relative-after", "high")
	before := submitPriorityFixture(t, s, "relative-before", "low")
	if _, err := s.db.Exec("INSERT INTO run_queue_order(before_run_id,after_run_id) VALUES(?,?)", before.ID, after.ID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{prereq, before.TaskID, after.TaskID} {
		got, err := s.claim(ctx)
		if err != nil || got.TaskID != want {
			t.Fatalf("hard eligible order violated: %+v want task %s, %v", got, want, err)
		}
	}
	if got, err := s.claim(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty eligible classes must not claim the blocked workflow child or aggregate: %+v %v", got, err)
	}
}

func TestModelRunPriorityCursorPreservedOnRestartAndRollback(t *testing.T) {
	path := explicitTaskAdmissionDBFile(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	load := func(want int) {
		t.Helper()
		var got int
		must(s.db.QueryRow("SELECT next_slot FROM model_run_priority_cursor WHERE singleton=1").Scan(&got))
		if got != want {
			t.Fatalf("cursor %d want %d", got, want)
		}
	}
	claim := func(want Run) Run {
		t.Helper()
		r, err := s.claim(ctx)
		if err != nil || r.ID != want.ID {
			t.Fatalf("claim: %+v want %s, %v", r, want.ID, err)
		}
		return r
	}
	low1 := submitPriorityFixture(t, s, "strict-low", "low")
	high1 := submitPriorityFixture(t, s, "strict-high", "high")
	r := claim(high1) // default must preserve strict priority and not consume turns
	must(s.finish(r, "failed", "fixture terminal outcome", runAccounting{}))
	load(0)
	r = claim(low1)
	must(s.finish(r, "failed", "fixture terminal outcome", runAccounting{}))
	load(0)
	s.configureRuntime(1, RunLimits{ModelRunPriorityFairness: true})
	for _, key := range []string{"normal-a", "normal-b"} {
		r := claim(submitPriorityFixture(t, s, key, "normal"))
		must(s.finish(r, "failed", "fixture terminal outcome", runAccounting{}))
	}
	load(6) // Empty H was skipped; two normal slots really advanced.
	if _, err := s.claim(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty queue: %v", err)
	}
	load(6)
	high := submitPriorityFixture(t, s, "restart-high", "high")
	low := submitPriorityFixture(t, s, "restart-low", "low")
	must(s.Close())
	reopened, err := Open(path)
	must(err)
	s = reopened
	_, err = s.RecoverRuntime(ctx, now())
	must(err)
	s.configureRuntime(1, RunLimits{ModelRunPriorityFairness: true})
	load(6)
	// Fail the final event after the cursor, Run status, and bindings would
	// otherwise change. The whole transaction must roll back.
	_, err = s.db.Exec(`CREATE TEMP TRIGGER fixture_claim_fail BEFORE INSERT ON run_events
	 WHEN NEW.kind='run.state_changed' BEGIN SELECT RAISE(ABORT,'fixture claim rollback'); END`)
	must(err)
	if _, err := s.claim(ctx); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim rollback fixture missing: %v", err)
	}
	load(6)
	var queued string
	must(s.db.QueryRow("SELECT status FROM runs WHERE id=?", low.ID).Scan(&queued))
	if queued != "queued" {
		t.Fatalf("failed claim changed run status: %s", queued)
	}
	_, err = s.db.Exec("DROP TRIGGER fixture_claim_fail")
	must(err)
	claim(low)
	load(0)
	claim(high)
	load(1)
}
func TestModelRunClaimSkipsUnavailableRolesIncludingDefaultlessIsolation(t *testing.T) {
	ctx := t.Context()
	db, err := Open(filepath.Join(t.TempDir(), "role-claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	worker, err := db.Submit(ctx, "worker-first", "安排任务", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := db.Submit(ctx, "reply-after-worker", "解释问题", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := db.Submit(ctx, "pinned-only-after-worker", "仅本条", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO isolated_reply_pins VALUES(?,?,?,'profile',1,'credential','only_this_message','not dispatched fixture')`,
		isolated.ID, isolated.ConversationID, "user:fixture"); err != nil {
		t.Fatal(err)
	}
	db.configureRuntime(1, RunLimits{ModelRunPriorityFairness: true})
	assertClaim := func(reply, work bool, expected Run) {
		t.Helper()
		claim, err := db.claimAvailable(ctx, reply, work)
		if err != nil || claim.ID != expected.ID {
			t.Fatalf("selected %+v, want ID=%s: %v", claim, expected.ID, err)
		}
	}
	assertClaim(false, false, isolated)
	assertClaim(true, false, ordinary)
	if got, err := db.claimAvailable(ctx, true, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unavailable Worker was claimed: %+v %v", got, err)
	}
	assertClaim(false, true, worker)
}
