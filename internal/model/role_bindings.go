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

// ConversationReplyRoleInput explicitly covers richer context than Router text.
type ConversationReplyRoleInput struct {
	Action                           string `json:"action"`
	Revision                         int    `json:"revision"`
	ProfileID                        string `json:"profile_id,omitempty"`
	ProfileRevision                  int    `json:"profile_revision,omitempty"`
	ConfirmConversationContextEgress bool   `json:"confirm_conversation_context_egress,omitempty"`
}

type BenchmarkAdmissionRoleView struct {
	Role            string   `json:"role"`
	Revision        int      `json:"revision"`
	Source          string   `json:"source"`
	Status          string   `json:"status"`
	ProfileID       string   `json:"profile_id,omitempty"`
	ProfileRevision int      `json:"profile_revision,omitempty"`
	EgressFields    []string `json:"egress_fields,omitempty"`
	requestEnvelope *roleRequestEnvelope
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
	switch role {
	case work.GeneralRouterModelRole:
		v.EgressFields = []string{"submitted_text", "recent_history", "task_candidates", "pending_intents", "benchmark_candidate"}
	case work.ConversationReplyModelRole:
		v.EgressFields = []string{"submitted_text", "recent_history", "selected_text_materials", "selected_images_if_supported", "matching_user_memories"}
	case work.TaskWorkerModelRole:
		v.EgressFields = []string{"task_messages_history_and_requirements", "selected_text_materials", "selected_images_if_supported", "matching_user_memories",
			"workflow_input_artifacts", "applicable_skill_and_experience", "authorized_read_tool_definitions_and_results", "current_run_model_tool_round_history"}
	}
	if b.ProfileID == "" {
		return v, work.ModelSettings{}, "", nil
	}
	v.ProfileID, v.ProfileRevision, v.Source = b.ProfileID, b.ProfileRevision, "explicit_profile"
	switch role {
	case work.BenchmarkAdmissionModelRole:
		v.EgressFields = []string{"submitted_text", "published_target", "published_profile_and_version"}
	case work.BenchmarkRevisionModelRole:
		v.EgressFields = []string{"submitted_text", "editable_case_keys", "editable_parameter_keys"}
	case work.BenchmarkExecutionModelRole:
		v.EgressFields = []string{"submitted_text", "pending_case_keys", "pending_case_positions"}
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
		value := p.Settings
		if defaultDestinationRole(role) {
			base, same := s.roleDefaultDestination(ctx, value, key)
			if !same {
				v.Status = "default_destination_mismatch"
				return v, work.ModelSettings{}, "", nil
			}
			v.requestEnvelope = &roleRequestEnvelope{Model: base.ModelID}
			if base.Attributes != nil {
				v.requestEnvelope.Attributes = *base.Attributes
			}
			value = boundRouterProfile(value, base)
			if (role == work.ConversationReplyModelRole || role == work.TaskWorkerModelRole) &&
				(base.Attributes == nil || base.Attributes.Vision == nil || !*base.Attributes.Vision) {
				// Selecting another model cannot newly authorize egress of images
				// the effective default provider would have rejected locally.
				value.Attributes.Vision = nil
			}
		}
		v.Status = "locally_configured"
		return v, value, key, nil
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

func (s *Settings) ReadBenchmarkExecutionRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	view, _, _, err := s.modelRole(ctx, work.BenchmarkExecutionModelRole)
	return view, err
}

func (s *Settings) SetBenchmarkExecutionRole(ctx context.Context, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	return s.setModelRole(ctx, work.BenchmarkExecutionModelRole, in)
}

func (s *Settings) ReadGeneralRouterRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	view, _, _, err := s.modelRole(ctx, work.GeneralRouterModelRole)
	return view, err
}

func (s *Settings) SetGeneralRouterRole(ctx context.Context, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	return s.setModelRole(ctx, work.GeneralRouterModelRole, in)
}

func (s *Settings) ReadConversationReplyRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	view, _, _, err := s.modelRole(ctx, work.ConversationReplyModelRole)
	return view, err
}

func (s *Settings) SetConversationReplyRole(ctx context.Context, in ConversationReplyRoleInput) (BenchmarkAdmissionRoleView, error) {
	if in.Action == "bind_profile" && !in.ConfirmConversationContextEgress {
		return BenchmarkAdmissionRoleView{}, InvalidSettings("请先明确同意所选模型处理普通对话上下文：本次文字、近期对话、已选材料和可用图片、匹配的个人记忆")
	}
	return s.setModelRole(ctx, work.ConversationReplyModelRole, BenchmarkAdmissionRoleInput{
		Action: in.Action, Revision: in.Revision, ProfileID: in.ProfileID, ProfileRevision: in.ProfileRevision,
		ConfirmRequestTextEgress: in.ConfirmConversationContextEgress,
	})
}

func defaultDestinationRole(role string) bool {
	return role == work.GeneralRouterModelRole || role == work.ConversationReplyModelRole || role == work.TaskWorkerModelRole
}

// Ordinary routing/reply roles preserve the existing trusted destination.
func (s *Settings) roleDefaultDestination(ctx context.Context, profile work.ModelSettings, profileKey string) (work.ModelSettings, bool) {
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return work.ModelSettings{}, false
	}
	base := s.effective(stored)
	key := keyValue(base)
	if base.ModelID == "" || base.Protocol != profile.Protocol || base.KeyRef != profile.KeyRef ||
		base.APIKey != "" || key == "" || s.Store.ModelCredentialBinding(key) != s.Store.ModelCredentialBinding(profileKey) {
		return work.ModelSettings{}, false
	}
	baseEndpoint, err := modelEndpoint(base.BaseURL, base.Protocol)
	if err != nil {
		return work.ModelSettings{}, false
	}
	profileEndpoint, err := modelEndpoint(profile.BaseURL, profile.Protocol)
	return base, err == nil && baseEndpoint == profileEndpoint
}

// A profile must not raise the default routing-context or reply ceilings.
func boundRouterProfile(profile, base work.ModelSettings) work.ModelSettings {
	attributes := work.ModelAttributes{}
	if profile.Attributes != nil {
		attributes = *profile.Attributes
	}
	baseWindow := DefaultContextTokens
	if base.Attributes != nil {
		if base.Attributes.ContextTokens != nil {
			baseWindow = *base.Attributes.ContextTokens
		}
		if base.Attributes.MaxOutputTokens != nil &&
			(attributes.MaxOutputTokens == nil || *attributes.MaxOutputTokens > *base.Attributes.MaxOutputTokens) {
			attributes.MaxOutputTokens = base.Attributes.MaxOutputTokens
		}
	}
	if attributes.ContextTokens == nil || *attributes.ContextTokens > baseWindow {
		attributes.ContextTokens = &baseWindow
	}
	profile.Attributes = &attributes
	return profile
}

func (s *Settings) setModelRole(ctx context.Context, role string, in BenchmarkAdmissionRoleInput) (BenchmarkAdmissionRoleView, error) {
	if s == nil || s.Store == nil || in.Revision < 0 {
		return BenchmarkAdmissionRoleView{}, InvalidSettings("角色设置尚不可用或版本无效")
	}
	b := work.ModelRoleBinding{Role: role, Revision: in.Revision}
	switch in.Action {
	case "bind_profile":
		if role == work.GeneralRouterModelRole && !in.ConfirmRequestTextEgress {
			return BenchmarkAdmissionRoleView{}, InvalidSettings("请先明确同意所选模型处理常规路由上下文：本次文字、近期消息、候选任务、待澄清要求和基准候选")
		}
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
		if defaultDestinationRole(role) {
			if _, same := s.roleDefaultDestination(ctx, p.Settings, key); !same {
				return BenchmarkAdmissionRoleView{}, InvalidSettings("角色档案必须与有效默认模型使用同一协议、地址和环境凭据引用")
			}
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
	return s.inheritedModelRoleProvider(ctx, work.BenchmarkAdmissionModelRole, work.ErrBenchmarkAdmissionModelBinding)
}

// Resolve exactly one selection for this model call. Explicit bindings never
// try the default, including when a profile or credential no longer matches.
func (s *Settings) inheritedModelRoleProvider(ctx context.Context, role string, unavailable error) (*Provider, bool, error) {
	p, view, err := s.inheritedModelRoleSelection(ctx, role, unavailable)
	return p, view.Source == "explicit_profile", err
}

// Return the selection snapshot, not a fresh read of potentially changed settings.
func (s *Settings) inheritedModelRoleSelection(ctx context.Context, role string, unavailable error) (*Provider, BenchmarkAdmissionRoleView, error) {
	v, value, key, err := s.modelRole(ctx, role)
	if err != nil {
		return nil, v, err
	}
	if v.Source == "legacy_default" {
		p, err := s.provider(ctx)
		return p, v, err
	}
	p, err := s.profileForModelRole(v, value, key, unavailable)
	return p, v, err
}

func (s *Settings) profileForModelRole(v BenchmarkAdmissionRoleView, value work.ModelSettings, key string, unavailable error) (*Provider, error) {
	if v.Source != "explicit_profile" || v.Status != "locally_configured" {
		return nil, fmt.Errorf("%w（%s）", unavailable, v.Status)
	}
	p, err := s.providerForValue(value, key)
	if err != nil {
		return nil, unavailable
	}
	p.roleEnvelope = v.requestEnvelope
	return p, nil
}
