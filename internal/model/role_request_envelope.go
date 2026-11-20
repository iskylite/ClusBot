package model

import (
	"errors"

	"clusbot/internal/work"
)

// roleRequestEnvelope records the effective default's shaping policy, not its
// credentials or endpoint. Explicit same-destination roles may lower limits,
// but cannot use a lower output reservation (or altered metadata) to send
// context that the inherited provider would have omitted.
type roleRequestEnvelope struct {
	Model      string
	Attributes work.ModelAttributes
}

func applyModelWireOptions(request *responseRequest, model string, attributes work.ModelAttributes) {
	request.Model, request.Stream, request.Store = model, true, false
	if attributes.ReasoningEffort != "" {
		request.Reasoning = &reasoningOptions{Effort: attributes.ReasoningEffort}
	}
	if cap := attributes.MaxOutputTokens; cap != nil && request.MaxOutputTokens > *cap {
		request.MaxOutputTokens = max(1, *cap)
	}
	if window := attributes.ContextTokens; window != nil && request.MaxOutputTokens > *window/2 {
		request.MaxOutputTokens = max(1, *window/2)
	}
}

func (p *Provider) boundedWireRequest(request *responseRequest, encode func(any) ([]byte, error)) ([]byte, error) {
	limit := p.requestByteLimit()
	if p.roleEnvelope == nil {
		applyModelWireOptions(request, p.model, p.attributes)
		return boundedModelRequest(request, limit, p.attributes.ContextTokens, encode)
	}
	original := *request
	inheritedPolicy := *p.roleEnvelope
	// This projection is local only. The default never receives a request,
	// creates a manifest, or acts as a fallback provider.
	project := func(excerpts bool) ([]byte, bool, error) {
		inherited := original
		applyModelWireOptions(&inherited, inheritedPolicy.Model, inheritedPolicy.Attributes)
		if _, err := boundWithExcerptPolicy(&inherited, limit, inheritedPolicy.Attributes.ContextTokens, encode, excerpts); err != nil {
			return nil, false, err
		}
		*request = original
		request.Input, request.Instructions, request.compacted, request.excerpted = inherited.Input, inherited.Instructions, inherited.compacted, inherited.excerpted
		applyModelWireOptions(request, p.model, p.attributes)
		// Never append excerpts from another turn on this second pass.
		body, err := boundWithExcerptPolicy(request, limit, p.attributes.ContextTokens, encode, false)
		return body, inherited.compacted && excerpts, err
	}
	body, hadExcerpts, err := project(inheritedPolicy.Attributes.ContextTokens != nil)
	if hadExcerpts && errors.Is(err, work.ErrModelRequestBudget) {
		// Optional inherited excerpts must not block an intentionally smaller
		// role if removing only those excerpts suffices. Still no extra send.
		body, _, err = project(false)
	}
	return body, err
}
