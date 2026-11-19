package work

import "testing"

func TestTimerWaitFollowupAtomicAndRestartable(t *testing.T) {
	for _, scenario := range []string{"normal", "replaced", "superseded_step", "reconcile", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			s, task := setupV3P108(t, scenario+".sqlite")
			ctx, stamp := t.Context(), now()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := transitionTaskState(tx, task, EventCreate, TaskGuard{Actor: "system"}, stamp); err != nil {
				t.Fatal(err)
			}
			cur, err := readTaskState(tx, task)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = transitionTaskState(tx, task, EventNoRunnableStep, TaskGuard{Actor: "system", ExpectedStorageRevision: cur.StorageRevision}, stamp); err != nil {
				t.Fatal(err)
			}
			step := "step-" + task
			if err = insertStep(tx, Step{StepID: step, TaskID: task, ContractRevision: 1,
				DefinitionKey: "timer-deferred-step", DefinitionVersion: 1, State: V3StepWaiting, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
				t.Fatal(err)
			}
			wait := Wait{WaitID: "wait-" + task, TaskID: task, StepID: step, Kind: WaitKindTimer, OpenedAt: stamp}
			if _, err = openWait(tx, wait); err != nil {
				t.Fatal(err)
			}
			if _, err = createTimer(tx, Timer{TimerID: "t1", WaitID: wait.WaitID, WaitVersion: 1, DueAt: stamp}); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := s.db.Exec(sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := recoverDueTimers(s.db, stamp, 50); err != nil {
				t.Fatal(err)
			}
			if n, err := s.consumeTimerInbox(ctx, stamp, 50); err != nil || n != 1 {
				t.Fatalf("timer consumption: %d %v", n, err)
			}

			switch scenario {
			case "normal":
				exec(`CREATE TRIGGER simulate_lost_commit BEFORE INSERT ON outbox_receipts BEGIN SELECT RAISE(ABORT,'injected'); END`)
				if _, err := s.consumeTimerWaitFollowups(ctx, stamp, 50); err == nil {
					t.Fatal("expected transaction failure")
				}
				exec("DROP TRIGGER simulate_lost_commit")
				got, err := readTaskState(s.db, task)
				if err != nil || got.State != V3TaskWaiting {
					t.Fatalf("failed receipt wrote task state: %+v %v", got, err)
				}
			case "replaced":
				exec("UPDATE waits SET state='open',wait_version=2 WHERE wait_id=?", wait.WaitID)
			case "superseded_step":
				exec("UPDATE steps SET contract_revision=2 WHERE step_id=?", step)
			case "reconcile":
				exec("UPDATE task_states SET reconcile_required=1 WHERE task_id=?", task)
			case "paused":
				exec("UPDATE task_states SET state='paused' WHERE task_id=?", task)
			}
			wantState, wantCount := V3TaskWaiting, 0
			if scenario == "replaced" || scenario == "superseded_step" {
				wantCount = 1
			}
			if scenario == "paused" {
				wantState = V3TaskPaused
			}
			if scenario == "normal" {
				wantState, wantCount = V3TaskPending, 1
			}
			if n, err := s.consumeTimerWaitFollowups(ctx, stamp, 50); err != nil || n != wantCount {
				t.Fatalf("follow-up: %d/%d %v", n, wantCount, err)
			}
			stateIs := func(want V3TaskState) {
				t.Helper()
				got, err := readTaskState(s.db, task)
				if err != nil || got.State != want {
					t.Fatalf("state: %s/%s %v", got.State, want, err)
				}
			}
			stateIs(wantState)
			var units int
			if err := s.db.QueryRow("SELECT count(*) FROM work_units WHERE step_id=?", step).Scan(&units); err != nil || units != 0 {
				t.Fatalf("follow-up directly created work: %d, %v", units, err)
			}
			if scenario == "reconcile" || scenario == "paused" {
				exec("UPDATE task_states SET reconcile_required=0,state='waiting' WHERE task_id=?", task)
				if _, err := s.RecoverV3(ctx, stamp+5000); err != nil {
					t.Fatal(err)
				}
				stateIs(V3TaskPending)
			}
			if n, err := s.consumeTimerWaitFollowups(ctx, stamp+5000, 50); err != nil || n != 0 {
				t.Fatalf("duplicate wake: %d %v", n, err)
			}
			if scenario != "replaced" && scenario != "superseded_step" {
				if _, err := s.computeAllReadyUnits(ctx); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRow("SELECT count(*) FROM work_units WHERE step_id=?", step).Scan(&units); err != nil || units != 1 {
					t.Fatalf("eligibility not recomputed: %d %v", units, err)
				}
			}
		})
	}
}
