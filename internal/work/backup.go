package work

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"modernc.org/sqlite"
)

const backupManifestName = "manifest.json"
const backupDB = "clusbot.db"

type backupManifest struct {
	Version     int             `json:"version"`
	Schema      int             `json:"schema_version"`
	CreatedAt   int64           `json:"created_at"`
	WorkspaceID string          `json:"workspace_id"`
	Files       []backupFile    `json:"files"`
	Accounts    []backupAccount `json:"accounts,omitempty"`
}

type backupAccount struct {
	ID          string `json:"id"`
	Schema      int    `json:"schema_version"`
	WorkspaceID string `json:"workspace_id"`
}

type backupFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"sha256"`
}

func LockDataDir(dir string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("数据目录已由另一个 ClusBot 实例使用，请先停止服务")
	}
	return lock, nil
}

func Backup(ctx context.Context, dataDir, workspaceDir, destination string) error {
	dataDir, err := resolvedDirectory(dataDir)
	if err != nil {
		return err
	}
	destination, err = newDirectoryPath(destination)
	if err != nil {
		return err
	}
	if within(dataDir, destination) {
		return errors.New("备份目录须位于数据目录与工作区之外")
	}
	lock, err := LockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	server, err := isServerBackupDatabase(ctx, filepath.Join(dataDir, backupDB))
	if err != nil {
		return err
	}
	if server {
		return backupServer(ctx, dataDir, workspaceDir, destination)
	}
	workspaceDir, err = resolvedDirectory(workspaceDir)
	if err != nil {
		return err
	}
	if within(workspaceDir, destination) {
		return errors.New("备份目录须位于数据目录与工作区之外")
	}
	source, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return err
	}
	defer source.Close()
	output, err := source.Lstat("outputs")
	if err != nil || !output.IsDir() {
		return ErrWorkspace
	}
	if info, err := source.Lstat("materials"); err != nil || !info.IsDir() {
		return ErrWorkspace
	}
	manifest := backupManifest{Version: 1, CreatedAt: now(), WorkspaceID: workspaceIdentity(workspaceDir, output)}
	return stageDirectory(destination, func(staging string, dest *os.Root) error {
		version, err := snapshotDatabase(ctx, filepath.Join(dataDir, backupDB), filepath.Join(staging, backupDB))
		if err != nil {
			return err
		}
		manifest.Schema = version
		if err = validateBackupDatabase(ctx, filepath.Join(staging, backupDB), version); err != nil {
			return err
		}
		database, err := transferBackupFile(ctx, dest, backupDB, nil, backupDB)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, database)
		uploads, err := backupUploads(ctx, filepath.Join(staging, backupDB))
		if err != nil {
			return err
		}
		if len(uploads) >= 10000 {
			return errors.New("文件数量超出备份上限")
		}
		if len(uploads) > 0 {
			dataRoot, err := os.OpenRoot(dataDir)
			if err != nil {
				return err
			}
			defer dataRoot.Close()
			info, err := dataRoot.Lstat("uploads")
			if err != nil || !info.IsDir() {
				return ErrUploadChanged
			}
			if err = dest.Mkdir("uploads", 0700); err != nil {
				return err
			}
			for _, expected := range uploads {
				actual, err := transferBackupFile(ctx, dataRoot, expected.Path, dest, expected.Path)
				if err != nil {
					return err
				}
				if actual != expected {
					return ErrUploadChanged
				}
				manifest.Files = append(manifest.Files, actual)
			}
		}
		if len(manifest.Files) > 10000 {
			return errors.New("文件数量超出备份上限")
		}
		for _, name := range []string{"materials", "outputs"} {
			base := path.Join("workspace", name)
			if err = dest.MkdirAll(base, 0700); err != nil {
				return err
			}
			err = fs.WalkDir(source.FS(), name, func(rel string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				target := path.Join("workspace", rel)
				if entry.IsDir() {
					return dest.MkdirAll(target, 0700)
				}
				if len(manifest.Files) >= 10000 || len(target) > 1024 {
					return errors.New("文件数量或路径长度超出备份上限")
				}
				file, err := transferBackupFile(ctx, source, rel, dest, target)
				if err == nil {
					manifest.Files = append(manifest.Files, file)
				}
				return err
			})
			if err != nil {
				return err
			}
		}
		if current, err := source.Lstat("outputs"); err != nil || !os.SameFile(output, current) {
			return ErrWorkspace
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

func Restore(ctx context.Context, from, destination string) error {
	from, err := resolvedDirectory(from)
	if err != nil {
		return err
	}
	destination, err = newDirectoryPath(destination)
	if err != nil {
		return err
	}
	if within(from, destination) {
		return errors.New("恢复目录须位于备份目录之外")
	}
	source, err := os.OpenRoot(from)
	if err != nil {
		return err
	}
	defer source.Close()
	manifest, err := loadBackupManifest(source)
	if err != nil {
		return err
	}
	if manifest.Version == 2 {
		return restoreServer(ctx, source, destination, manifest)
	}
	return stageDirectory(destination, func(staging string, dest *os.Root) error {
		for _, name := range []string{"workspace/materials", "workspace/outputs"} {
			if err = dest.MkdirAll(name, 0700); err != nil {
				return err
			}
		}
		for _, expected := range manifest.Files {
			if err = dest.MkdirAll(path.Dir(expected.Path), 0700); err != nil {
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
		return prepareRestoredStore(ctx, staging, destination, manifest)
	})
}

func resolvedDirectory(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func newDirectoryPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("请指定新目录")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := resolvedDirectory(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	if _, err = os.Lstat(absolute); err == nil {
		return "", errors.New("目标目录已存在，请指定新目录")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return absolute, nil
}

func within(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func stageDirectory(destination string, write func(string, *os.Root) error) error {
	parent, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer parent.Close()
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".clusbot-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	root, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	writeErr := write(staging, root)
	if writeErr == nil {
		writeErr = syncBackupTree(root)
	}
	if err = errors.Join(writeErr, root.Close()); err != nil {
		return err
	}
	if err = renameWithoutReplace(int(parent.Fd()), filepath.Base(staging), filepath.Base(destination)); err != nil {
		return err
	}
	return parent.Sync()
}

func syncBackupTree(root *os.Root) error {
	var directories []string
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		dir, err := root.Open(directories[i])
		if err != nil {
			return err
		}
		if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}

func openBackupFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("备份只接受普通文件：%s", name)
	}
	return f, info, nil
}

type backupReader struct {
	ctx context.Context
	io.Reader
}

func (r backupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func transferBackupFile(ctx context.Context, source *os.Root, name string, dest *os.Root, target string) (backupFile, error) {
	entry := backupFile{Path: target}
	f, before, err := openBackupFile(source, name)
	if err != nil {
		return entry, err
	}
	defer f.Close()
	hash := sha256.New()
	var writer io.Writer = hash
	var output *os.File
	if dest != nil {
		output, err = dest.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return entry, err
		}
		writer = io.MultiWriter(hash, output)
	}
	size, copyErr := io.Copy(writer, backupReader{ctx, io.LimitReader(f, before.Size()+1)})
	if output != nil {
		copyErr = errors.Join(copyErr, output.Sync(), output.Close())
	}
	if copyErr != nil {
		return entry, copyErr
	}
	after, err := source.Lstat(name)
	if err != nil || size != before.Size() || !os.SameFile(before, after) || fileVersion(before) != fileVersion(after) {
		return entry, fmt.Errorf("备份期间文件发生变化：%s", name)
	}
	entry.Bytes, entry.Digest = size, hex.EncodeToString(hash.Sum(nil))
	return entry, nil
}

func loadBackupManifest(source *os.Root) (backupManifest, error) {
	var manifest backupManifest
	file, info, err := openBackupFile(source, backupManifestName)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	if info.Size() > 16*1024*1024 {
		return manifest, errors.New("备份清单过大")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16*1024*1024+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("备份清单包含额外内容")
	}
	if (manifest.Version != 1 && manifest.Version != 2) || len(manifest.Files) > 10000 ||
		(manifest.Version == 1 && (len(manifest.WorkspaceID) != 64 || len(manifest.Accounts) != 0)) ||
		(manifest.Version == 2 && (manifest.WorkspaceID != "" || len(manifest.Accounts) > 10000)) {
		return manifest, errors.New("不支持的备份清单")
	}
	minSchema := 125
	if manifest.Version == 2 {
		minSchema = 127
	}
	legacyLocalSchema := manifest.Version == 1 && (manifest.Schema == modelProtocolMigrationFrom || manifest.Schema == modelKeyMigrationFrom)
	if !legacyLocalSchema {
		if err = requireBackupSchema(manifest.Schema, minSchema); err != nil {
			return manifest, err
		}
	}
	accounts := make(map[string]bool, len(manifest.Accounts))
	for _, account := range manifest.Accounts {
		if !validBackupAccountID(account.ID) || accounts[account.ID] || len(account.WorkspaceID) != 64 {
			return manifest, errors.New("无效的备份账号")
		}
		if err = requireBackupSchema(account.Schema, 127); err != nil {
			return manifest, err
		}
		accounts[account.ID] = true
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		allowed := file.Path == backupDB || strings.HasPrefix(file.Path, "workspace/materials/") || strings.HasPrefix(file.Path, "workspace/outputs/") || validUploadPath(file.Path)
		if manifest.Version == 2 {
			allowed = file.Path == backupDB || validServerBackupPath(file.Path, accounts)
		}
		if !allowed || !fs.ValidPath(file.Path) || len(file.Path) > 1024 || seen[file.Path] || file.Bytes < 0 || len(file.Digest) != 64 {
			return manifest, fmt.Errorf("无效的备份条目：%s", file.Path)
		}
		seen[file.Path] = true
	}
	if !seen[backupDB] {
		return manifest, errors.New("备份清单缺少数据库")
	}
	for id := range accounts {
		if !seen[path.Join("account-data", id, backupDB)] {
			return manifest, errors.New("备份清单缺少个人数据库")
		}
	}
	return manifest, nil
}

// Personal-storage and model-catalog migrations support earlier snapshots of
// these formats. Open upgrades the copy during restore or server startup.
func requireBackupSchema(version, minimum int) error {
	if version >= minimum && version <= databaseSchemaVersion {
		return nil
	}
	return requireDatabaseVersion(version)
}

func sqliteFileURI(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

func snapshotDatabase(ctx context.Context, source, target string) (int, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("数据目录中的数据库须为普通文件")
	}
	db, err := sql.Open("sqlite", sqliteFileURI(source, "mode=ro"))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if err = requireBackupSchema(version, 125); err != nil {
		return 0, err
	}
	if err = verifyWritesSettled(ctx, db); err != nil {
		return 0, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	err = conn.Raw(func(driver any) error {
		snapshot, err := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup(sqliteFileURI(target, ""))
		if err != nil {
			return err
		}
		for ctx.Err() == nil {
			more, err := snapshot.Step(128)
			if err != nil || !more {
				return errors.Join(err, snapshot.Finish())
			}
		}
		return errors.Join(ctx.Err(), snapshot.Finish())
	})
	if err != nil {
		return 0, err
	}
	if err = os.Chmod(target, 0600); err != nil {
		return 0, err
	}
	file, err := os.Open(target)
	if err != nil {
		return 0, err
	}
	return version, errors.Join(file.Sync(), file.Close())
}

func verifyWritesSettled(ctx context.Context, db *sql.DB) error {
	var unsettled bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM publications WHERE status IN ('dispatched','unknown'))").Scan(&unsettled); err != nil {
		return err
	}
	if unsettled {
		return errors.New("仍有待核对的工作区写入。请在原工作台核对后再备份或恢复")
	}
	return nil
}
