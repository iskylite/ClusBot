package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"clusbot/internal/work"
)

const accountInitializing = ".desktop-initializing" // Existing on-disk first-launch marker; shared by both launchers.

var errUnownedData = errors.New("数据目录含有未归属账号的旧版单用户数据，已拒绝启动。请停止旧版并保留完整原目录；可尝试用 backup 另作备份。可用 --data-dir 指向新的空目录开始账号初始化，旧数据须明确迁移到指定账号")

// openAccountInstance requires the caller to hold the data directory lock.
// Inspect a pre-existing database without invoking read-write schema setup.
func openAccountInstance(ctx context.Context, dir string) (*work.Store, error) {
	path := filepath.Join(dir, "clusbot.db")
	pending, err := prepareAccountData(ctx, dir, path)
	if err != nil {
		return nil, err
	}
	db, err := work.Open(path)
	if err != nil {
		return nil, err
	}
	if err = db.EnsureAccountMode(ctx, true); err == nil && pending {
		err = os.Remove(filepath.Join(dir, accountInitializing))
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func prepareAccountData(ctx context.Context, dir, database string) (bool, error) {
	dbInfo, err := os.Lstat(database)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if dbInfo != nil && !dbInfo.Mode().IsRegular() {
		return false, errors.New("主数据库必须是普通文件")
	}
	// These were previously used as shared business data. Never move them,
	// attach them to a first user, or read a shared login secret.
	for _, name := range []string{"workspace", "uploads"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return false, errUnownedData
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	marker := filepath.Join(dir, accountInitializing)
	markerInfo, err := os.Lstat(marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if dbInfo != nil && markerInfo == nil {
		return false, requireAccountInstance(ctx, database)
	}
	// A first-time initialization can only claim its own hard-linked DB
	// inode, never a replacement or a database found next to account files.
	for _, name := range []string{"access-token", "clusbot.db-wal", "clusbot.db-shm", "clusbot.db-journal", "account-data", "connection-key"} {
		// SQLite may have created its own journal alongside the hard-linked
		// database before an interrupted initialization. Resume that inode.
		if dbInfo != nil && (name == "clusbot.db-wal" || name == "clusbot.db-shm" || name == "clusbot.db-journal") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			if name == "access-token" {
				return false, errUnownedData
			}
			return false, fmt.Errorf("发现残留数据 %s，但缺少可确认的账号实例；请备份并恢复正确的数据目录", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if markerInfo == nil {
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return false, err
		}
		markerInfo, err = f.Stat()
		if err = errors.Join(err, f.Close()); err != nil {
			return false, err
		}
	}
	if !markerInfo.Mode().IsRegular() {
		return false, errors.New("账号初始化标记不是普通文件")
	}
	if dbInfo == nil {
		if err := os.Link(marker, database); err != nil {
			return false, fmt.Errorf("准备首次初始化数据库：%w", err)
		}
	} else if !os.SameFile(markerInfo, dbInfo) {
		return false, errors.New("账号初始化标记与数据库不匹配；不会初始化或改动原数据库")
	}
	return true, nil
}

func requireAccountInstance(ctx context.Context, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	hasRows := func(table string) (bool, error) {
		var present bool
		err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", table).Scan(&present)
		if err != nil || !present {
			return false, err
		}
		// Table names are fixed literals from the lists below.
		err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+")").Scan(&present)
		return present, err
	}
	for _, table := range []string{"account_business_owner", "conversations", "tasks", "runs", "messages", "artifacts", "publications", "materials", "uploads", "memories", "task_acceptances", "general_preferences", "goals"} {
		nonempty, err := hasRows(table)
		if err != nil {
			return fmt.Errorf("无法只读检查原数据库，请先备份：%w", err)
		}
		if nonempty {
			if table == "account_business_owner" {
				return errors.New("指定目录属于单个账号而非账号实例，请指向上级实例目录或从备份恢复")
			}
			return errUnownedData
		}
	}
	for _, table := range []string{"account_instance", "account_bootstrap", "accounts"} {
		nonempty, err := hasRows(table)
		if err != nil {
			return fmt.Errorf("无法只读检查原数据库，请先备份：%w", err)
		}
		if nonempty {
			return nil
		}
	}
	// An unmarked database has unknown ownership. Even an apparently empty
	// one may contain older settings not represented by these tables.
	return errUnownedData
}

func connectionCredentialKey(dir string) (string, error) {
	path := filepath.Join(dir, "connection-key")
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("连接凭据校验密钥须为普通文件")
		}
		data, err := io.ReadAll(io.LimitReader(file, 128))
		if err != nil {
			return "", err
		}
		key := strings.TrimSpace(string(data))
		decoded, err := hex.DecodeString(key)
		if err != nil || len(decoded) != 32 || len(data) == 128 {
			return "", errors.New("连接凭据校验密钥文件无效；不会自动替换")
		}
		if err := file.Chmod(0600); err != nil {
			return "", err
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	rand.Read(raw[:])
	key := hex.EncodeToString(raw[:])
	file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	if _, err = io.WriteString(file, key+"\n"); err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	return key, nil
}
