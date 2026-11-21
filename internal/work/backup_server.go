package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Older local backups predate account tables. Do not require those tables in
// order to recognize their databases as local.
func isServerBackupDatabase(ctx context.Context, name string) (bool, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("数据目录中的数据库须为普通文件")
	}
	db, err := sql.Open("sqlite", sqliteFileURI(name, "mode=ro"))
	if err != nil {
		return false, err
	}
	defer db.Close()
	for _, table := range []string{"account_instance", "account_bootstrap", "accounts"} {
		var exists bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+")").Scan(&exists); err != nil {
				return false, err
			}
			if exists {
				return true, nil
			}
		}
	}
	return false, nil
}

func validBackupAccountID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

func validServerBackupPath(name string, accounts map[string]bool) bool {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) != 3 || parts[0] != "account-data" || !accounts[parts[1]] {
		return false
	}
	rel := parts[2]
	return rel == backupDB || validUploadPath(rel) || strings.HasPrefix(rel, "workspace/materials/") || strings.HasPrefix(rel, "workspace/outputs/")
}

// The snapshot contains password hashes and instance configuration. Session
// digests are removed before publishing the backup, including from free pages.
func clearBackupSessions(ctx context.Context, name string) error {
	db, err := sql.Open("sqlite", sqliteFileURI(name, ""))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err == nil && mode != "delete" {
		err = errors.New("无法安全处理备份数据库日志")
	}
	if err == nil {
		_, err = db.ExecContext(ctx, "PRAGMA secure_delete=ON")
	}
	if err == nil {
		_, err = db.ExecContext(ctx, "DELETE FROM account_sessions")
	}
	if err == nil {
		_, err = db.ExecContext(ctx, "VACUUM")
	}
	if err = errors.Join(err, db.Close()); err != nil {
		return err
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

var errLegacyServerData = errors.New("服务器实例根库或根工作区含有旧版单人业务数据，无法完整备份；请先单独保管旧数据并确认处理方式")

func verifyNoLegacyServerData(ctx context.Context, database, dataDir, workspaceDir, destination string) error {
	db, err := sql.Open("sqlite", sqliteFileURI(database, "mode=ro"))
	if err != nil {
		return err
	}
	var legacy bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations) OR EXISTS(SELECT 1 FROM tasks)
OR EXISTS(SELECT 1 FROM runs) OR EXISTS(SELECT 1 FROM messages) OR EXISTS(SELECT 1 FROM artifacts)
OR EXISTS(SELECT 1 FROM publications) OR EXISTS(SELECT 1 FROM materials) OR EXISTS(SELECT 1 FROM uploads)
OR EXISTS(SELECT 1 FROM memories) OR EXISTS(SELECT 1 FROM goals) OR EXISTS(SELECT 1 FROM task_acceptances)`).Scan(&legacy)
	// A stopped installation may still be on a schema older than preferences.
	// If the table exists, its root-store row also has no account owner.
	if err == nil && !legacy {
		var hasPreferences bool
		err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='general_preferences')`).Scan(&hasPreferences)
		if err == nil && hasPreferences {
			err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM general_preferences)`).Scan(&legacy)
		}
	}
	if err = errors.Join(err, db.Close()); err != nil {
		return err
	}
	if legacy {
		return errLegacyServerData
	}
	// Root uploads or workspaces from a previous local-mode installation have
	// no reliable account owner. Never silently omit or assign those files.
	uploads := filepath.Join(dataDir, "uploads")
	if err := rejectLegacyFiles(uploads); err != nil {
		return err
	}
	workspaces := []string{filepath.Join(dataDir, "workspace")}
	if strings.TrimSpace(workspaceDir) != "" {
		workspaces = append(workspaces, workspaceDir)
	}
	for _, workspace := range workspaces {
		workspace, err := resolvedDirectory(workspace)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if within(workspace, destination) {
			return errors.New("备份目录须位于数据目录与工作区之外")
		}
		for _, name := range []string{"materials", "outputs"} {
			if err := rejectLegacyFiles(filepath.Join(workspace, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectLegacyFiles(name string) error {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errLegacyServerData
	}
	return filepath.WalkDir(name, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return errLegacyServerData
		}
		return nil
	})
}

func backupServer(ctx context.Context, dataDir, workspaceDir, destination string) error {
	data, err := os.OpenRoot(dataDir)
	if err != nil {
		return err
	}
	defer data.Close()
	info, err := data.Lstat("account-data")
	if err != nil || !info.IsDir() {
		return errors.New("服务器个人数据目录不存在或不是目录")
	}
	accounts, err := data.OpenRoot("account-data")
	if err != nil {
		return err
	}
	defer accounts.Close()
	opened, err := accounts.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("服务器个人数据目录已变更")
	}
	dir, err := accounts.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	if err = errors.Join(readErr, dir.Close()); err != nil {
		return err
	}
	manifest := backupManifest{Version: 2, CreatedAt: now()}
	return stageDirectory(destination, func(staging string, dest *os.Root) error {
		rootDB := filepath.Join(staging, backupDB)
		manifest.Schema, err = snapshotDatabase(ctx, filepath.Join(dataDir, backupDB), rootDB)
		if err != nil {
			return err
		}
		if err = verifyNoLegacyServerData(ctx, rootDB, dataDir, workspaceDir, destination); err != nil {
			return err
		}
		if err = clearBackupSessions(ctx, rootDB); err != nil {
			return err
		}
		if err = validateBackupDatabase(ctx, rootDB, manifest.Schema); err != nil {
			return err
		}
		rootFile, err := transferBackupFile(ctx, dest, backupDB, nil, backupDB)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, rootFile)
		if err = dest.Mkdir("account-data", 0700); err != nil {
			return err
		}
		for _, entry := range entries {
			id := entry.Name()
			if !entry.IsDir() || !validBackupAccountID(id) {
				return fmt.Errorf("个人数据目录含有无效项 %q", id)
			}
			if err = backupServerAccount(ctx, accounts, dataDir, staging, dest, rootDB, id, &manifest); err != nil {
				return fmt.Errorf("备份账号 %s 失败：%w", id, err)
			}
		}
		if current, err := data.Lstat("account-data"); err != nil || !os.SameFile(info, current) {
			return errors.New("服务器个人数据目录已变更")
		}
		file, err := dest.OpenFile(backupManifestName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		return errors.Join(encoder.Encode(manifest), file.Sync(), file.Close())
	})
}

func backupServerAccount(ctx context.Context, accounts *os.Root, dataDir, staging string, dest *os.Root, rootDB, id string, manifest *backupManifest) error {
	if len(manifest.Files) >= 10000 {
		return errors.New("文件数量超出备份上限")
	}
	info, err := accounts.Lstat(id)
	if err != nil || !info.IsDir() {
		return errors.New("个人数据目录已变更")
	}
	account, err := accounts.OpenRoot(id)
	if err != nil {
		return err
	}
	defer account.Close()
	opened, err := account.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("个人数据目录已变更")
	}
	workspaceInfo, err := account.Lstat("workspace")
	if err != nil || !workspaceInfo.IsDir() {
		return ErrWorkspace
	}
	workspace, err := account.OpenRoot("workspace")
	if err != nil {
		return err
	}
	defer workspace.Close()
	workspaceOpened, err := workspace.Stat(".")
	if err != nil || !os.SameFile(workspaceInfo, workspaceOpened) {
		return ErrWorkspace
	}
	output, err := workspace.Lstat("outputs")
	if err != nil || !output.IsDir() {
		return ErrWorkspace
	}
	if materials, err := workspace.Lstat("materials"); err != nil || !materials.IsDir() {
		return ErrWorkspace
	}
	prefix := path.Join("account-data", id)
	if err = dest.MkdirAll(prefix, 0700); err != nil {
		return err
	}
	database := filepath.Join(staging, filepath.FromSlash(prefix), backupDB)
	version, err := snapshotDatabase(ctx, filepath.Join(dataDir, filepath.FromSlash(prefix), backupDB), database)
	if err != nil {
		return err
	}
	if err = clearBackupSessions(ctx, database); err != nil {
		return err
	}
	if err = validateBackupDatabase(ctx, database, version); err != nil {
		return err
	}
	if err = validateBackupAccount(ctx, rootDB, database, id); err != nil {
		return err
	}
	dbFile, err := transferBackupFile(ctx, dest, path.Join(prefix, backupDB), nil, path.Join(prefix, backupDB))
	if err != nil {
		return err
	}
	manifest.Files = append(manifest.Files, dbFile)
	uploads, err := backupUploads(ctx, database)
	if err != nil {
		return err
	}
	uploadInfo, err := account.Lstat("uploads")
	if err != nil || !uploadInfo.IsDir() {
		return ErrUploadChanged
	}
	uploadRoot, err := account.OpenRoot("uploads")
	if err != nil {
		return err
	}
	defer uploadRoot.Close()
	if opened, err := uploadRoot.Stat("."); err != nil || !os.SameFile(uploadInfo, opened) {
		return ErrUploadChanged
	}
	uploadDir, err := uploadRoot.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := uploadDir.ReadDir(-1)
	if err = errors.Join(readErr, uploadDir.Close()); err != nil {
		return err
	}
	expectedNames := make(map[string]bool, len(uploads))
	for _, expected := range uploads {
		expectedNames[path.Base(expected.Path)] = true
	}
	for _, entry := range entries {
		if !expectedNames[entry.Name()] || !entry.Type().IsRegular() {
			return ErrUploadChanged
		}
	}
	if len(entries) != len(uploads) {
		return ErrUploadChanged
	}
	if len(uploads) > 0 {
		if err = dest.MkdirAll(path.Join(prefix, "uploads"), 0700); err != nil {
			return err
		}
		for _, expected := range uploads {
			if len(manifest.Files) >= 10000 {
				return errors.New("文件数量超出备份上限")
			}
			target := path.Join(prefix, expected.Path)
			actual, err := transferBackupFile(ctx, uploadRoot, path.Base(expected.Path), dest, target)
			if err != nil {
				return err
			}
			expected.Path = target
			if actual != expected {
				return ErrUploadChanged
			}
			manifest.Files = append(manifest.Files, actual)
		}
	}
	if current, err := account.Lstat("uploads"); err != nil || !os.SameFile(uploadInfo, current) {
		return ErrUploadChanged
	}
	for _, name := range []string{"materials", "outputs"} {
		if err = dest.MkdirAll(path.Join(prefix, "workspace", name), 0700); err != nil {
			return err
		}
		err = fs.WalkDir(workspace.FS(), name, func(rel string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			target := path.Join(prefix, "workspace", rel)
			if entry.IsDir() {
				return dest.MkdirAll(target, 0700)
			}
			if len(manifest.Files) >= 10000 || len(target) > 1024 {
				return errors.New("文件数量或路径长度超出备份上限")
			}
			file, err := transferBackupFile(ctx, workspace, rel, dest, target)
			if err == nil {
				manifest.Files = append(manifest.Files, file)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	if current, err := workspace.Lstat("outputs"); err != nil || !os.SameFile(output, current) {
		return ErrWorkspace
	}
	if current, err := account.Lstat("workspace"); err != nil || !os.SameFile(workspaceInfo, current) {
		return ErrWorkspace
	}
	if current, err := accounts.Lstat(id); err != nil || !os.SameFile(info, current) {
		return errors.New("个人数据目录已变更")
	}
	manifest.Accounts = append(manifest.Accounts, backupAccount{
		ID: id, Schema: version, WorkspaceID: workspaceIdentity(filepath.Join(dataDir, filepath.FromSlash(prefix), "workspace"), output),
	})
	return nil
}

func validateBackupAccount(ctx context.Context, rootDB, accountDB, id string) error {
	root, err := sql.Open("sqlite", sqliteFileURI(rootDB, "mode=ro"))
	if err != nil {
		return err
	}
	defer root.Close()
	var exists bool
	if err := root.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE principal_id=?)", "user:"+id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("个人数据目录没有对应账号")
	}
	account, err := sql.Open("sqlite", sqliteFileURI(accountDB, "mode=ro"))
	if err != nil {
		return err
	}
	defer account.Close()
	var owner string
	if err := account.QueryRowContext(ctx, "SELECT principal_id FROM account_business_owner WHERE singleton=1").Scan(&owner); err != nil {
		return err
	}
	if owner != "user:"+id {
		return errors.New("个人数据库归属与所属目录不符")
	}
	return nil
}

func restoreServer(ctx context.Context, source *os.Root, destination string, manifest backupManifest) error {
	return stageDirectory(destination, func(staging string, dest *os.Root) error {
		if err := dest.Mkdir("account-data", 0700); err != nil {
			return err
		}
		for _, account := range manifest.Accounts {
			prefix := path.Join("account-data", account.ID)
			for _, name := range []string{"uploads", "workspace/materials", "workspace/outputs"} {
				if err := dest.MkdirAll(path.Join(prefix, name), 0700); err != nil {
					return err
				}
			}
		}
		for _, expected := range manifest.Files {
			if err := dest.MkdirAll(path.Dir(expected.Path), 0700); err != nil {
				return err
			}
			actual, err := transferBackupFile(ctx, source, expected.Path, dest, expected.Path)
			if err != nil {
				return err
			}
			if actual != expected {
				return fmt.Errorf("备份校验失败：%s", expected.Path)
			}
		}
		rootDB := filepath.Join(staging, backupDB)
		if err := validateBackupDatabase(ctx, rootDB, manifest.Schema); err != nil {
			return err
		}
		server, err := isServerBackupDatabase(ctx, rootDB)
		if err != nil {
			return err
		}
		if !server {
			return errors.New("服务器备份的实例数据库不匹配")
		}
		if err := verifyNoLegacyServerData(ctx, rootDB, staging, "", destination); err != nil {
			return err
		}
		for _, account := range manifest.Accounts {
			if err := prepareRestoredAccount(ctx, staging, destination, rootDB, account, manifest.Files); err != nil {
				return fmt.Errorf("恢复账号 %s 失败：%w", account.ID, err)
			}
		}
		return resetRestoredServer(ctx, rootDB)
	})
}

func prepareRestoredAccount(ctx context.Context, staging, destination, rootDB string, account backupAccount, files []backupFile) error {
	prefix := path.Join("account-data", account.ID)
	directory := filepath.Join(staging, filepath.FromSlash(prefix))
	database := filepath.Join(directory, backupDB)
	if err := validateBackupDatabase(ctx, database, account.Schema); err != nil {
		return err
	}
	if err := validateBackupAccount(ctx, rootDB, database, account.ID); err != nil {
		return err
	}
	uploads, err := backupUploads(ctx, database)
	if err != nil {
		return err
	}
	expected := make(map[string]backupFile, len(uploads))
	for _, item := range uploads {
		item.Path = path.Join(prefix, item.Path)
		expected[item.Path] = item
	}
	for _, item := range files {
		if !strings.HasPrefix(item.Path, prefix+"/uploads/") {
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
	store, err := Open(database)
	if err != nil {
		return err
	}
	err = store.restoreWork(ctx, filepath.Join(directory, "workspace"), filepath.Join(destination, filepath.FromSlash(prefix), "workspace"), account.WorkspaceID)
	if err = errors.Join(err, store.Close()); err != nil {
		return err
	}
	return clearBackupSessions(ctx, database)
}

func resetRestoredServer(ctx context.Context, database string) error {
	db, err := sql.Open("sqlite", sqliteFileURI(database, ""))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE connections SET enabled=0,status='untested',protocol='',credential_binding='',detail='',checked_at=0,revision=revision+1;
DELETE FROM connection_tools`)
	if err = errors.Join(err, db.Close()); err != nil {
		return err
	}
	return clearBackupSessions(ctx, database)
}
