package model

import (
	"context"
	"slices"

	"clusbot/internal/work"
)

const deterministicPhase = "deterministic"

var modelRoutingPhases = []string{
	work.GeneralRouterModelRole, work.ConversationReplyModelRole, work.TaskWorkerModelRole,
	work.BenchmarkAdmissionModelRole, work.BenchmarkRevisionModelRole, work.BenchmarkExecutionModelRole,
	deterministicPhase,
}

type RoutingCapabilities struct {
	BusinessOverridesSupported bool `json:"business_overrides_supported"`
	OrderedFallbackSupported   bool `json:"ordered_fallback_supported"`
}

type ModelRoutingView struct {
	RoutingCapabilities
	Roles []ModelRoutingSelection `json:"roles"`
}

type ModelRoutingSelection struct {
	RoutingCapabilities
	Role                string                 `json:"role"`
	Source              string                 `json:"source"`
	Status              string                 `json:"status"`
	ModelID             string                 `json:"model_id,omitempty"`
	Revision            int                    `json:"revision,omitempty"`
	ProfileID           string                 `json:"profile_id,omitempty"`
	ProfileRevision     int                    `json:"profile_revision,omitempty"`
	SettingsRevision    int                    `json:"settings_revision,omitempty"`
	DefaultProfileID    string                 `json:"default_profile_id,omitempty"`
	EgressFields        []string               `json:"egress_fields,omitempty"`
	RemoteObservation   string                 `json:"remote_observation"`
	Business            string                 `json:"business,omitempty"`
	PolicyRevision      int                    `json:"policy_revision,omitempty"`
	Candidates          []RoutingCandidateView `json:"candidates,omitempty"`
	FallbackConditions  []string               `json:"fallback_conditions,omitempty"`
	FallbackLimitations []string               `json:"fallback_limitations,omitempty"`
}

func (s *Settings) ModelRouting(ctx context.Context) (ModelRoutingView, error) {
	out := ModelRoutingView{RoutingCapabilities: RoutingCapabilities{true, true}, Roles: make([]ModelRoutingSelection, 0, len(modelRoutingPhases))}
	for _, role := range modelRoutingPhases {
		selection, err := s.PreviewModelRouting(ctx, role)
		if err != nil {
			return ModelRoutingView{}, err
		}
		out.Roles = append(out.Roles, selection)
	}
	return out, nil
}

// PreviewModelRouting reports what the enabled routing layers select now. It
// does not resolve a particular task, change configuration or call a model.
func (s *Settings) PreviewModelRouting(ctx context.Context, role string) (ModelRoutingSelection, error) {
	return s.PreviewModelRoutingForBusiness(ctx, "", role)
}

func (s *Settings) PreviewModelRoutingForBusiness(ctx context.Context, business, role string) (ModelRoutingSelection, error) {
	if !slices.Contains(modelRoutingPhases, role) {
		return ModelRoutingSelection{}, InvalidSettings("不支持此模型角色")
	}
	out := ModelRoutingSelection{RoutingCapabilities: RoutingCapabilities{true, true}, Role: role, RemoteObservation: "not_checked"}
	if role == deterministicPhase {
		if business != "" {
			return ModelRoutingSelection{}, InvalidSettings("此阶段不使用模型")
		}
		out.Source, out.Status, out.RemoteObservation = "go", "model_not_used", "not_applicable"
		return out, nil
	}
	if business == "" {
		business = routingBusiness(role)
	}
	policy, selected, err := s.selectedRoutingPolicy(ctx, business, role)
	if err != nil {
		return ModelRoutingSelection{}, err
	}
	out.Business = business
	if selected {
		view, err := s.routingPolicyView(ctx, policy)
		if err != nil {
			return ModelRoutingSelection{}, err
		}
		out.Source, out.Status, out.PolicyRevision = "role_policy", view.Candidates[0].Status, policy.Revision
		if s.Demo {
			out.Status = "demo_profile_disabled"
		}
		if policy.Business != "default" {
			out.Source = "business_override"
		}
		out.ProfileID, out.ProfileRevision, out.ModelID = view.Candidates[0].ProfileID, view.Candidates[0].ProfileRevision, view.Candidates[0].ModelID
		out.Candidates = view.Candidates
		out.EgressFields = routingEgressFields(role)
		out.FallbackConditions = []string{"network_error", "timeout", "rate_limited", "provider_unavailable"}
		out.FallbackLimitations = []string{"before_output_only", "tool_requests_do_not_fallback", "one_attempt_per_candidate"}
		return out, nil
	}
	binding, value, _, err := s.modelRole(ctx, role)
	if err != nil {
		return ModelRoutingSelection{}, err
	}
	out.Source, out.Status = binding.Source, binding.Status
	out.Revision, out.EgressFields = binding.Revision, binding.EgressFields
	out.ProfileID, out.ProfileRevision = binding.ProfileID, binding.ProfileRevision
	if binding.Source == "explicit_profile" {
		out.ModelID = value.ModelID
		return out, nil
	}
	runtime, err := s.RuntimeProjection(ctx)
	if err != nil {
		return ModelRoutingSelection{}, err
	}
	out.Source, out.Status, out.ModelID = "global_default", runtime.Availability, runtime.ModelID
	out.SettingsRevision, out.RemoteObservation = runtime.SettingsRevision, runtime.RemoteObservation
	out.DefaultProfileID = runtime.DefaultProfileID
	if runtime.Mode == "demo" && !defaultDestinationRole(role) {
		out.Status = "demo_role_unavailable"
	}
	return out, nil
}

func routingEgressFields(role string) []string {
	switch role {
	case work.GeneralRouterModelRole:
		return []string{"submitted_text", "recent_history", "task_candidates", "pending_intents", "benchmark_candidate"}
	case work.ConversationReplyModelRole:
		return []string{"submitted_text", "recent_history", "selected_text_materials", "selected_images_if_supported", "matching_user_memories"}
	case work.TaskWorkerModelRole:
		return []string{"task_messages_history_and_requirements", "selected_text_materials", "selected_images_if_supported", "matching_user_memories", "workflow_input_artifacts", "applicable_skill_and_experience", "authorized_read_tool_definitions_and_results", "current_run_model_tool_round_history"}
	case work.BenchmarkAdmissionModelRole:
		return []string{"submitted_text", "published_target", "published_profile_and_version"}
	case work.BenchmarkRevisionModelRole:
		return []string{"submitted_text", "editable_case_keys", "editable_parameter_keys"}
	case work.BenchmarkExecutionModelRole:
		return []string{"submitted_text", "pending_case_keys", "pending_case_positions"}
	}
	return nil
}
