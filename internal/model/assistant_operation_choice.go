package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"clusbot/internal/work"
)

// InterpretAssistantAdviceChoice cannot expose an asset identifier, the
// displayed suggestion text, a procedure, or tool definitions. The model
// may only acknowledge the selection or ask for further details in chat.
func (s *Settings) InterpretAssistantAdviceChoice(ctx context.Context, scope, focus, slot string, authorize func(context.Context) error) (string, error) {
	if s == nil || s.Store == nil || s.Demo || authorize == nil || !slices.Contains([]string{"pod", "global"}, scope) ||
		!slices.Contains([]string{"node_investigation", "reported_slow_training", "recovery"}, focus) || !slices.Contains([]string{"1", "2", "3", "4"}, slot) {
		return "", InvalidSettings("运维建议选择暂不可用")
	}
	p, _, err := s.inheritedModelRoleSelection(ctx, work.TaskWorkerModelRole, work.ErrTaskWorkerModelRoleUnavailable)
	if err != nil {
		return "", err
	}
	if p.routing != nil {
		p, err = p.routing.candidate(ctx, 0)
		if err != nil {
			return "", err
		}
	}
	if err = authorize(ctx); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"action": "review_operation_advice_choice", "scope_kind": scope, "focus_kind": focus, "choice_slot": slot})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := p.readText(ctx, responseRequest{
		Instructions:    `用户明确选择了本次运维建议中的一个文字选项。这不是批准、派发、介入现场或任何业务操作。仅回复 record_selection（记录选择）或 request_details（让用户另行补充信息）；不返回命令、网址、工具或动作。`,
		Input:           []any{responseMessage{Type: "message", Role: "user", Content: string(body)}},
		MaxOutputTokens: 48, noRetry: true,
	})
	if err != nil {
		return "", err
	}
	result := strings.TrimSpace(raw)
	if result != "record_selection" && result != "request_details" {
		return "", InvalidSettings("模型未返回有效的建议回执")
	}
	return result, nil
}
