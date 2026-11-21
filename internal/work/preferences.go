package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrPreferencesRevision = errors.New("偏好设置已更新，请重新载入页面")
	ErrInvalidPreferences  = errors.New("偏好设置无效")
)

// A preferences row belongs to the store: in server mode this is the
// authenticated account's private business database, not the instance store.
func migratePreferences(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS general_preferences (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		language TEXT NOT NULL,
		timezone TEXT NOT NULL,
		appearance TEXT NOT NULL,
		reduce_motion INTEGER NOT NULL CHECK (reduce_motion IN (0, 1)),
		send_shortcut TEXT NOT NULL,
		desktop_notifications INTEGER NOT NULL CHECK (desktop_notifications IN (0, 1)),
		revision INTEGER NOT NULL CHECK (revision > 0)
	)`)
	return err
}

type GeneralPreferences struct {
	Language             string `json:"language"`
	Timezone             string `json:"timezone"`
	Appearance           string `json:"appearance"`
	ReduceMotion         bool   `json:"reduce_motion"`
	SendShortcut         string `json:"send_shortcut"`
	DesktopNotifications bool   `json:"desktop_notifications"`
	Revision             int    `json:"revision"`
}

func DefaultGeneralPreferences() GeneralPreferences {
	return GeneralPreferences{
		Language:     "system",
		Timezone:     "system",
		Appearance:   "system",
		SendShortcut: "mod_enter",
	}
}

func validateGeneralPreferences(p GeneralPreferences) error {
	if p.Language != "system" && p.Language != "zh-CN" && p.Language != "en-US" {
		return fmt.Errorf("%w：语言不受支持", ErrInvalidPreferences)
	}
	if p.Timezone != "system" {
		if p.Timezone == "" || p.Timezone == "Local" || len(p.Timezone) > 128 {
			return fmt.Errorf("%w：时区无效", ErrInvalidPreferences)
		}
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("%w：时区无效", ErrInvalidPreferences)
		}
	}
	if p.Appearance != "system" && p.Appearance != "light" && p.Appearance != "dark" {
		return fmt.Errorf("%w：外观不受支持", ErrInvalidPreferences)
	}
	if p.SendShortcut != "enter" && p.SendShortcut != "mod_enter" {
		return fmt.Errorf("%w：发送快捷键不受支持", ErrInvalidPreferences)
	}
	if p.Revision < 0 {
		return fmt.Errorf("%w：修订号无效", ErrInvalidPreferences)
	}
	return nil
}

func (s *Store) GeneralPreferences(ctx context.Context) (GeneralPreferences, error) {
	var p GeneralPreferences
	err := s.db.QueryRowContext(ctx, `SELECT language, timezone, appearance, reduce_motion,
		send_shortcut, desktop_notifications, revision FROM general_preferences WHERE id = 1`).
		Scan(&p.Language, &p.Timezone, &p.Appearance, &p.ReduceMotion, &p.SendShortcut, &p.DesktopNotifications, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultGeneralPreferences(), nil
	}
	return p, err
}

// SaveGeneralPreferences replaces all fields only when the caller's revision
// still matches. Revision zero represents defaults which have not been saved.
func (s *Store) SaveGeneralPreferences(ctx context.Context, p GeneralPreferences) (GeneralPreferences, error) {
	if err := validateGeneralPreferences(p); err != nil {
		return GeneralPreferences{}, err
	}
	var result sql.Result
	var err error
	if p.Revision == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO general_preferences
			(id, language, timezone, appearance, reduce_motion, send_shortcut, desktop_notifications, revision)
			VALUES (1, ?, ?, ?, ?, ?, ?, 1) ON CONFLICT(id) DO NOTHING`,
			p.Language, p.Timezone, p.Appearance, p.ReduceMotion, p.SendShortcut, p.DesktopNotifications)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE general_preferences SET
			language = ?, timezone = ?, appearance = ?, reduce_motion = ?, send_shortcut = ?,
			desktop_notifications = ?, revision = revision + 1 WHERE id = 1 AND revision = ?`,
			p.Language, p.Timezone, p.Appearance, p.ReduceMotion, p.SendShortcut, p.DesktopNotifications, p.Revision)
	}
	if err != nil {
		return GeneralPreferences{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return GeneralPreferences{}, err
	}
	if n != 1 {
		return GeneralPreferences{}, ErrPreferencesRevision
	}
	p.Revision++
	return p, nil
}
