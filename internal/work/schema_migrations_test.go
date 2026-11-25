package work

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSchemaMigrationsV133ToV135AndUnwiredIndicators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v133.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS v3_skills (skill_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS skill_replays (replay_id TEXT PRIMARY KEY, skill_id TEXT REFERENCES v3_skills(skill_id));
CREATE TABLE IF NOT EXISTS scheduler_groups (group_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS goals (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS goal_tasks (goal_id TEXT NOT NULL REFERENCES goals(id), scheduled_at INTEGER NOT NULL, task_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS schedules (schedule_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS schedule_occurrences (occurrence_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS v3_context_manifests (manifest_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS workflow_templates (template_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS workflow_bindings (binding_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS workflow_template_changes (change_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS asset_admission_decisions (decision_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS cleanup_progress (progress_id INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS domain_process_refs (package_id TEXT NOT NULL, process_key TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS self_heal_counters (policy_id TEXT NOT NULL);
PRAGMA user_version = 133;
`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v133 database: %v", err)
	}
	defer upgraded.Close()

	var version int
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion {
		t.Fatalf("version = %d, want %d: %v", version, databaseSchemaVersion, err)
	}
	for _, dropped := range []string{
		"v3_skills", "skill_replays", "scheduler_groups",
		"goals", "goal_tasks", "schedules", "schedule_occurrences",
		"v3_context_manifests", "workflow_templates", "workflow_bindings",
		"workflow_template_changes", "asset_admission_decisions",
		"cleanup_progress", "domain_process_refs", "self_heal_counters",
	} {
		var exists bool
		if err := upgraded.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", dropped).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table %s was not dropped", dropped)
		}
	}
	for _, created := range []string{"skill_evaluation_suites", "skill_evaluation_runs"} {
		var exists bool
		if err := upgraded.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", created).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("new RSI table %s missing", created)
		}
	}
	var readOnly int
	if err := upgraded.db.QueryRow("SELECT read_only FROM connection_tools LIMIT 1").Scan(&readOnly); err != nil && err != sql.ErrNoRows {
		t.Fatalf("connection_tools.read_only missing: %v", err)
	}

	// Verify LatestContextManifest reads from context_manifests (not deleted v3_context_manifests).
	ctx := t.Context()
	run, err := upgraded.Submit(ctx, "cm-test", "check manifest", "ops", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgraded.db.ExecContext(ctx, `INSERT INTO run_bindings(run_id,generation,accepted_at) VALUES(?,0,1234)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := upgraded.db.ExecContext(ctx, `INSERT INTO context_manifests(run_id,generation,assembled_at,previous_revision,control_revision,manifest) VALUES(?,0,1234,1,2,'{"ok":true}')`, run.ID); err != nil {
		t.Fatal(err)
	}
	cm, err := upgraded.LatestContextManifest(ctx, run.TaskID)
	if err != nil || cm.TaskID != run.TaskID || cm.UsedRefs != `{"ok":true}` {
		t.Fatalf("LatestContextManifest failed: %+v, %v", cm, err)
	}

	// Verify DomainSnapshot, ClusterTrendStats, and IncidentWorkflow expose unwired/not_connected status.
	snap, err := upgraded.DomainSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"fault_segments", "self_heal", "delivery_stage_events"} {
		if !slices.Contains(snap.UnwiredSections, expected) {
			t.Fatalf("DomainSnapshot.UnwiredSections missing %q: %v", expected, snap.UnwiredSections)
		}
	}
	trends, err := upgraded.ClusterTrendStats(ctx, "user:ops", "24h", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if trends.FaultSourceStatus != "not_connected" {
		t.Fatalf("ClusterTrendStats.FaultSourceStatus = %q, want not_connected", trends.FaultSourceStatus)
	}
	tx, err := upgraded.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	inc, err := createIncident(tx, Incident{Symptoms: "high temp", ImpactScope: `{"scope":"pod7","node":"node-1"}`}, 1000)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wf, err := upgraded.IncidentWorkflow(ctx, inc.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Stages[1].DataStatus != "not_connected" || wf.Stages[2].DataStatus != "not_connected" {
		t.Fatalf("IncidentWorkflow stages did not report not_connected: %+v", wf.Stages)
	}
}
