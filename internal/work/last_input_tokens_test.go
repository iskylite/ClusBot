package work

import (
	"path/filepath"
	"testing"
)

// Fresh databases must carry last_input_tokens and surface it on the run usage.
func TestLastInputTokensRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Column exists on a freshly created schema.
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != databaseSchemaVersion {
		t.Fatalf("fresh schema version = %d, want %d", version, databaseSchemaVersion)
	}

	conv := id()
	run := id()
	if _, err := store.db.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", conv, "t", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("INSERT INTO runs(id,conversation_id,status,created_at,usage_tracked) VALUES(?,?,?,?,1)", run, conv, "running", now()); err != nil {
		t.Fatal(err)
	}

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Two requests: the second (last) input must win for last_input_tokens,
	// while input_tokens accumulates.
	first := int64(100)
	second := int64(250)
	out := int64(10)
	if err := recordModelUsage(tx, run, &TokenUsage{InputTokens: &first, OutputTokens: &out}); err != nil {
		t.Fatal(err)
	}
	if err := recordModelUsage(tx, run, &TokenUsage{InputTokens: &second, OutputTokens: &out}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	r, err := scanRun(store.db.QueryRow("SELECT " + runColumns + " FROM runs WHERE id=?", run))
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil {
		t.Fatal("usage missing")
	}
	if r.Usage.InputTokens == nil || *r.Usage.InputTokens != 350 {
		t.Errorf("accumulated input = %v, want 350", r.Usage.InputTokens)
	}
	if r.Usage.LastInputTokens == nil || *r.Usage.LastInputTokens != 250 {
		t.Errorf("last input = %v, want 250", r.Usage.LastInputTokens)
	}
}
