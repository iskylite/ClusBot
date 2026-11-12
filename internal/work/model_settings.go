package work

import (
	"context"
	"errors"
)

var ErrModelSettingsRevision = errors.New("模型设置已更新，请重新载入页面")

type ModelSettings struct {
	BaseURL  string `json:"base_url"`
	ModelID  string `json:"model_id"`
	KeyRef   string `json:"api_key_ref"`
	Revision int    `json:"revision"`
}

func (s *Store) InitModelSettings(ctx context.Context, initial ModelSettings) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_settings(id, base_url, model_id, api_key_ref, revision)
		VALUES (1, ?, ?, ?, 1) ON CONFLICT(id) DO NOTHING`, initial.BaseURL, initial.ModelID, initial.KeyRef)
	return err
}

func (s *Store) ModelSettings(ctx context.Context) (ModelSettings, error) {
	var value ModelSettings
	err := s.db.QueryRowContext(ctx, `
		SELECT base_url, model_id, api_key_ref, revision FROM model_settings WHERE id = 1`).
		Scan(&value.BaseURL, &value.ModelID, &value.KeyRef, &value.Revision)
	return value, err
}

func (s *Store) SaveModelSettings(ctx context.Context, value ModelSettings) (ModelSettings, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE model_settings SET base_url = ?, model_id = ?, api_key_ref = ?, revision = revision + 1
		WHERE id = 1 AND revision = ?`, value.BaseURL, value.ModelID, value.KeyRef, value.Revision)
	if err != nil {
		return ModelSettings{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return ModelSettings{}, err
	}
	if count != 1 {
		return ModelSettings{}, ErrModelSettingsRevision
	}
	value.Revision++
	return value, nil
}
