package model

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

type RuntimeProjection struct {
	SchemaVersion     int                      `json:"schema_version"`
	SettingsRevision  int                      `json:"settings_revision,omitempty"`
	Mode              string                   `json:"mode"`
	Availability      string                   `json:"availability"`
	EndpointSource    string                   `json:"endpoint_source,omitempty"`
	ModelID           string                   `json:"model_id,omitempty"`
	ModelIDSource     string                   `json:"model_id_source,omitempty"`
	CredentialSource  string                   `json:"credential_source,omitempty"`
	RemoteObservation string                   `json:"remote_observation"`
	Capabilities      ModelRuntimeCapabilities `json:"capabilities"`
	Policy            *ModelRuntimePolicy      `json:"policy,omitempty"`
}

type ModelRuntimeCapabilities struct {
	TextStreaming string `json:"text_streaming"`
	ToolCalling   string `json:"tool_calling"`
	Vision        string `json:"vision"`
	Reasoning     string `json:"reasoning"`
}

type ModelRuntimePolicy struct {
	ContextTokens             int      `json:"context_tokens"`
	ContextWindowSource       string   `json:"context_window_source"`
	ContextHeadroomDivisor    int      `json:"context_headroom_divisor"`
	MaxRequestBytes           int      `json:"max_request_bytes"`
	ConfiguredMaxOutputTokens *int     `json:"configured_max_output_tokens,omitempty"`
	EffectiveOutputUpperBound *int     `json:"effective_output_upper_bound,omitempty"`
	ReasoningEffort           string   `json:"reasoning_effort,omitempty"`
	ReasoningLevels           []string `json:"reasoning_levels,omitempty"`
}

// RuntimeProjection reports local configuration and actual adapter policies
// without a network probe, and intentionally omits the endpoint URL, secret,
// key reference and credential binding from its DTO.
func (s *Settings) RuntimeProjection(ctx context.Context) (RuntimeProjection, error) {
	out := RuntimeProjection{SchemaVersion: 1, Mode: "unconfigured", Availability: "not_configured", RemoteObservation: "not_applicable",
		Capabilities: ModelRuntimeCapabilities{"unavailable", "unavailable", "unavailable", "unavailable"}}
	if s == nil || s.Store == nil {
		out.Availability = "settings_unavailable"
		return out, nil
	}
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RuntimeProjection{}, err
	}
	out.SettingsRevision = stored.Revision
	if s.Demo {
		out.Mode, out.Availability = "demo", "local_demo"
		out.Capabilities.TextStreaming = "local_demo"
		return out, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	v := s.effective(stored)
	out.ModelID, out.Mode = v.ModelID, v.Protocol
	out.EndpointSource, out.ModelIDSource = "settings", "settings"
	if s.BaseURLOverride != "" {
		out.EndpointSource = "runtime_override"
	}
	if s.ModelIDOverride != "" {
		out.ModelIDSource = "runtime_override"
	}
	switch {
	case v.APIKey != "":
		out.CredentialSource = "local_secret"
	case keyValue(v) != "":
		out.CredentialSource = "environment"
	default:
		out.CredentialSource = "not_resolved"
	}
	if v.ModelID == "" {
		out.Mode = "unconfigured"
		return out, nil
	}
	if _, err := modelEndpoint(v.BaseURL, v.Protocol); err != nil {
		out.Availability = "invalid_configuration"
		return out, nil
	}
	out.Availability, out.RemoteObservation = "configured", "not_checked"
	out.Capabilities = ModelRuntimeCapabilities{"adapter_enabled", "adapter_enabled", "unknown_disabled", "unknown"}
	limit := (&Provider{maxRequestBytes: s.MaxRequestBytes}).requestByteLimit()
	p := &ModelRuntimePolicy{ContextTokens: DefaultContextTokens, ContextWindowSource: "runtime_default",
		ContextHeadroomDivisor: reservedContextDivisor, MaxRequestBytes: limit}
	out.Policy = p
	a := v.Attributes
	if a == nil {
		return out, nil
	}
	if a.ContextTokens != nil {
		p.ContextTokens, p.ContextWindowSource = *a.ContextTokens, "configured_metadata"
		bound := max(1, *a.ContextTokens/2)
		p.EffectiveOutputUpperBound = &bound
	}
	if a.MaxOutputTokens != nil {
		p.ConfiguredMaxOutputTokens = a.MaxOutputTokens
		bound := *a.MaxOutputTokens
		if p.EffectiveOutputUpperBound != nil {
			bound = min(bound, *p.EffectiveOutputUpperBound)
		}
		p.EffectiveOutputUpperBound = &bound
	}
	p.ReasoningEffort = a.ReasoningEffort
	p.ReasoningLevels = slices.Clone(a.ReasoningLevels)
	if a.Vision != nil {
		out.Capabilities.Vision = "disabled"
		if *a.Vision {
			out.Capabilities.Vision = "metadata_enabled"
		}
	}
	if a.Reasoning != nil && !*a.Reasoning {
		out.Capabilities.Reasoning = "metadata_unsupported"
	} else if len(a.ReasoningLevels) > 0 || a.Reasoning != nil && *a.Reasoning {
		out.Capabilities.Reasoning = "metadata_supported"
	}
	return out, nil
}
