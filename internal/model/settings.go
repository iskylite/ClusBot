package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"clusbot/internal/work"
)

type Settings struct {
	Store           *work.Store
	BaseURLOverride string
	ModelIDOverride string
	Demo            bool
	MaxRequestBytes int
}

type SettingsView struct {
	work.ModelSettings
	Mode          string `json:"mode"`
	BaseURLSource string `json:"base_url_source,omitempty"`
	ModelIDSource string `json:"model_id_source,omitempty"`
	KeyConfigured bool   `json:"key_configured"`
}

type InvalidSettings string

func (e InvalidSettings) Error() string { return string(e) }

var keyReference = regexp.MustCompile(`^env:[A-Za-z_][A-Za-z0-9_]{0,79}$`)

func (s *Settings) Initialize(ctx context.Context, initial work.ModelSettings) (SettingsView, error) {
	value, err := s.Store.ModelSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		initial = cleanSettings(initial)
		if err = validateSettings(initial); err != nil {
			return SettingsView{}, err
		}
		if err = s.Store.InitModelSettings(ctx, initial); err != nil {
			return SettingsView{}, err
		}
		value, err = s.Store.ModelSettings(ctx)
	}
	if err != nil {
		return SettingsView{}, err
	}
	if err = validateSettings(s.effective(value)); err != nil {
		return SettingsView{}, err
	}
	return s.view(value), nil
}

func (s *Settings) View(ctx context.Context) (SettingsView, error) {
	value, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	return s.view(value), nil
}

func (s *Settings) Save(ctx context.Context, input work.ModelSettings) (SettingsView, error) {
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	if stored.Revision != input.Revision {
		return SettingsView{}, work.ErrModelSettingsRevision
	}
	input = cleanSettings(input)
	if s.BaseURLOverride != "" {
		input.BaseURL = stored.BaseURL
	}
	if s.ModelIDOverride != "" {
		input.ModelID = stored.ModelID
	}
	if err = validateSettings(input); err != nil {
		return SettingsView{}, err
	}
	value, err := s.Store.SaveModelSettings(ctx, input)
	if err != nil {
		return SettingsView{}, err
	}
	return s.view(value), nil
}

func (s *Settings) Test(ctx context.Context, input work.ModelSettings) (time.Duration, error) {
	value := s.effective(cleanSettings(input))
	if err := validateSettings(value); err != nil {
		return 0, err
	}
	if value.ModelID == "" {
		return 0, InvalidSettings("请填写模型 ID")
	}
	provider, err := NewResponses(value.BaseURL, value.ModelID, keyValue(value))
	if err != nil {
		return 0, err
	}
	provider.maxRequestBytes = s.MaxRequestBytes
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	if err = provider.testCapabilities(ctx); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func (s *Settings) Stream(ctx context.Context, messages []work.Message, task bool, limit int, emit func(string) error) (*work.TokenUsage, error) {
	if s.Demo {
		return (Demo{}).Stream(ctx, messages, task, limit, emit)
	}
	provider, err := s.responses(ctx)
	if err != nil {
		return nil, err
	}
	return provider.Stream(ctx, messages, task, limit, emit)
}

func (s *Settings) TextRunProvider(ctx context.Context, task bool) (work.Provider, string, error) {
	kind := "conversation/v1"
	if task {
		kind = "task/v1"
	}
	if s.Demo {
		return Demo{}, "demo/" + kind, nil
	}
	provider, err := s.responses(ctx)
	if err != nil {
		return nil, "", err
	}
	binding, _ := provider.ReadCheckpointIdentity()
	return provider, binding + "\n" + kind, nil
}

func (s *Settings) SkillReplayModel(ctx context.Context) (work.Provider, work.ExperienceEvaluationConfig, error) {
	provider, err := s.responses(ctx)
	if err != nil {
		return nil, work.ExperienceEvaluationConfig{}, err
	}
	binding, _ := provider.ReadCheckpointIdentity()
	return provider, work.ExperienceEvaluationConfig{
		Model: provider.model, ModelBinding: fmt.Sprintf("%x", sha256.Sum256([]byte(binding))),
		MaxOutput: 1024, RequestTimeout: 30 * time.Second,
	}, nil
}

func (s *Settings) TaskReadModel(ctx context.Context) (work.TaskReadModel, error) {
	if s.Demo {
		return nil, nil
	}
	provider, err := s.responses(ctx)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (s *Settings) MultiTaskReadModel(ctx context.Context) (work.MultiTaskReadModel, error) {
	if s.Demo {
		return nil, nil
	}
	provider, err := s.responses(ctx)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (s *Settings) responses(ctx context.Context) (*Responses, error) {
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return nil, err
	}
	value := s.effective(stored)
	key := keyValue(value)
	provider, err := NewResponses(value.BaseURL, value.ModelID, key)
	if err != nil {
		return nil, err
	}
	provider.configRevision = value.Revision
	provider.credentialBinding = s.Store.ModelCredentialBinding(key)
	provider.maxRequestBytes = s.MaxRequestBytes
	return provider, nil
}

func (s *Settings) effective(value work.ModelSettings) work.ModelSettings {
	if s.BaseURLOverride != "" {
		value.BaseURL = s.BaseURLOverride
	}
	if s.ModelIDOverride != "" {
		value.ModelID = s.ModelIDOverride
	}
	return value
}

func (s *Settings) view(value work.ModelSettings) SettingsView {
	value = s.effective(value)
	view := SettingsView{ModelSettings: value, Mode: "unconfigured", KeyConfigured: keyValue(value) != ""}
	if s.Demo {
		view.Mode = "demo"
	} else if value.ModelID != "" {
		view.Mode = "responses"
	}
	if s.BaseURLOverride != "" {
		view.BaseURLSource = "CLUSBOT_MODEL_BASE_URL"
	}
	if s.ModelIDOverride != "" {
		view.ModelIDSource = "CLUSBOT_MODEL_ID"
	}
	return view
}

func cleanSettings(value work.ModelSettings) work.ModelSettings {
	value.BaseURL = strings.TrimRight(strings.TrimSpace(value.BaseURL), "/")
	value.ModelID = strings.TrimSpace(value.ModelID)
	value.KeyRef = strings.TrimSpace(value.KeyRef)
	return value
}

func validateSettings(value work.ModelSettings) error {
	if len(value.BaseURL) > 2048 {
		return InvalidSettings("服务地址最多 2048 字节")
	}
	if _, err := responseEndpoint(value.BaseURL); err != nil {
		return InvalidSettings(err.Error())
	}
	if len(value.ModelID) > 256 {
		return InvalidSettings("模型 ID 最多 256 字节")
	}
	if !keyReference.MatchString(value.KeyRef) {
		return InvalidSettings("密钥引用格式为 env:环境变量名")
	}
	return nil
}

func keyValue(value work.ModelSettings) string {
	return os.Getenv(strings.TrimPrefix(value.KeyRef, "env:"))
}
