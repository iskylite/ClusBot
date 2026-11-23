package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
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

type SettingsInput struct {
	work.ModelSettings
	APIKey *string `json:"api_key"`
}

type SettingsView struct {
	work.ModelSettings
	DefaultSource    string `json:"default_source,omitempty"`
	DefaultProfileID string `json:"default_profile_id,omitempty"`
	Mode             string `json:"mode"`
	BaseURLSource    string `json:"base_url_source,omitempty"`
	ModelIDSource    string `json:"model_id_source,omitempty"`
	LocalKeySaved    bool   `json:"local_key_saved"`
	KeyConfigured    bool   `json:"key_configured"`
	// EffectiveContextTokens is the window compaction actually enforces: the
	// configured value, or the default applied when it is unknown. The client
	// uses it to show context usage against the same limit the backend uses.
	EffectiveContextTokens int `json:"effective_context_tokens"`
}

type InvalidSettings string

func (e InvalidSettings) Error() string { return string(e) }

var keyReference = regexp.MustCompile(`^env:[A-Za-z_][A-Za-z0-9_]{0,79}$`)

func (s *Settings) Initialize(ctx context.Context, initial work.ModelSettings) (SettingsView, error) {
	value, err := s.Store.ModelSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		initial = cleanSettings(initial)
		if initial.Protocol == "" {
			initial.Protocol = ProtocolResponses
		}
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
	return s.View(ctx)
}

func (s *Settings) View(ctx context.Context) (SettingsView, error) {
	value, selected, err := s.resolveDefault(ctx)
	if errors.Is(err, work.ErrModelDefaultUnavailable) {
		return SettingsView{Mode: "unavailable", DefaultSource: "profile", DefaultProfileID: selected.ProfileID}, nil
	}
	if err != nil {
		return SettingsView{}, err
	}
	view := s.viewResolved(value, selected.ProfileID == "")
	view.DefaultSource = "legacy_default"
	if selected.ProfileID != "" {
		view.DefaultSource, view.DefaultProfileID = "profile", selected.ProfileID
		view.KeyRef = ""
		if !credentialAvailable(value) {
			view.Mode = "unavailable"
		}
	}
	return view, nil
}

func (s *Settings) Save(ctx context.Context, draft SettingsInput) (SettingsView, error) {
	selection, err := s.Store.ModelDefault(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	if selection.ProfileID != "" {
		return SettingsView{}, InvalidSettings("当前默认模型来自目录，请在目录中编辑或先切换为旧默认")
	}
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	if stored.Revision != draft.Revision {
		return SettingsView{}, work.ErrModelSettingsRevision
	}
	input := settingsWithKey(stored, draft)
	if input.Protocol == "" {
		input.Protocol = stored.Protocol
	}
	if s.BaseURLOverride != "" {
		input.BaseURL = stored.BaseURL
	}
	if s.ModelIDOverride != "" {
		input.ModelID = stored.ModelID
	}
	if draft.Attributes == nil && input.BaseURL == stored.BaseURL &&
		input.ModelID == stored.ModelID && input.Protocol == stored.Protocol {
		input.Attributes = stored.Attributes
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

func (s *Settings) Test(ctx context.Context, draft SettingsInput) (time.Duration, error) {
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return 0, err
	}
	value := s.effective(settingsWithKey(stored, draft))
	if value.Protocol == "" {
		value.Protocol = stored.Protocol
	}
	return s.testModelConfig(ctx, value)
}

func (s *Settings) testModelConfig(ctx context.Context, value work.ModelSettings) (time.Duration, error) {
	return s.testModelConfigWithKey(ctx, value, keyValue(value))
}

func (s *Settings) testModelConfigWithKey(ctx context.Context, value work.ModelSettings, key string) (time.Duration, error) {
	validate := validateSettings
	if value.KeyRef == "none" {
		validate = validateProfileSettings
	}
	if err := validate(value); err != nil {
		return 0, err
	}
	if value.ModelID == "" {
		return 0, InvalidSettings("请填写模型 ID")
	}
	provider, err := NewProvider(value.BaseURL, value.ModelID, key, value.Protocol)
	if err != nil {
		return 0, err
	}
	provider.maxRequestBytes = s.MaxRequestBytes
	if value.Attributes != nil {
		provider.attributes = *value.Attributes
	}
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
	provider, err := s.provider(ctx)
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
	if !task {
		reply, binding, explicit, err := s.conversationReplyProvider(ctx)
		if err != nil || explicit {
			return reply, binding, err
		}
	}
	provider, err := s.provider(ctx)
	if err != nil {
		return nil, "", err
	}
	binding, _ := provider.ReadCheckpointIdentity()
	return provider, binding + "\n" + kind, nil
}

func (s *Settings) SkillReplayModel(ctx context.Context) (work.Provider, work.ExperienceEvaluationConfig, error) {
	provider, err := s.provider(ctx)
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
	provider, err := s.provider(ctx)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (s *Settings) MultiTaskReadModel(ctx context.Context) (work.MultiTaskReadModel, error) {
	if s.Demo {
		return nil, nil
	}
	provider, err := s.provider(ctx)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (s *Settings) provider(ctx context.Context) (*Provider, error) {
	value, selected, err := s.resolveDefault(ctx)
	if err != nil {
		return nil, err
	}
	key := keyValue(value)
	if selected.ProfileID != "" {
		if err := validateProfileSettings(value); err != nil {
			return nil, err
		}
		if !credentialAvailableWithKey(value, key) {
			return nil, InvalidSettings("默认模型凭据尚不可用")
		}
	}
	return s.providerForValue(value, key)
}

func (s *Settings) resolveDefault(ctx context.Context) (work.ModelSettings, work.ModelDefaultReference, error) {
	value, selected, err := s.Store.EffectiveModelSettings(ctx)
	if err == nil && selected.ProfileID == "" {
		value = s.effective(value)
	}
	return value, selected, err
}

func (s *Settings) providerForValue(value work.ModelSettings, key string) (*Provider, error) {
	provider, err := NewProvider(value.BaseURL, value.ModelID, key, value.Protocol)
	if err != nil {
		return nil, err
	}
	provider.configRevision = value.Revision
	provider.credentialBinding = s.Store.ModelCredentialBinding(key)
	provider.maxRequestBytes = s.MaxRequestBytes
	if value.Attributes != nil {
		provider.attributes = *value.Attributes
	}
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
	return s.viewResolved(s.effective(value), true)
}

func (s *Settings) viewResolved(value work.ModelSettings, legacy bool) SettingsView {
	view := SettingsView{ModelSettings: value, Mode: "unconfigured", LocalKeySaved: value.APIKey != "", KeyConfigured: keyValue(value) != ""}
	if s.Demo {
		view.Mode = "demo"
	} else if value.ModelID != "" {
		view.Mode = value.Protocol
	}
	if legacy && s.BaseURLOverride != "" {
		view.BaseURLSource = "CLUSBOT_MODEL_BASE_URL"
	}
	if legacy && s.ModelIDOverride != "" {
		view.ModelIDSource = "CLUSBOT_MODEL_ID"
	}
	// Report the window compaction enforces. A configured value always wins; the
	// default only stands in once a real model is configured, so demo/unconfigured
	// modes report 0 and the client omits the usage bar there.
	if value.Attributes != nil && value.Attributes.ContextTokens != nil {
		view.EffectiveContextTokens = *value.Attributes.ContextTokens
	} else if view.Mode == ProtocolResponses || view.Mode == ProtocolChat {
		view.EffectiveContextTokens = DefaultContextTokens
	}
	return view
}

func settingsWithKey(stored work.ModelSettings, draft SettingsInput) work.ModelSettings {
	value := cleanSettings(draft.ModelSettings)
	value.APIKey = stored.APIKey
	if draft.APIKey != nil {
		value.APIKey = strings.TrimSpace(*draft.APIKey)
	}
	return value
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
	if _, err := modelEndpoint(value.BaseURL, value.Protocol); err != nil {
		return InvalidSettings(err.Error())
	}
	if len(value.ModelID) > 256 {
		return InvalidSettings("模型 ID 最多 256 字节")
	}
	if len(value.APIKey) > 4096 || strings.ContainsAny(value.APIKey, "\r\n\x00") {
		return InvalidSettings("API Key 必须为单行且最多 4096 字节")
	}
	if a := value.Attributes; a != nil {
		if a.ContextTokens != nil && *a.ContextTokens <= 0 {
			return InvalidSettings("上下文窗口必须是正整数")
		}
		if a.MaxOutputTokens != nil && *a.MaxOutputTokens <= 0 {
			return InvalidSettings("最大输出长度必须是正整数")
		}
		if a.ReasoningEffort != "" && !slices.Contains(a.ReasoningLevels, a.ReasoningEffort) {
			return InvalidSettings("请选择可用的推理强度档位")
		}
		if len(a.ReasoningLevels) > 16 || (a.Reasoning != nil && !*a.Reasoning && len(a.ReasoningLevels) > 0) {
			return InvalidSettings("推理强度档位与模型属性不匹配")
		}
		for _, level := range a.ReasoningLevels {
			if level == "" || len(level) > 32 || strings.TrimSpace(level) != level {
				return InvalidSettings("推理强度档位必须为 1 至 32 字节")
			}
		}
		for key, source := range a.Sources {
			switch key {
			case "context_tokens", "max_output_tokens", "vision", "file_input", "tool_calling", "structured_output", "reasoning", "reasoning_levels", "reasoning_effort":
			default:
				return InvalidSettings("模型属性来源无效")
			}
			if source != "provider" && source != "manual" {
				return InvalidSettings("模型属性来源无效")
			}
		}
	}
	if !keyReference.MatchString(value.KeyRef) {
		return InvalidSettings("密钥引用格式为 env:环境变量名")
	}
	return nil
}

func keyValue(value work.ModelSettings) string {
	if value.APIKey != "" {
		return value.APIKey
	}
	if value.KeyRef == "none" || value.KeyRef == work.ModelProviderAPIKeyRef {
		return ""
	}
	return strings.TrimSpace(os.Getenv(strings.TrimPrefix(value.KeyRef, "env:")))
}
