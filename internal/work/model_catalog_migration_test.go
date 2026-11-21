package work

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestModelProviderCatalogMigrationKeepsLegacyCredentialsAndPins(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateModelProfiles(tx); err != nil {
		t.Fatal(err)
	}
	if err := migrateModelRoleBindings(tx); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO model_connection_profiles VALUES
 ('old-a','first',3,'responses','https://same.invalid','model-a','env:FIRST_KEY','null'),
 ('old-b','second',7,'responses','https://same.invalid','model-b','env:SECOND_KEY','null');
INSERT INTO model_role_bindings VALUES('benchmark_admission',2,'old-b',7,'credential-pin')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateModelProviderCatalog(tx); err != nil {
		t.Fatal(err)
	}
	if err := migrateModelProviderCatalog(tx); err != nil {
		t.Fatalf("migration was not idempotent: %v", err)
	}
	if err := migrateModelProviderAPIKey(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	a, err := store.ReadModelProfile(t.Context(), "old-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.ReadModelProfile(t.Context(), "old-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProviderID == b.ProviderID || !a.Enabled || !b.ProviderEnabled || a.Settings.KeyRef != "env:FIRST_KEY" || b.Settings.KeyRef != "env:SECOND_KEY" || a.Settings.Revision != 3 || b.Settings.Revision != 7 {
		t.Fatalf("legacy profiles changed or different credentials were merged: %+v %+v", a, b)
	}
	binding, p, err := store.ReadModelRole(t.Context(), BenchmarkAdmissionModelRole)
	if err != nil || binding.ProfileID != "old-b" || binding.ProfileRevision != 7 || binding.CredentialBinding != "credential-pin" || p.Settings.KeyRef != "env:SECOND_KEY" {
		t.Fatalf("role pin changed: %+v %+v %v", binding, p, err)
	}
	selection, err := store.ModelDefault(t.Context())
	if err != nil || selection.ProfileID != "" || selection.Revision != 0 {
		t.Fatalf("legacy default changed: %+v %v", selection, err)
	}
}
