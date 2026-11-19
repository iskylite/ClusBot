package work

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const storageScope = "sqlite_main_and_wal_only"

type StorageSnapshot struct {
	Scope            string             `json:"scope"`
	CheckedAt        int64              `json:"checked_at_unix_ms"`
	Database         SQLiteStoragePages `json:"database"`
	WAL              SQLiteStorageWAL   `json:"wal"`
	AutomaticCleanup string             `json:"automatic_cleanup"`
}

type SQLiteStoragePages struct {
	State                   string `json:"state"`
	AllocatedBytes          *int64 `json:"allocated_bytes,omitempty"`
	InternallyReusableBytes *int64 `json:"internally_reusable_bytes,omitempty"`
}

type SQLiteStorageWAL struct {
	State string `json:"state"`
	Bytes *int64 `json:"bytes,omitempty"`
}

// identity pins the configured main file, not any filename from the caller.
type sqliteStorageLocation struct {
	path     string
	identity os.FileInfo
}

func configuredSQLiteStorage(path string) sqliteStorageLocation {
	if path == "" || strings.HasPrefix(path, ":") || strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?#\x00") {
		return sqliteStorageLocation{}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return sqliteStorageLocation{}
	}
	info, err := statRegularSQLiteFile(path)
	if err != nil {
		return sqliteStorageLocation{}
	}
	return sqliteStorageLocation{path: path, identity: info}
}

// Inspect only exact path components and the named file, never the contents
// or directory entries. Reject symlinked components rather than guessing an
// alternate location (including unexpected WAL links).
func statRegularSQLiteFile(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, os.ErrInvalid
	}
	var parents []string
	for parent := filepath.Dir(path); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		parents = append(parents, parent)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		info, err := os.Lstat(parents[i])
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, os.ErrInvalid
		}
	}

	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	return info, nil
}

func (s *Store) ReadStorageSnapshot(ctx context.Context) StorageSnapshot {
	out := StorageSnapshot{Scope: storageScope, CheckedAt: now(), AutomaticCleanup: "not_enabled",
		Database: SQLiteStoragePages{State: "unavailable"}, WAL: SQLiteStorageWAL{State: "unavailable"}}
	location := s.storageLocation
	if location.identity == nil {
		return out
	}
	current, err := statRegularSQLiteFile(location.path)
	if err != nil || !os.SameFile(location.identity, current) {
		return out
	}
	if wal, err := statRegularSQLiteFile(location.path + "-wal"); err == nil {
		bytes := wal.Size()
		if bytes >= 0 {
			out.WAL = SQLiteStorageWAL{State: "available", Bytes: &bytes}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		out.WAL.State = "absent"
	}
	// Do not enter SQLite through the reporting read pool when a WAL
	// exists but did not pass the configured location/regular-file check.
	if s.readPool == nil || out.WAL.State == "unavailable" {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var pageSize, pages, free int64
	// One SQL statement on the already configured read-only pool; the
	// counts describe one SQLite snapshot, not a filesystem reclaim estimate.
	err = s.readPool.db.QueryRowContext(ctx, `SELECT a.page_size,b.page_count,c.freelist_count
FROM pragma_page_size() a CROSS JOIN pragma_page_count() b CROSS JOIN pragma_freelist_count() c`).Scan(&pageSize, &pages, &free)
	if err != nil || pageSize <= 0 || pages < 0 || free < 0 || free > pages || pages > math.MaxInt64/pageSize {
		return out
	}
	allocated, reusable := pages*pageSize, free*pageSize
	out.Database = SQLiteStoragePages{State: "available", AllocatedBytes: &allocated, InternallyReusableBytes: &reusable}
	return out
}
