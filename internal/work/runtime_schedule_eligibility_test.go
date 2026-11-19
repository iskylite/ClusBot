package work

import "testing"

func TestV31ScheduleNoCatchupAndExhaustedDoesNotStarve(t *testing.T) {
	s := setupV3P109(t, "skip.db")
	stamp := now()
	old := v3P109Schedule("old-skip", 60_000, stamp-120_000, MissedSkip)
	exhausted := v3P109Schedule("finished-once", 60_000, 0, MissedCoalesce)
	exhausted.Kind = "once"
	exhausted.OnceAt = stamp - 120_000
	due := v3P109Schedule("due", 60_000, stamp-1, MissedCoalesce)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, sc := range []Schedule{old, exhausted, due} {
		if _, err := upsertSchedule(tx, sc); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := scanDueSchedules(s.db, stamp, 1)
	if err != nil || got.Skipped != 1 || got.Triggered != 0 {
		t.Fatalf("overdue %+v %v", got, err)
	}
	var result string
	var unassigned bool
	if err = s.db.QueryRow(`SELECT outcome,task_id IS NULL FROM schedule_occurrences WHERE schedule_id='old-skip'`).Scan(&result, &unassigned); err != nil || result != "skipped" || !unassigned {
		t.Fatalf("%s %v %v", result, unassigned, err)
	}
	got, err = scanDueSchedules(s.db, stamp, 1)
	if err != nil || got.Triggered != 1 {
		t.Fatalf("next plan starved %+v %v", got, err)
	}
	got, err = scanDueSchedules(s.db, stamp, 1)
	if err != nil || got != (ScanSchedulesResult{}) {
		t.Fatalf("replay %+v %v", got, err)
	}
}
