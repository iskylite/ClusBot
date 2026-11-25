package work

import "database/sql"

func migrateCleanupRetiredPrototypeTablesAndRSI(tx *sql.Tx) error {
	// Drop tables from retired prototype schemas (child tables first).
	retiredTables := []string{
		"skill_replays",
		"v3_skills",
		"report_fragment_conflicts",
		"report_fragments",
		"worker_reports",
		"delegation_handovers",
		"delegations",
		"evidence_reads",
		"evidence_references",
		"image_verifications",
		"golden_images",
		"material_versions",
		"material_indexes",
		"service_check_ownership",
		"service_watermarks",
		"service_events",
		"service_instances",
		"schema_discoveries",
		"capability_snapshots",
		"scheduler_fairness_log",
		"scheduler_backpressure",
		"scheduler_groups",
		"read_cache_keys",
		"revocation_gates",
		"provenance_closures",
	}
	for _, table := range retiredTables {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS skill_evaluation_suites (
  id TEXT PRIMARY KEY,
  scope_label TEXT NOT NULL,
  goal TEXT NOT NULL,
  suite_json TEXT NOT NULL,
  suite_digest TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(scope_label, goal, suite_digest)
)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS skill_evaluation_runs (
  id TEXT PRIMARY KEY,
  suite_id TEXT NOT NULL REFERENCES skill_evaluation_suites(id),
  scope_label TEXT NOT NULL,
  goal TEXT NOT NULL,
  suite_digest TEXT NOT NULL,
  snapshot_digest TEXT NOT NULL,
  report_digest TEXT NOT NULL,
  model TEXT NOT NULL,
  report_json TEXT NOT NULL,
  created_at INTEGER NOT NULL
)`); err != nil {
		return err
	}
	var hasSkillProposals bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='skill_proposals')").Scan(&hasSkillProposals); err != nil {
		return err
	}
	if hasSkillProposals {
		for _, alter := range []struct {
			column string
			ddl    string
		}{
			{"suite_digest", `ALTER TABLE skill_proposals ADD COLUMN suite_digest TEXT NOT NULL DEFAULT ''`},
			{"evaluation_run_id", `ALTER TABLE skill_proposals ADD COLUMN evaluation_run_id TEXT NOT NULL DEFAULT ''`},
			{"match_mode", `ALTER TABLE skill_proposals ADD COLUMN match_mode TEXT NOT NULL DEFAULT 'goal_exact'`},
			{"match_keywords", `ALTER TABLE skill_proposals ADD COLUMN match_keywords TEXT NOT NULL DEFAULT '[]'`},
			{"draft_mode", `ALTER TABLE skill_proposals ADD COLUMN draft_mode TEXT NOT NULL DEFAULT 'template'`},
		} {
			if err := addColumnIfMissing(tx, "skill_proposals", alter.column, alter.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func migrateGovernanceAndMCPReadOnly(tx *sql.Tx) error {
	var hasConnectionTools bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='connection_tools')").Scan(&hasConnectionTools); err != nil {
		return err
	}
	if hasConnectionTools {
		if err := addColumnIfMissing(tx, "connection_tools", "read_only",
			`ALTER TABLE connection_tools ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	retiredTables := []string{
		"goal_tasks",
		"goals",
		"schedule_occurrences",
		"schedules",
		"v3_context_manifests",
		"workflow_template_changes",
		"workflow_bindings",
		"workflow_templates",
		"asset_admission_decisions",
		"cleanup_progress",
		"domain_process_refs",
		"self_heal_counters",
	}
	for _, table := range retiredTables {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return err
		}
	}
	return nil
}

func migrateRetireRedundantDomainPrototypeTables(tx *sql.Tx) error {
	for _, table := range []string{
		"case_events",
		"operational_cases",
		"cleanup_queue",
		"domain_relation_types",
		"domain_object_types",
		"domain_packages",
	} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return err
		}
	}
	return nil
}
