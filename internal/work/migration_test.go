package work

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A database left at the previous schema version (45) must migrate: gain the
// last_input_tokens column and advance to the current version.
func TestMigrateFrom45AddsLastInputTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// Build a minimal v45 database: a runs table WITHOUT last_input_tokens,
	// a model_settings table, and a non-empty schema marker at user_version=45.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE runs(
		id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, task_id TEXT,
		status TEXT NOT NULL, response TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, control TEXT NOT NULL DEFAULT '',
		usage_tracked INTEGER NOT NULL DEFAULT 0, model_requests INTEGER NOT NULL DEFAULT 0,
		active_ms INTEGER NOT NULL DEFAULT 0, time_partial INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER, output_tokens INTEGER,
		error_code TEXT NOT NULL DEFAULT '', reported_requests INTEGER NOT NULL DEFAULT 0,
		execution_generation INTEGER NOT NULL DEFAULT 0, read_failure_run_id TEXT,
		memory_command INTEGER NOT NULL DEFAULT 0, admission_requests INTEGER NOT NULL DEFAULT 0,
		admission_output INTEGER NOT NULL DEFAULT 0, admission_output_used INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE model_settings(
		id INTEGER PRIMARY KEY CHECK(id=1), base_url TEXT NOT NULL, model_id TEXT NOT NULL,
		api_key_ref TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL DEFAULT 'responses',
		revision INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Real v45 databases carry context_manifests (it predates v45); the 46->47
	// migration ALTERs it, so the fixture must include the pre-46 shape.
	if _, err := old.Exec(`CREATE TABLE context_manifests(
		id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, generation INTEGER NOT NULL,
		assembled_at INTEGER NOT NULL, manifest TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("PRAGMA user_version=45"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("migrate from 45 failed: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != databaseSchemaVersion {
		t.Fatalf("version after migration = %d, want %d", version, databaseSchemaVersion)
	}
	// The new column exists and is writable.
	if _, err := store.db.Exec("UPDATE runs SET last_input_tokens=5 WHERE id='x'"); err != nil {
		t.Fatalf("last_input_tokens column missing after migration: %v", err)
	}
	// The 46->47 context_manifests columns exist after migration.
	if _, err := store.db.Exec("UPDATE context_manifests SET previous_revision=1, control_revision=1 WHERE id=-1"); err != nil {
		t.Fatalf("context_manifests revision columns missing after migration: %v", err)
	}
}

func TestLateSchemaUpgradeRollsBackAndPreservesLegacyDefault(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "late-schema.db")
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := ModelSettings{BaseURL: "http://127.0.0.1:48601", ModelID: "fixture-default", Protocol: "chat_completions", KeyRef: "env:CLUSBOT_FIXTURE_LEGACY_KEY"}
	if err := old.InitModelSettings(ctx, defaults); err != nil {
		t.Fatal(err)
	}
	// Reverse only the additive late migrations. The legacy default row and
	// all earlier production schema (including personal Skills) stay intact.
	tables := []string{"benchmark_admission_previews", "conversation_skill_bindings", "conversation_mcp_selections", "run_mcp_selections", "model_connection_profiles", "model_role_bindings"}
	for _, table := range tables {
		if _, err := old.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.db.Exec(`PRAGMA user_version=102;
CREATE INDEX model_role_bindings ON model_settings(model_id)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if broken, err := Open(path); err == nil {
		broken.Close()
		t.Fatal("last migration unexpectedly accepted a name collision")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, n int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 102 {
		t.Fatalf("failed upgrade advanced version: %d %v", version, err)
	}
	for _, table := range tables {
		if err := raw.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("failed upgrade left %s behind: %d %v", table, n, err)
		}
	}
	if _, err := raw.Exec("DROP INDEX model_role_bindings"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("Open after rollback/restart %d: %v", i, err)
		}
		func() {
			defer reopened.Close()
			// A different command configuration must not replace the DB's
			// previously configured default; initialization is insert-only.
			if err := reopened.InitModelSettings(ctx, ModelSettings{ModelID: "not-the-legacy-default", Protocol: "responses"}); err != nil {
				t.Fatal(err)
			}
			got, err := reopened.ModelSettings(ctx)
			defaults.Revision = 1
			if err != nil || got != defaults {
				t.Fatalf("legacy default or key reference changed: %+v %v", got, err)
			}
			binding, _, err := reopened.ReadModelRole(ctx, BenchmarkAdmissionModelRole)
			if err != nil || binding.ProfileID != "" || binding.Revision != 0 {
				t.Fatalf("new provider was selected without explicit configuration: %+v %v", binding, err)
			}
			if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion {
				t.Fatalf("upgrade/restart version: %d %v", version, err)
			}
		}()
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version=%d", databaseSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if tooNew, err := Open(path); err == nil {
		tooNew.Close()
		t.Fatal("older binary accepted a newer schema")
	} else if !strings.Contains(err.Error(), "不能就地降级") || !strings.Contains(err.Error(), "升级前备份") {
		t.Fatalf("downgrade guidance is misleading: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion+1 {
		t.Fatalf("downgrade rejection changed future schema version: %d %v", version, err)
	}
}

func TestRevisionModelRoleUpgradePreservesAdmissionRole(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "model-role-v107.db")
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := old.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DROP TABLE model_role_bindings"); err != nil {
		t.Fatal(err)
	}
	if err := migrateModelRoleBindings(tx); err != nil {
		t.Fatal(err)
	}
	before := ModelRoleBinding{Role: BenchmarkAdmissionModelRole, Revision: 4, ProfileID: "migration-profile", ProfileRevision: 7, CredentialBinding: "fixture-fingerprint"}
	if _, err := tx.Exec("INSERT INTO model_role_bindings VALUES(?,?,?,?,?)", before.Role, before.Revision, before.ProfileID, before.ProfileRevision, before.CredentialBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("PRAGMA user_version=107"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _, err := s.ReadModelRole(ctx, BenchmarkAdmissionModelRole)
	if err != nil || got != before {
		t.Fatalf("existing admission consent/pin changed: %+v %v", got, err)
	}
	next, _, err := s.ReadModelRole(ctx, BenchmarkRevisionModelRole)
	if err != nil || next.ProfileID != "" || next.Revision != 0 {
		t.Fatalf("upgrade implicitly activated new destination: %+v %v", next, err)
	}
}
