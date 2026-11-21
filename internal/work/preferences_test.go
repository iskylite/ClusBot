package work

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestGeneralPreferencesPersistAndRejectStaleWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "preferences.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.GeneralPreferences(ctx)
	if err != nil || p != DefaultGeneralPreferences() {
		t.Fatalf("unexpected defaults: %+v, %v", p, err)
	}
	p.Language = "en-US"
	p.Timezone = "Asia/Shanghai"
	p.Appearance = "dark"
	p.ReduceMotion = true
	p.SendShortcut = "enter"
	p.DesktopNotifications = true
	p, err = store.SaveGeneralPreferences(ctx, p)
	if err != nil || p.Revision != 1 {
		t.Fatalf("save preferences: %+v, %v", p, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GeneralPreferences(ctx)
	if err != nil || got != p {
		t.Fatalf("preferences did not persist: %+v, %v", got, err)
	}

	// Two clients saved from the same revision may not overwrite each other.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, appearance := range []string{"light", "system"} {
		wg.Add(1)
		go func(appearance string) {
			defer wg.Done()
			candidate := p
			candidate.Appearance = appearance
			_, err := store.SaveGeneralPreferences(ctx, candidate)
			results <- err
		}(appearance)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrPreferencesRevision) {
			conflict++
		} else {
			t.Fatalf("save failed: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("expected one save and one conflict, got %d and %d", success, conflict)
	}
	got, err = store.GeneralPreferences(ctx)
	if err != nil || got.Revision != 2 || got.Timezone != p.Timezone || !got.DesktopNotifications {
		t.Fatalf("concurrent save damaged fields: %+v, %v", got, err)
	}
	got.Timezone = "Not/A_Timezone"
	if _, err := store.SaveGeneralPreferences(ctx, got); !errors.Is(err, ErrInvalidPreferences) {
		t.Fatalf("invalid timezone accepted: %v", err)
	}
}

func TestGeneralPreferencesMigrateExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE general_preferences; PRAGMA user_version=129"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p, err := store.GeneralPreferences(context.Background())
	if err != nil || p != DefaultGeneralPreferences() {
		t.Fatalf("old database did not migrate: %+v, %v", p, err)
	}
}

func TestGeneralPreferencesDoNotAssignUnownedDatabase(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "unowned.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SaveGeneralPreferences(ctx, DefaultGeneralPreferences()); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAccountBusinessStore(ctx, "user:alice"); err == nil {
		t.Fatal("preferences-only database was assigned to an account")
	}
}

func TestGeneralPreferencesLegacyBackupCheck(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	path := filepath.Join(base, "root.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveGeneralPreferences(ctx, DefaultGeneralPreferences()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyNoLegacyServerData(ctx, path, base, "", filepath.Join(base, "backup")); !errors.Is(err, errLegacyServerData) {
		t.Fatalf("unowned preferences were not detected: %v", err)
	}

	// An old, stopped server database may predate the preferences table.
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE general_preferences; PRAGMA user_version=129"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyNoLegacyServerData(ctx, path, base, "", filepath.Join(base, "backup")); err != nil {
		t.Fatalf("older database incorrectly rejected: %v", err)
	}
}
