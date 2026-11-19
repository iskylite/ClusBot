package model

import (
	"context"
	"fmt"

	"clusbot/internal/work"
)

type BenchmarkAdmissionRoleInput struct {
	Action                   string `json:"action"`
	Revision                 int    `json:"revision"`
	ProfileID                string `json:"profile_id,omitempty"`
	ProfileRevision          int    `json:"profile_revision,omitempty"`
	ConfirmRequestTextEgress bool   `json:"confirm_request_text_egress,omitempty"`
}

type BenchmarkAdmissionRoleView struct {
	Role            string   `json:"role"`
	Revision        int      `json:"revision"`
	Source          string   `json:"source"`
	Status          string   `json:"status"`
	ProfileID       string   `json:"profile_id,omitempty"`
	ProfileRevision int      `json:"profile_revision,omitempty"`
	EgressFields    []string `json:"egress_fields,omitempty"`
}

func (s *Settings) modelRole(ctx context.Context, role string) (BenchmarkAdmissionRoleView, work.ModelSettings, string, error) {
	if s == nil || s.Store == nil {
		return BenchmarkAdmissionRoleView{}, work.ModelSettings{}, "", InvalidSettings("模型设置尚不可用")
	}
	b, p, err := s.Store.ReadModelRole(ctx, role)
	if err != nil {
		return BenchmarkAdmissionRoleView{}, work.ModelSettings{}, "", err
	}
	v := BenchmarkAdmissionRoleView{Role: b.Role, Revision: b.Revision, Source: "legacy_default", Status: "uses_existing_default"}
	if b.ProfileID == "" {
		return v, work.ModelSettings{}, "", nil
	}
	v.ProfileID, v.ProfileRevision, v.Source = b.ProfileID, b.ProfileRevision, "explicit_profile"
	switch role {
	case work.BenchmarkAdmissionModelRole:
		v.EgressFields = []string{"submitted_text", "published_target", "published_profile_and_version"}
	case work.BenchmarkRevisionModelRole:
		v.EgressFields = []string{"submitted_text", "editable_case_keys", "editable_parameter_keys"}
	}
	key := keyValue(p.Settings)
	switch {
	case s.Demo:
		v.Status = "demo_profile_disabled"
	case p.ID == "":
		v.Status = "profile_missing"
	case p.Settings.Revision != b.ProfileRevision:
		v.Status = "profile_revision_changed"
	case p.Settings.ModelID == "" || validateProfileSettings(p.Settings) != nil:
		v.Status = "profile_invalid"
	case key == "":
		v.Status = "credential_missing"
	case s.Store.ModelCredentialBinding(key) != b.CredentialBinding:
		v.Status = "credential_changed"
	default:
		v.Status = "locally_configured"
		return v, p.Settings, key, nil
	}
	return v, work.ModelSettings{}, "", nil
}

func (s *Settings) ReadBenchmarkAdmissionRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	view, _, _, err := s.modelRole(ctx, work.BenchmarkAdmissionModelRole)
	return view, err
}

func (s *Settings) SetBenchmarkAdmissionRole(ctx context.Context, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	return s.setModelRole(ctx, work.BenchmarkAdmissionModelRole, in)
}

func (s *Settings) ReadBenchmarkRevisionRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	view, _, _, err := s.modelRole(ctx, work.BenchmarkRevisionModelRole)
	return view, err
}

func (s *Settings) SetBenchmarkRevisionRole(ctx context.Context, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	return s.setModelRole(ctx, work.BenchmarkRevisionModelRole, in)
}

func (s *Settings) setModelRole(ctx context.Context, role string, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	if s == nil || s.Store == nil || in.Revision < 0 {
		return BenchmarkAdmissionRoleView{}, InvalidSettings("角色设置尚不可用或版本无效")
	}
	b := work.ModelRoleBinding{Role: role, Revision: in.Revision}
	switch in.Action {
	case "bind_profile":
		if s.Demo || in.ProfileID == "" || in.ProfileRevision < 1 || !in.ConfirmRequestTextEgress {
			return BenchmarkAdmissionRoleView{}, InvalidSettings("请选择档案精确版本并明确确认此入口的用户文本由该供应方处理")
		}
		p, err := s.Store.ReadModelProfile(ctx, in.ProfileID)
		if err != nil {
			return BenchmarkAdmissionRoleView{}, err
		}
		if p.Settings.Revision != in.ProfileRevision {
			return BenchmarkAdmissionRoleView{}, work.ErrModelSettingsRevision
		}
		if err := validateProfileSettings(p.Settings); err != nil {
			return BenchmarkAdmissionRoleView{}, err
		}
		key := keyValue(p.Settings)
		if key == "" {
			return BenchmarkAdmissionRoleView{}, InvalidSettings("档案的环境凭据尚不可用")
		}
		b.ProfileID, b.ProfileRevision, b.CredentialBinding = p.ID, p.Settings.Revision, s.Store.ModelCredentialBinding(key)
	case "use_existing_default":
		if in.ProfileID != "" || in.ProfileRevision != 0 || in.ConfirmRequestTextEgress {
			return BenchmarkAdmissionRoleView{}, InvalidSettings("选择现有默认时不要发送档案或出域参数")
		}
	default:
		return BenchmarkAdmissionRoleView{}, InvalidSettings("未知的角色配置操作")
	}
	if _, err := s.Store.SaveModelRole(ctx, b); err != nil {
		return BenchmarkAdmissionRoleView{}, err
	}
	view, _, _, err := s.modelRole(ctx, role)
	return view, err
}

func (s *Settings) benchmarkAdmissionProvider(ctx context.Context) (*Provider, bool, error) {
	v, value, key, err := s.modelRole(ctx, work.BenchmarkAdmissionModelRole)
	if err != nil {
		return nil, false, err
	}
	if v.Source == "legacy_default" {
		p, err := s.provider(ctx)
		return p, false, err
	}
	p, err := s.profileForModelRole(v, value, key, work.ErrBenchmarkAdmissionModelBinding)
	return p, true, err
}

func (s *Settings) profileForModelRole(v BenchmarkAdmissionRoleView, value work.ModelSettings, key string, unavailable error) (*Provider, error) {
	if v.Source != "explicit_profile" || v.Status != "locally_configured" {
		return nil, fmt.Errorf("%w（%s）", unavailable, v.Status)
	}
	p, err := s.providerForValue(value, key)
	if err != nil {
		return nil, unavailable
	}
	return p, nil
}
