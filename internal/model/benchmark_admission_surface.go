package model

import (
	"context"
	"strings"
	"time"

	"clusbot/internal/work"
)

// A signed card carries a single stored suggestion, not a model-selected
// object. Only this event kind and a generic scope marker go to the model.
func (s *Settings) InterpretBenchmarkAdmissionAction(ctx context.Context) (string, error) {
	if s == nil || s.Store == nil || s.Demo {
		return "", work.ErrBenchmarkAdmissionModel
	}
	p, _, err := s.benchmarkAdmissionProvider(ctx)
	if err != nil {
		return "", err
	}
	if p.routing != nil {
		p, err = p.routing.candidate(ctx, 0)
		if err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := p.readText(ctx, responseRequest{
		noRetry: true, MaxOutputTokens: 48,
		Instructions: `The user pressed a benchmark intake preview button on a single previously matched plan. Reply only preview, or clarify if clarification is needed. This cannot approve, queue, dispatch, change the plan, or name a target.`,
		Input:        []any{responseMessage{Type: "message", Role: "user", Content: `{"action":"preview_benchmark_admission","scope_kind":"single_bound_plan"}`}},
	})
	if err != nil {
		return "", work.ErrBenchmarkAdmissionModel
	}
	answer := strings.TrimSpace(raw)
	if answer != "preview" && answer != "clarify" {
		return "", work.ErrBenchmarkAdmissionModel
	}
	return answer, nil
}
