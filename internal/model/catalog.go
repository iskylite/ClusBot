package model

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clusbot/internal/work"
)

type ProviderInput struct {
	Name     string  `json:"name"`
	Protocol string  `json:"protocol"`
	BaseURL  string  `json:"base_url"`
	AuthMode string  `json:"auth_mode"`
	KeyRef   string  `json:"api_key_ref,omitempty"`
	APIKey   *string `json:"api_key,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
	Revision int     `json:"revision"`
}

type ProviderView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Protocol         string `json:"protocol"`
	BaseURL          string `json:"base_url"`
	AuthMode         string `json:"auth_mode"`
	CredentialStatus string `json:"credential_status"`
	Enabled          bool   `json:"enabled"`
	Revision         int    `json:"revision"`
	ModelCount       int    `json:"model_count"`
}

func providerSettings(p work.ModelProvider) work.ModelSettings {
	return work.ModelSettings{Protocol: p.Protocol, BaseURL: p.BaseURL, KeyRef: p.KeyRef, APIKey: p.APIKey, ModelID: "validation"}
}

func providerAuthMode(p work.ModelProvider) string {
	switch p.KeyRef {
	case "none", work.ModelProviderAPIKeyRef:
		return p.KeyRef
	default:
		return "environment"
	}
}

func providerView(p work.ModelProvider) ProviderView {
	v := ProviderView{ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL, Enabled: p.Enabled, Revision: p.Revision, ModelCount: p.ModelCount, AuthMode: providerAuthMode(p), CredentialStatus: "missing"}
	if p.KeyRef == "none" {
		v.CredentialStatus = "not_required"
	} else if keyValue(providerSettings(p)) != "" {
		v.CredentialStatus = "configured"
	}
	return v
}

func validCatalogName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 60 && !strings.ContainsFunc(name, unicode.IsControl)
}

func (s *Settings) Providers(ctx context.Context) ([]ProviderView, error) {
	if s == nil || s.Store == nil {
		return nil, InvalidSettings("模型设置尚不可用")
	}
	items, err := s.Store.ModelProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderView, 0, len(items))
	for _, p := range items {
		out = append(out, providerView(p))
	}
	return out, nil
}

func (s *Settings) SaveProvider(ctx context.Context, id string, in ProviderInput) (ProviderView, error) {
	if s == nil || s.Store == nil {
		return ProviderView{}, InvalidSettings("模型设置尚不可用")
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validCatalogName(in.Name) || (id == "" && in.Revision != 0) || (id != "" && in.Revision < 1) {
		return ProviderView{}, InvalidSettings("提供方名称或版本无效")
	}
	p := work.ModelProvider{ID: id, Name: in.Name, Revision: in.Revision, Protocol: in.Protocol, BaseURL: strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"), KeyRef: strings.TrimSpace(in.KeyRef), Enabled: true}
	if id != "" {
		prev, err := s.Store.ReadModelProvider(ctx, id)
		if err != nil {
			return ProviderView{}, err
		}
		if prev.Revision != in.Revision {
			return ProviderView{}, work.ErrModelSettingsRevision
		}
		p.Enabled = prev.Enabled
		if in.AuthMode == "" {
			in.AuthMode = providerAuthMode(prev)
		}
		if in.AuthMode == "environment" && p.KeyRef == "" && providerAuthMode(prev) == "environment" {
			if p.BaseURL != prev.BaseURL {
				return ProviderView{}, InvalidSettings("更换服务地址须重新明确输入密钥环境变量引用")
			}
			p.KeyRef = prev.KeyRef
		}
		if in.AuthMode == work.ModelProviderAPIKeyRef && in.APIKey == nil && prev.KeyRef == work.ModelProviderAPIKeyRef {
			if p.BaseURL != prev.BaseURL || p.Protocol != prev.Protocol {
				return ProviderView{}, InvalidSettings("更换服务地址或协议须重新输入 API Key")
			}
			p.APIKey = prev.APIKey
		}
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.AuthMode != work.ModelProviderAPIKeyRef && in.APIKey != nil {
		return ProviderView{}, InvalidSettings("仅直接 API Key 模式接受 API Key")
	}
	switch in.AuthMode {
	case "none":
		if p.KeyRef != "" {
			return ProviderView{}, InvalidSettings("无凭据模式不接受密钥引用")
		}
		p.KeyRef = "none"
	case "environment":
		if !keyReference.MatchString(p.KeyRef) {
			return ProviderView{}, InvalidSettings("请填写密钥环境变量引用")
		}
	case work.ModelProviderAPIKeyRef:
		if p.KeyRef != "" {
			return ProviderView{}, InvalidSettings("直接 API Key 模式不接受密钥环境变量引用")
		}
		if in.APIKey != nil {
			p.APIKey = *in.APIKey
		}
		if strings.TrimSpace(p.APIKey) == "" {
			return ProviderView{}, InvalidSettings("请填写 API Key")
		}
		p.KeyRef = work.ModelProviderAPIKeyRef
	default:
		return ProviderView{}, InvalidSettings("请选择环境变量、直接 API Key 或本机无凭据模式")
	}
	if err := validateProfileSettings(providerSettings(p)); err != nil {
		return ProviderView{}, err
	}
	out, err := s.Store.SaveModelProvider(ctx, p)
	if err != nil {
		return ProviderView{}, err
	}
	return providerView(out), nil
}

func (s *Settings) DeleteProvider(ctx context.Context, id string, revision int) error {
	if s == nil || s.Store == nil || revision < 1 {
		return InvalidSettings("提供方版本无效")
	}
	return s.Store.DeleteModelProvider(ctx, id, revision)
}

type CatalogModelInput struct {
	Name             string                `json:"name"`
	ProviderID       string                `json:"provider_id"`
	ProviderRevision int                   `json:"provider_revision"`
	ModelID          string                `json:"model_id"`
	Attributes       *work.ModelAttributes `json:"attributes,omitempty"`
	Enabled          *bool                 `json:"enabled,omitempty"`
	Revision         int                   `json:"revision"`
}

func (s *Settings) SaveCatalogModel(ctx context.Context, id string, in CatalogModelInput) (ProfileView, error) {
	if s == nil || s.Store == nil {
		return ProfileView{}, InvalidSettings("模型设置尚不可用")
	}
	in.Name, in.ModelID = strings.TrimSpace(in.Name), strings.TrimSpace(in.ModelID)
	if !validCatalogName(in.Name) || in.ModelID == "" || in.ProviderID == "" || in.ProviderRevision < 1 || (id == "" && in.Revision != 0) || (id != "" && in.Revision < 1) {
		return ProfileView{}, InvalidSettings("模型名称、提供方、模型 ID 或版本无效")
	}
	provider, err := s.Store.ReadModelProvider(ctx, in.ProviderID)
	if err != nil {
		return ProfileView{}, err
	}
	if provider.Revision != in.ProviderRevision {
		return ProfileView{}, work.ErrModelSettingsRevision
	}
	p := work.ModelConnectionProfile{ID: id, Name: in.Name, ProviderID: in.ProviderID, ProviderRevision: in.ProviderRevision, Enabled: true, Settings: work.ModelSettings{Protocol: provider.Protocol, BaseURL: provider.BaseURL, KeyRef: provider.KeyRef, ModelID: in.ModelID, Attributes: in.Attributes, Revision: in.Revision}}
	if id != "" {
		prev, err := s.Store.ReadModelProfile(ctx, id)
		if err != nil {
			return ProfileView{}, err
		}
		if prev.Settings.Revision != in.Revision {
			return ProfileView{}, work.ErrModelSettingsRevision
		}
		p.Enabled = prev.Enabled
		if in.Attributes == nil && prev.ProviderID == in.ProviderID && prev.Settings.ModelID == in.ModelID {
			p.Settings.Attributes = prev.Settings.Attributes
		}
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if err := validateProfileSettings(p.Settings); err != nil {
		return ProfileView{}, err
	}
	out, err := s.Store.SaveCatalogModel(ctx, p)
	if err != nil {
		return ProfileView{}, err
	}
	return profileView(out), nil
}

type DefaultModelInput struct {
	Source          string `json:"source"`
	Revision        int    `json:"revision"`
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision int    `json:"profile_revision,omitempty"`
}

type DefaultModelView struct {
	Source          string `json:"source"`
	Revision        int    `json:"revision"`
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision int    `json:"profile_revision,omitempty"`
	Status          string `json:"status"`
}

func (s *Settings) DefaultModel(ctx context.Context) (DefaultModelView, error) {
	if s == nil || s.Store == nil {
		return DefaultModelView{}, InvalidSettings("模型设置尚不可用")
	}
	d, err := s.Store.ModelDefault(ctx)
	if err != nil {
		return DefaultModelView{}, err
	}
	out := DefaultModelView{Source: "legacy_default", Revision: d.Revision, Status: "selected"}
	if d.ProfileID == "" {
		return out, nil
	}
	out.Source, out.ProfileID = "profile", d.ProfileID
	p, err := s.Store.ReadModelProfile(ctx, d.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		out.Status = "profile_missing"
		return out, nil
	}
	if err != nil {
		return DefaultModelView{}, err
	}
	out.ProfileRevision = p.Settings.Revision
	switch {
	case !p.ProviderEnabled:
		out.Status = "provider_disabled"
	case !p.Enabled:
		out.Status = "profile_disabled"
	case validateProfileSettings(p.Settings) != nil:
		out.Status = "invalid_configuration"
	case !credentialAvailable(p.Settings):
		out.Status = "credential_missing"
	}
	return out, nil
}

func (s *Settings) SetDefaultModel(ctx context.Context, in DefaultModelInput) (DefaultModelView, error) {
	if s == nil || s.Store == nil || in.Revision < 0 {
		return DefaultModelView{}, InvalidSettings("默认模型版本无效")
	}
	switch in.Source {
	case "legacy_default":
		if in.ProfileID != "" || in.ProfileRevision != 0 {
			return DefaultModelView{}, InvalidSettings("旧默认不应包含模型档案")
		}
	case "profile":
		if in.ProfileID == "" || in.ProfileRevision < 1 {
			return DefaultModelView{}, InvalidSettings("请选择模型及其版本")
		}
		p, err := s.Store.ReadModelProfile(ctx, in.ProfileID)
		if err != nil {
			return DefaultModelView{}, err
		}
		if p.Settings.Revision != in.ProfileRevision {
			return DefaultModelView{}, work.ErrModelSettingsRevision
		}
		if !p.Enabled || !p.ProviderEnabled {
			return DefaultModelView{}, InvalidSettings("模型或提供方已停用")
		}
		if err := validateProfileSettings(p.Settings); err != nil {
			return DefaultModelView{}, err
		}
		if !credentialAvailable(p.Settings) {
			return DefaultModelView{}, InvalidSettings("模型的环境凭据尚不可用")
		}
	default:
		return DefaultModelView{}, InvalidSettings("请选择旧默认或目录模型")
	}
	_, err := s.Store.SetModelDefault(ctx, work.ModelDefaultReference{Revision: in.Revision, ProfileID: in.ProfileID}, in.ProfileRevision)
	if err != nil {
		return DefaultModelView{}, err
	}
	return s.DefaultModel(ctx)
}

func (s *Settings) providerForOperation(ctx context.Context, id string, revision int) (work.ModelProvider, error) {
	if s == nil || s.Store == nil || revision < 1 {
		return work.ModelProvider{}, InvalidSettings("提供方版本无效")
	}
	p, err := s.Store.ReadModelProvider(ctx, id)
	if err != nil {
		return work.ModelProvider{}, err
	}
	if p.Revision != revision {
		return work.ModelProvider{}, work.ErrModelSettingsRevision
	}
	if !p.Enabled {
		return work.ModelProvider{}, InvalidSettings("提供方已停用")
	}
	if err := validateProfileSettings(providerSettings(p)); err != nil {
		return work.ModelProvider{}, err
	}
	return p, nil
}

func (s *Settings) DiscoverProvider(ctx context.Context, id string, revision int) ([]DiscoveredModel, error) {
	p, err := s.providerForOperation(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	v := providerSettings(p)
	key := keyValue(v)
	if !credentialAvailableWithKey(v, key) {
		return nil, InvalidSettings("提供方凭据尚不可用")
	}
	return discoverModelValues(ctx, v, key)
}

type ProviderDiagnostic struct {
	Connection     string `json:"connection"`
	Authentication string `json:"authentication"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Revision       int    `json:"revision"`
}

func (s *Settings) DiagnoseProvider(ctx context.Context, id string, revision int) (ProviderDiagnostic, error) {
	p, err := s.providerForOperation(ctx, id, revision)
	if err != nil {
		return ProviderDiagnostic{}, err
	}
	out := ProviderDiagnostic{Connection: "not_checked", Authentication: "unknown", Revision: p.Revision}
	v := providerSettings(p)
	key := keyValue(v)
	if !credentialAvailableWithKey(v, key) {
		out.Authentication = "credential_missing"
		return out, nil
	}
	endpoint, err := apiEndpoint(v.BaseURL, "/models")
	if err != nil {
		return ProviderDiagnostic{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ProviderDiagnostic{}, err
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		out.Connection = "failed"
		return out, nil
	}
	defer resp.Body.Close()
	out.Connection, out.HTTPStatus = "reachable", resp.StatusCode
	if v.KeyRef == "none" {
		out.Authentication = "not_used"
	} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		out.Authentication = "rejected"
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Some compatible endpoints ignore credentials; a successful response
		// proves only that the supplied credential was not rejected.
		out.Authentication = "not_rejected"
	}
	return out, nil
}
