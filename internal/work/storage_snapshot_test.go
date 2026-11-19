package work

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorageSnapshotIsConsistentAndReadOnly(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Use a disposable SQLite-only fixture to exercise freelist accounting.
	if _, err = s.db.Exec(`CREATE TABLE storage_fixture(value BLOB); INSERT INTO storage_fixture VALUES(zeroblob(262144)); DROP TABLE storage_fixture;`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.db.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.Lstat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		out := s.ReadStorageSnapshot(t.Context())
		if out.Scope != storageScope || out.CheckedAt <= 0 || out.AutomaticCleanup != "not_enabled" ||
			out.Database.State != "available" || out.Database.AllocatedBytes == nil || *out.Database.AllocatedBytes <= 0 ||
			out.Database.InternallyReusableBytes == nil || *out.Database.InternallyReusableBytes <= 0 ||
			*out.Database.InternallyReusableBytes > *out.Database.AllocatedBytes ||
			out.WAL.State != "available" || out.WAL.Bytes == nil || *out.WAL.Bytes != walBefore.Size() {
			t.Fatalf("incorrect storage observation: %+v %+v", out.Database, out.WAL)
		}
	}
	after, err := os.Lstat(path + "-wal")
	if err != nil || after.Size() != walBefore.Size() || !after.ModTime().Equal(walBefore.ModTime()) {
		t.Fatalf("diagnostics changed physical WAL: %v", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA data_version").Scan(&version); err != nil || version != before {
		t.Fatalf("diagnostics changed SQLite: %d→%d %v", before, version, err)
	}
}

func TestStorageSnapshotDoesNotTreatUnknownOrSymlinkAsStorage(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "main")
	if err := os.WriteFile(main, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Store{storageLocation: configuredSQLiteStorage(main)}
	if out := s.ReadStorageSnapshot(t.Context()); out.Database.State != "unavailable" || out.WAL.State != "absent" ||
		out.WAL.Bytes != nil {
		t.Fatalf("unexpected missing WAL observation: %+v", out)
	}
	if err := os.Symlink(main, main+"-wal"); err != nil {
		t.Fatal(err)
	}
	if out := s.ReadStorageSnapshot(t.Context()); out.WAL.State != "unavailable" || out.WAL.Bytes != nil {
		t.Fatalf("linked WAL should not be reported: %+v", out)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, ":memory:", "file:" + main} {
		s.storageLocation = configuredSQLiteStorage(path)
		if out := s.ReadStorageSnapshot(t.Context()); out.Database.State != "unavailable" || out.WAL.State != "unavailable" {
			t.Fatalf("unexpected unsupported location result: %+v", out)
		}
	}
}
