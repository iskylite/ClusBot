package work

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func validateBackupDatabase(ctx context.Context, path string, schema int) error {
	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro"))
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != schema {
		return errors.New("数据库版本与备份清单不符")
	}
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("备份数据库完整性检查失败")
	}
	return nil
}

func prepareRestoredStore(ctx context.Context, staging, destination string, manifest backupManifest) error {
	path := filepath.Join(staging, backupDB)
	if err := validateBackupDatabase(ctx, path, manifest.Schema); err != nil {
		return err
	}
	server, err := isServerBackupDatabase(ctx, path)
	if err != nil {
		return err
	}
	if server {
		return errors.New("旧版备份不包含服务器的个人数据，不能恢复为完整服务器备份")
	}
	uploads, err := backupUploads(ctx, path)
	if err != nil {
		return err
	}
	expected := make(map[string]backupFile, len(uploads))
	for _, item := range uploads {
		expected[item.Path] = item
	}
	for _, item := range manifest.Files {
		if !strings.HasPrefix(item.Path, "uploads/") {
			continue
		}
		if item != expected[item.Path] {
			return ErrUploadChanged
		}
		delete(expected, item.Path)
	}
	if len(expected) > 0 {
		return ErrUploadChanged
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	err = store.restoreWork(ctx, filepath.Join(staging, "workspace"), filepath.Join(destination, "workspace"), manifest.WorkspaceID)
	if err = errors.Join(err, store.Close()); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func (s *Store) restoreWork(ctx context.Context, stagedWorkspace, finalWorkspace, originalIdentity string) error {
	if err := verifyWritesSettled(ctx, s.db); err != nil {
		return err
	}
	if err := s.OpenWorkspace(stagedWorkspace); err != nil {
		return err
	}
	if err := s.Recover(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status='interrupted',status_reason='restored',reason='从备份恢复，等待你继续',revision=revision+1,updated_at=?
WHERE id IN (SELECT task_id FROM runs WHERE status='queued')`, now()); err != nil {
		return err
	}
	for _, query := range []string{
		`UPDATE run_reads SET status='not_run',detail='从备份恢复，工具未调用'
WHERE status='queued' AND run_id IN (SELECT id FROM runs WHERE status='queued')`,
		`UPDATE runs SET status='interrupted',error='从备份恢复，等待你继续',error_code='restored' WHERE status='queued'`,
		`UPDATE goals SET status='stopped',revision=revision+1 WHERE status='active'`,
		`UPDATE connections SET enabled=0,status='untested',protocol='',credential_binding='',detail='',checked_at=0,revision=revision+1`,
		`DELETE FROM connection_tools`,
	} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+publicationColumns+
		" FROM publications WHERE (status='pending' OR (status='succeeded' AND workspace_id=?)) ORDER BY rowid", originalIdentity)
	if err != nil {
		return err
	}
	var entries []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	finalIdentity := workspaceIdentity(finalWorkspace, s.workspace.info)
	for _, p := range entries {
		if p.Status == "pending" {
			p.Status, p.Detail = "expired", "备份恢复后，原写入审批已失效"
			if err = savePublication(tx, p); err != nil {
				return err
			}
			continue
		}
		if !validFilename(p.Filename) {
			continue
		}
		actual, err := s.workspace.readOutput(p.Filename)
		if err == nil && actual.hash == p.Digest {
			if _, err = tx.ExecContext(ctx, "UPDATE publications SET workspace_id=? WHERE id=?", finalIdentity, p.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
