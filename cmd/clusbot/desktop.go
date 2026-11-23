package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"clusbot/internal/work"
)

const desktopInitializing = ".desktop-initializing"

// A previous local installation has no account owner for its business data.
// Keep using that installation as-is; never silently assign it to a new user.
func desktopExistingData(dir string) (bool, error) {
	exists := func(name string) (bool, error) {
		_, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	database, err := exists("clusbot.db")
	if err != nil {
		return false, err
	}
	if database {
		return true, nil
	}
	for _, name := range []string{"account-data", "connection-key"} {
		found, err := exists(name)
		if err != nil {
			return false, err
		}
		if found {
			return false, errors.New("发现已有账号数据，但主数据库缺失；不会重新初始化")
		}
	}
	for _, name := range []string{"access-token", "workspace", "uploads"} {
		found, err := exists(name)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// Keep a link to a new database until its account mode is persisted. If the
// first launch is interrupted, the link identifies that same database on
// retry without treating an unrelated, restored database as a new install.
func desktopPrepareData(dir string) (bool, error) {
	existing, err := desktopExistingData(dir)
	if err != nil {
		return false, err
	}
	marker := filepath.Join(dir, desktopInitializing)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		if existing {
			return true, nil
		}
		file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if createErr != nil {
			return false, createErr
		}
		info, err = file.Stat()
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("首次初始化标记不是普通文件；不会自动初始化")
	}
	for _, name := range []string{"access-token", "workspace", "uploads", "account-data", "connection-key"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return false, errors.New("首次初始化标记与已有数据冲突；不会自动初始化")
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	database := filepath.Join(dir, "clusbot.db")
	dbInfo, err := os.Lstat(database)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Link(marker, database); err != nil {
			return false, fmt.Errorf("准备首次初始化数据库: %w", err)
		}
	} else if err != nil {
		return false, err
	} else if !os.SameFile(info, dbInfo) {
		return false, errors.New("首次初始化标记与数据库不匹配；不会自动初始化")
	}
	return false, nil
}

func desktopFinishInitialization(dir string) error {
	err := os.Remove(filepath.Join(dir, desktopInitializing))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func desktopDataMode(ctx context.Context, db *work.Store, existing bool, dir string) (string, error) {
	if !existing {
		return "server", nil
	}
	if err := db.EnsureAccountMode(ctx, false); errors.Is(err, work.ErrServerDatabase) {
		return "server", nil
	} else if err != nil {
		return "", err
	}
	for _, name := range []string{"account-data", "connection-key"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return "", errors.New("发现已有账号数据，但主数据库与账号数据不匹配；不会重新初始化")
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "local", nil
}
