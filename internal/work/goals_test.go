package work

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestGoalScheduleIsDurableAndStopsOwnedWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	stamp := now()
	interval := time.Hour.Milliseconds()
	spec := Goal{Target: "整理例行工作清单", Criteria: "列出优先级", Interval: interval,
		NextAt: stamp, ExpiresAt: stamp + 5*interval, Remaining: 3}
	goal, err := s.CreateGoal(t.Context(), "schedule-v2", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleGoals(t.Context(), stamp); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	// The same occurrence is already durable, and an active occurrence prevents overlap.
	if err = s.scheduleGoals(t.Context(), stamp+2*interval); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM goal_tasks WHERE goal_id=?", goal.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("occurrences=%d, %v", count, err)
	}
	retry, err := s.CreateGoal(t.Context(), "schedule-v2", spec)
	if err != nil || retry.ID != goal.ID || retry.Remaining != 2 || retry.Revision != 2 {
		t.Fatalf("deduplication: %+v, %v", retry, err)
	}
	if err = s.StopGoal(t.Context(), goal.ID, retry.Revision); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err = s.db.QueryRow("SELECT task_id FROM goal_tasks WHERE goal_id=?", goal.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(taskID, true)
	if err != nil || d.Task.Status != "cancelled" || d.Run.Status != "cancelled" {
		t.Fatalf("owned task after stop: %+v, %v", d, err)
	}
	if _, err = s.Resume(t.Context(), taskID, "resume-stopped-goal", d.Task.Revision); !errors.Is(err, ErrState) {
		t.Fatalf("stopped goal resumed: %v", err)
	}
	if err = s.scheduleGoals(t.Context(), stamp+3*interval); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM goal_tasks WHERE goal_id=?", goal.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stopped goal generated another occurrence: %d, %v", count, err)
	}
}
