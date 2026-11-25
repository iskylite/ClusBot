package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestReadyClaimFiltersBeforeLimit(t *testing.T) {
	s, taskID, unitID := setupLeasesStore(t, "ready-filter")
	stamp := recordsTestNow()
	workerID, cred := registerLeaseWorker(t, s, "scoped", []WorkUnitKind{WorkUnitModel}, "POD7")
	req := ClaimRequest{WorkerID: workerID, Credential: cred, Kind: WorkUnitModel, Scope: "POD7", LeaseTTL: 5000, QuotaAvailable: true}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE tasks SET scope_label='POD7',revision=revision+1 WHERE id=?", taskID)
	// The first draft was created under the old contract. Editing even only
	// the scope changes the contract, not just the presentation.
	var stepID string
	if err := s.db.QueryRow("SELECT step_id FROM work_units WHERE unit_id=?", unitID).Scan(&stepID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		otherRun, err := s.Submit(ctx, "ready-filter-other-"+id(), "other task", "other scope", "", true)
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO work_units(unit_id,task_id,kind,ready_at,state,created_at,updated_at) VALUES(?,?,'model',?,'ready',?,?)",
			id(), otherRun.TaskID, stamp-1, stamp, stamp)
	}
	claim := func(at int64) (ClaimResult, error) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		got, err := claimWorkUnit(tx, req, at)
		if err != nil {
			return got, err
		}
		return got, tx.Commit()
	}
	if _, err := claim(stamp); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("obsolete or foreign-scope work was claimable: %v", err)
	}
	exec("UPDATE steps SET contract_revision=2 WHERE step_id=?", stepID)
	exec("UPDATE work_units SET ready_at=? WHERE unit_id=?", stamp+1, unitID)
	if _, err := claim(stamp); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claimed before eligible time: %v", err)
	}
	// A storage-only revision bump must not invalidate the frozen contract.
	exec("UPDATE tasks SET revision=revision+1 WHERE id=?", taskID)
	got, err := claim(stamp + 1)
	if err != nil || got.Unit.UnitID != unitID {
		t.Fatalf("eligible unit lost behind ineligible rows: %v, %v", got.Unit, err)
	}
}

func TestReadyRecoveryGuardsAndKeepsExistingWork(t *testing.T) {
	s, taskID := setupRecoveryStore(t, "ready-recovery")
	ctx := context.Background()
	stamp := recordsTestNow()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	addStep := func(key, task, state string, rev, evidence int) {
		t.Helper()
		exec("INSERT INTO steps(step_id,task_id,contract_revision,definition_key,definition_version,state,evidence_revision,created_at,updated_at) VALUES(?,?,?,?,1,?,?,?,?)",
			key, task, rev, key, state, evidence, stamp, stamp)
	}
	addStep("target", taskID, "blocked", 1, 0)
	addStep("upstream", taskID, "completed", 1, 2)
	addStep("old-contract", taskID, "ready", 2, 0) // not yet a published contract
	exec("INSERT INTO step_dependencies(step_id,depends_on_step_id,evidence_revision) VALUES('target','upstream',1)")
	check := func(want int) {
		t.Helper()
		got, err := s.computeAllReadyUnits(ctx)
		if err != nil || got != want {
			t.Fatalf("materialized %d, want %d: %v", got, want, err)
		}
	}
	check(0) // wrong edge evidence and wrong contract
	exec("UPDATE steps SET evidence_revision=1 WHERE step_id='upstream'")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openWait(tx, Wait{WaitID: "w", TaskID: taskID, StepID: "target", Kind: WaitKindTimer, OpenedAt: stamp}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	check(0)
	exec("UPDATE waits SET state='closed' WHERE wait_id='w'")
	exec("INSERT INTO task_states(task_id,state,storage_revision,updated_at) VALUES(?,'paused',1,?)", taskID, stamp)
	check(0)
	exec("UPDATE task_states SET state='pending' WHERE task_id=?", taskID)
	check(1)
	check(0)
	exec("UPDATE work_units SET state='cancelled' WHERE step_id='target'")
	check(0) // recovery must not silently recreate a cancelled execution
}

func TestReadyIndexMigration(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "v90.sqlite")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, "DROP INDEX work_units_by_step; PRAGMA user_version=90")
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='index' AND name='work_units_by_step')").Scan(&exists); err != nil || !exists {
		t.Fatalf("missing work_units_by_step on migrated database: %v", err)
	}
}

func TestReadyRecoveryRollsBackWholeBatch(t *testing.T) {
	s, taskID := setupRecoveryStore(t, "ready-rollback")
	ctx := context.Background()
	add := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, stepID := range []string{"a", "b"} {
		add("INSERT INTO steps(step_id,task_id,contract_revision,definition_key,definition_version,state,created_at,updated_at) VALUES(?,?,1,?,1,'ready',1,1)",
			stepID, taskID, stepID)
	}
	add(`CREATE TRIGGER test_reject_unit BEFORE INSERT ON work_units
WHEN NEW.step_id='b' BEGIN SELECT RAISE(ABORT,'test write failed'); END`)
	if got, err := s.computeAllReadyUnits(ctx); err == nil || got != 0 {
		t.Fatalf("failed batch reported %d, %v", got, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM work_units WHERE task_id=?", taskID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial batch persisted: %d, %v", count, err)
	}
	add("DROP TRIGGER test_reject_unit")
	if got, err := s.computeAllReadyUnits(ctx); err != nil || got != 2 {
		t.Fatalf("retry materialized %d, %v", got, err)
	}
}

func TestReadyActionWaitsForReconciliation(t *testing.T) {
	s, task, unit := setupLeasesStore(t, "reconcile-claim")
	stamp := recordsTestNow()
	add := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	add("UPDATE work_units SET kind='action' WHERE unit_id=?", unit)
	add("INSERT INTO task_states(task_id,state,storage_revision,reconcile_required,updated_at) VALUES(?,'pending',1,1,?)", task, stamp)
	check := func(want int) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		got, err := selectReadyUnits(tx, WorkUnitAction, "", stamp, 1)
		if err != nil || len(got) != want {
			t.Fatalf("action eligibility = %v, %v; want %d", got, err, want)
		}
	}
	check(0)
	add("UPDATE task_states SET reconcile_required=0 WHERE task_id=?", task)
	add(`INSERT INTO operations(operation_id,task_id,capability_key,target_refs,params_digest,idempotency_key,state,registered_at,updated_at)
VALUES('o',?,'test','[]','digest','key','reconcile',?,?)`, task, stamp, stamp)
	check(0)
	add("UPDATE operations SET state='succeeded' WHERE operation_id='o'")
	check(1)
}
