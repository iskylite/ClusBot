package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"clusbot/internal/work"
)

type RoutingCandidateInput struct {
	ProfileID       string `json:"profile_id"`
	ProfileRevision int    `json:"profile_revision"`
}

type RoutingPolicyInput struct {
	Revision           int                     `json:"revision"`
	Candidates         []RoutingCandidateInput `json:"candidates"`
	AllowCrossProvider bool                    `json:"allow_cross_provider"`
	ConfirmEgress      bool                    `json:"confirm_egress"`
}

type RoutingCandidateView struct {
	ProfileID       string `json:"profile_id"`
	ProfileRevision int    `json:"profile_revision"`
	ProviderID      string `json:"provider_id,omitempty"`
	ModelID         string `json:"model_id,omitempty"`
	Status          string `json:"status"`
}

type RoutingPolicyView struct {
	Business           string                 `json:"business"`
	Role               string                 `json:"role"`
	Revision           int                    `json:"revision"`
	AllowCrossProvider bool                   `json:"allow_cross_provider"`
	Candidates         []RoutingCandidateView `json:"candidates"`
}

func routingBusiness(role string) string {
	switch role {
	case work.GeneralRouterModelRole, work.ConversationReplyModelRole:
		return "conversation"
	case work.TaskWorkerModelRole:
		return "task"
	default:
		return "benchmark"
	}
}

func (s *Settings) routingCandidate(ctx context.Context, c work.ModelRoutingCandidate) (RoutingCandidateView, work.ModelConnectionProfile, string, error) {
	out := RoutingCandidateView{ProfileID: c.ProfileID, ProfileRevision: c.ProfileRevision}
	p, err := s.Store.ReadModelProfile(ctx, c.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		out.Status = "profile_missing"
		return out, p, "", nil
	}
	if err != nil {
		return out, p, "", err
	}
	out.ProviderID = p.ProviderID
	key := keyValue(p.Settings)
	switch {
	case !p.ProviderEnabled:
		out.Status = "provider_disabled"
	case !p.Enabled:
		out.Status = "profile_disabled"
	case p.Settings.Revision != c.ProfileRevision:
		out.Status = "profile_revision_changed"
	case p.Settings.ModelID == "" || validateProfileSettings(p.Settings) != nil:
		out.Status = "profile_invalid"
	case !credentialAvailableWithKey(p.Settings, key):
		out.Status = "credential_missing"
	case c.CredentialBinding != s.Store.ModelCredentialBinding(key):
		out.Status = "credential_changed"
	default:
		out.Status, out.ModelID = "locally_configured", p.Settings.ModelID
	}
	return out, p, key, nil
}

func (s *Settings) routingPolicyView(ctx context.Context, p work.ModelRoutingPolicy) (RoutingPolicyView, error) {
	out := RoutingPolicyView{Business: p.Business, Role: p.Role, Revision: p.Revision, AllowCrossProvider: p.AllowCrossProvider, Candidates: []RoutingCandidateView{}}
	for _, c := range p.Candidates {
		v, _, _, err := s.routingCandidate(ctx, c)
		if err != nil {
			return out, err
		}
		out.Candidates = append(out.Candidates, v)
	}
	return out, nil
}

func (s *Settings) ModelRoutingPolicies(ctx context.Context) ([]RoutingPolicyView, error) {
	if s == nil || s.Store == nil {
		return nil, InvalidSettings("模型设置尚不可用")
	}
	policies, err := s.Store.ModelRoutingPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RoutingPolicyView, 0, len(policies))
	for _, p := range policies {
		v, err := s.routingPolicyView(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// The shared admission gate has no stage argument. When the global default is
// absent, every core stage must have a usable selection before releasing work.
// This checks local configuration only; it does not probe a provider.
func (s *Settings) CoreRoutingAvailable(ctx context.Context) bool {
	for _, role := range []string{work.GeneralRouterModelRole, work.ConversationReplyModelRole, work.TaskWorkerModelRole} {
		view, err := s.PreviewModelRouting(ctx, role)
		if err != nil || (view.Status != "locally_configured" && view.Status != "configured") {
			return false
		}
	}
	return true
}

func (s *Settings) SaveModelRoutingPolicy(ctx context.Context, business, role string, in RoutingPolicyInput) (RoutingPolicyView, error) {
	if s == nil || s.Store == nil || !work.ValidModelRoutingScope(business, role) || in.Revision < 0 || len(in.Candidates) > work.MaxModelRoutingCandidates {
		return RoutingPolicyView{}, InvalidSettings("模型策略的业务、阶段或版本无效")
	}
	if len(in.Candidates) > 0 && (!in.ConfirmEgress || s.Demo) {
		return RoutingPolicyView{}, InvalidSettings("请明确确认所选模型均可处理此阶段的数据")
	}
	p := work.ModelRoutingPolicy{Business: business, Role: role, Revision: in.Revision, AllowCrossProvider: in.AllowCrossProvider}
	seen := map[string]bool{}
	providerID := ""
	for _, c := range in.Candidates {
		if c.ProfileID == "" || c.ProfileRevision < 1 || seen[c.ProfileID] {
			return RoutingPolicyView{}, InvalidSettings("候选模型或版本无效，不能重复")
		}
		seen[c.ProfileID] = true
		profile, err := s.Store.ReadModelProfile(ctx, c.ProfileID)
		if errors.Is(err, sql.ErrNoRows) {
			return RoutingPolicyView{}, work.ErrModelSettingsRevision
		}
		if err != nil {
			return RoutingPolicyView{}, err
		}
		if profile.Settings.Revision != c.ProfileRevision {
			return RoutingPolicyView{}, work.ErrModelSettingsRevision
		}
		key := keyValue(profile.Settings)
		if !profile.Enabled || !profile.ProviderEnabled || profile.Settings.ModelID == "" || validateProfileSettings(profile.Settings) != nil || !credentialAvailableWithKey(profile.Settings, key) {
			return RoutingPolicyView{}, InvalidSettings("候选模型、提供方或凭据不可用")
		}
		if providerID != "" && providerID != profile.ProviderID && !in.AllowCrossProvider {
			return RoutingPolicyView{}, InvalidSettings("跨提供方候补须明确允许")
		}
		providerID = profile.ProviderID
		p.Candidates = append(p.Candidates, work.ModelRoutingCandidate{ProfileID: c.ProfileID, ProfileRevision: c.ProfileRevision, CredentialBinding: s.Store.ModelCredentialBinding(key)})
	}
	p, err := s.Store.SaveModelRoutingPolicy(ctx, p)
	if err != nil {
		return RoutingPolicyView{}, err
	}
	return s.routingPolicyView(ctx, p)
}

// Only a configured higher layer is selected. Invalid selections never fall
// through to another layer, and cleared layers keep their revision for CAS.
func (s *Settings) selectedRoutingPolicy(ctx context.Context, business, role string) (work.ModelRoutingPolicy, bool, error) {
	if s == nil || s.Store == nil {
		return work.ModelRoutingPolicy{}, false, InvalidSettings("模型设置尚不可用")
	}
	if !work.ValidModelRoutingScope(business, role) {
		return work.ModelRoutingPolicy{}, false, InvalidSettings("不支持此业务和阶段组合")
	}
	p, err := s.Store.ReadModelRoutingPolicy(ctx, business, role)
	if err != nil {
		return p, false, err
	}
	if len(p.Candidates) > 0 {
		return p, true, nil
	}
	if business != "default" {
		p, err = s.Store.ReadModelRoutingPolicy(ctx, "default", role)
		if err != nil {
			return p, false, err
		}
		if len(p.Candidates) > 0 {
			return p, true, nil
		}
	}
	return p, false, nil
}

type routingExecution struct {
	settings *Settings
	policy   work.ModelRoutingPolicy
}

type routingAttempt struct {
	policy work.ModelRoutingPolicy
	number int
	reason string
}

func routingFailureReason(err error) string {
	var status responseHTTPError
	if errors.As(err, &status) {
		switch int(status) {
		case http.StatusTooManyRequests:
			return "rate_limited"
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return "provider_unavailable"
		}
	}
	var send modelSendError
	var network net.Error
	if errors.As(err, &send) && errors.As(err, &network) {
		return "network_error"
	}
	return ""
}

func (s *Settings) routingProvider(ctx context.Context, business, role string) (*Provider, work.ModelRoutingPolicy, bool, error) {
	p, selected, err := s.selectedRoutingPolicy(ctx, business, role)
	if err != nil || !selected {
		return nil, p, selected, err
	}
	if s.Demo {
		return nil, p, true, InvalidSettings("演示模式不使用模型策略")
	}
	v, profile, key, err := s.routingCandidate(ctx, p.Candidates[0])
	if err != nil {
		return nil, p, true, err
	}
	if v.Status != "locally_configured" {
		return nil, p, true, fmt.Errorf("模型策略不可用（%s）", v.Status)
	}
	provider, err := s.providerForValue(profile.Settings, key)
	if err != nil {
		return nil, p, true, err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, p, true, err
	}
	provider.checkpointScope = fmt.Sprintf("routing_policy/v1:%x", sha256.Sum256(encoded))
	provider.routing = &routingExecution{settings: s, policy: p}
	return provider, p, true, nil
}

// A previously selected policy may not bypass later revocation, profile edits,
// credential rotation, or a policy replacement. No lower layer is consulted.
func (r *routingExecution) candidate(ctx context.Context, index int) (*Provider, error) {
	current, err := r.settings.Store.ReadModelRoutingPolicy(ctx, r.policy.Business, r.policy.Role)
	if err != nil {
		return nil, err
	}
	if current.Revision != r.policy.Revision || index >= len(current.Candidates) {
		return nil, InvalidSettings("模型策略已变更，请重新开始")
	}
	v, profile, key, err := r.settings.routingCandidate(ctx, r.policy.Candidates[index])
	if err != nil {
		return nil, err
	}
	if v.Status != "locally_configured" {
		return nil, InvalidSettings("候选模型或凭据已失效，请检查策略")
	}
	p, err := r.settings.providerForValue(profile.Settings, key)
	if err != nil {
		return nil, err
	}
	p.checkpointScope = fmt.Sprintf("routing_policy/v1:%s:%s:%d", r.policy.Business, r.policy.Role, r.policy.Revision)
	return p, nil
}
