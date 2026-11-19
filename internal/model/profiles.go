package model

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clusbot/internal/work"
)

type ProfileInput struct {
	Name       string                `json:"name"`
	Protocol   string                `json:"protocol"`
	BaseURL    string                `json:"base_url"`
	ModelID    string                `json:"model_id"`
	KeyRef     string                `json:"api_key_ref,omitempty"`
	Revision   int                   `json:"revision"`
	Attributes *work.ModelAttributes `json:"attributes,omitempty"`
}

type ProfileView struct {
	ID                   string                `json:"id"`
	Name                 string                `json:"name"`
	Protocol             string                `json:"protocol"`
	BaseURL              string                `json:"base_url"`
	ModelID              string                `json:"model_id"`
	Revision             int                   `json:"revision"`
	Attributes           *work.ModelAttributes `json:"attributes,omitempty"`
	CredentialConfigured bool                  `json:"credential_configured"`
}

func profileView(p work.ModelConnectionProfile) ProfileView {
	v := p.Settings
	return ProfileView{ID: p.ID, Name: p.Name, Protocol: v.Protocol, BaseURL: v.BaseURL, ModelID: v.ModelID,
		Revision: v.Revision, Attributes: v.Attributes, CredentialConfigured: keyValue(v) != ""}
}

func (s *Settings) Profiles(ctx context.Context) ([]ProfileView, error) {
	if s == nil || s.Store == nil {
		return nil, InvalidSettings("模型设置尚不可用")
	}
	stored, err := s.Store.ModelProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProfileView, 0, len(stored))
	for _, p := range stored {
		out = append(out, profileView(p))
	}
	return out, nil
}

func (s *Settings) SaveProfile(ctx context.Context, key string, draft ProfileInput) (ProfileView, error) {
	if s == nil || s.Store == nil {
		return ProfileView{}, InvalidSettings("模型设置尚不可用")
	}
	name := strings.TrimSpace(draft.Name)
	if name == "" || utf8.RuneCountInString(name) > 60 || strings.ContainsFunc(name, unicode.IsControl) ||
		(key == "" && draft.Revision != 0) || (key != "" && draft.Revision < 1) {
		return ProfileView{}, InvalidSettings("档案名称或版本无效")
	}
	v := cleanSettings(work.ModelSettings{Protocol: draft.Protocol, BaseURL: draft.BaseURL, ModelID: draft.ModelID,
		KeyRef: draft.KeyRef, Attributes: draft.Attributes, Revision: draft.Revision})
	if key != "" {
		prev, err := s.Store.ReadModelProfile(ctx, key)
		if err != nil {
			return ProfileView{}, err
		}
		if prev.Settings.Revision != draft.Revision {
			return ProfileView{}, work.ErrModelSettingsRevision
		}
		if v.KeyRef == "" {
			if v.BaseURL != prev.Settings.BaseURL {
				return ProfileView{}, InvalidSettings("更换服务地址须重新明确输入密钥环境变量引用")
			}
			v.KeyRef = prev.Settings.KeyRef
		}
		if v.Attributes == nil && v.BaseURL == prev.Settings.BaseURL && v.ModelID == prev.Settings.ModelID && v.Protocol == prev.Settings.Protocol {
			v.Attributes = prev.Settings.Attributes
		}
	}
	if v.ModelID == "" {
		return ProfileView{}, InvalidSettings("请填写模型 ID")
	}
	if err := validateProfileSettings(v); err != nil {
		return ProfileView{}, err
	}
	out, err := s.Store.SaveModelProfile(ctx, work.ModelConnectionProfile{ID: key, Name: name, Settings: v})
	if err != nil {
		return ProfileView{}, err
	}
	return profileView(out), nil
}

func (s *Settings) DeleteProfile(ctx context.Context, key string, expected int) error {
	if s == nil || s.Store == nil || expected < 1 {
		return InvalidSettings("模型设置或档案版本无效")
	}
	return s.Store.DeleteModelProfile(ctx, key, expected)
}

func (s *Settings) TestProfile(ctx context.Context, key string, expected int) (time.Duration, error) {
	if s == nil || s.Store == nil || expected < 1 {
		return 0, InvalidSettings("模型设置或档案版本无效")
	}
	p, err := s.Store.ReadModelProfile(ctx, key)
	if err != nil {
		return 0, err
	}
	if p.Settings.Revision != expected {
		return 0, work.ErrModelSettingsRevision
	}
	if err := validateProfileSettings(p.Settings); err != nil {
		return 0, err
	}
	// Do not apply default runtime overrides or import the default's key.
	return s.testModelConfig(ctx, p.Settings)
}

func validateProfileSettings(v work.ModelSettings) error {
	if err := validateSettings(v); err != nil {
		return err
	}
	u, err := url.Parse(v.BaseURL)
	if err != nil || u.Scheme != "https" && (u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback()) {
		return InvalidSettings("模型档案须使用 HTTPS，或使用回环 IP 的本地 HTTP")
	}
	return nil
}
