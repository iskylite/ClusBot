package work

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrModelSettingsRevision = errors.New("模型设置已更新，请重新载入页面")

const modelAttributesSchema = `CREATE TABLE IF NOT EXISTS model_attributes (
    id INTEGER PRIMARY KEY REFERENCES model_settings(id),
    attributes TEXT NOT NULL
)`

type ModelAttributes struct {
	ReasoningEffort  string   `json:"reasoning_effort,omitempty"`
	ContextTokens    *int     `json:"context_tokens,omitempty"`
	MaxOutputTokens  *int     `json:"max_output_tokens,omitempty"`
	Vision           *bool    `json:"vision,omitempty"`
	FileInput        *bool    `json:"file_input,omitempty"`
	ToolCalling      *bool    `json:"tool_calling,omitempty"`
	StructuredOutput *bool    `json:"structured_output,omitempty"`
	Reasoning        *bool    `json:"reasoning,omitempty"`
	ReasoningLevels  []string `json:"reasoning_levels,omitempty"`
	// Sources records where a value was last set. Older records have no
	// provenance; a reply probe does not establish capability support.
	Sources map[string]string `json:"sources,omitempty"`
}

type ModelSettings struct {
	Attributes *ModelAttributes `json:"attributes,omitempty"`
	APIKey     string           `json:"-"`
	Protocol   string           `json:"protocol"`
	BaseURL    string           `json:"base_url"`
	ModelID    string           `json:"model_id"`
	KeyRef     string           `json:"api_key_ref"`
	Revision   int              `json:"revision"`
}

func (s *Store) InitModelSettings(ctx context.Context, initial ModelSettings) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_settings(id, base_url, model_id, api_key_ref, protocol, revision)
		VALUES (1, ?, ?, ?, ?, 1) ON CONFLICT(id) DO NOTHING`, initial.BaseURL, initial.ModelID, initial.KeyRef, initial.Protocol)
	return err
}

func (s *Store) ModelSettings(ctx context.Context) (ModelSettings, error) {
	var value ModelSettings
	var attributes string
	err := s.db.QueryRowContext(ctx, `
		SELECT base_url, model_id, api_key_ref, protocol, revision, api_key, COALESCE((SELECT attributes FROM model_attributes WHERE id = 1), 'null') FROM model_settings WHERE id = 1`).
		Scan(&value.BaseURL, &value.ModelID, &value.KeyRef, &value.Protocol, &value.Revision, &value.APIKey, &attributes)
	if err == nil {
		err = json.Unmarshal([]byte(attributes), &value.Attributes)
	}
	return value, err
}

func (s *Store) SaveModelSettings(ctx context.Context, value ModelSettings) (ModelSettings, error) {
	attributes, err := json.Marshal(value.Attributes)
	if err != nil {
		return ModelSettings{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelSettings{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE model_settings SET base_url = ?, model_id = ?, api_key_ref = ?, protocol = ?, api_key = ?, revision = revision + 1
		WHERE id = 1 AND revision = ?`, value.BaseURL, value.ModelID, value.KeyRef, value.Protocol, value.APIKey, value.Revision)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_attributes(id, attributes) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET attributes = excluded.attributes`, string(attributes)); err != nil {
		return ModelSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelSettings{}, err
	}
	value.Revision++
	return value, nil
}
