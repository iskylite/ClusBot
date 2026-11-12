package work

import (
	"database/sql"
	"path/filepath"
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
}
