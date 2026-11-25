package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"clusbot/internal/work"
)

// DiscoveryA2UIModelInput is already verified by the account's runtime. No
// address, serial number, chosen owner/location/role value, provider name,
// arbitrary notes, or conversation history is exported to a model.
type DiscoveryA2UIModelInput struct {
	CandidateID string   `json:"candidate_id"`
	Revision    int64    `json:"revision"`
	State       string   `json:"candidate_state"`
	Action      string   `json:"requested_action"`
	Allowed     []string `json:"allowed_results"`
	Fields      []string `json:"admission_fields"`
}

func (s *Settings) InterpretDiscoveryA2UI(ctx context.Context, input DiscoveryA2UIModelInput, authorize func(context.Context) error) (json.RawMessage, error) {
	if s == nil || s.Store == nil || s.Demo || authorize == nil {
		return nil, work.ErrTaskWorkerModelRoleUnavailable
	}
	role := work.TaskWorkerModelRole
	provider, selection, err := s.inheritedModelRoleSelection(ctx, role, work.ErrTaskWorkerModelRoleUnavailable)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", work.ErrTaskWorkerModelRoleUnavailable, err)
	}
	egress := selection.EgressFields
	if selection.Source == "role_policy" || selection.Source == "business_override" {
		egress = routingEgressFields(role)
	}
	if !slices.Contains(egress, "authorized_read_tool_definitions_and_results") {
		return nil, work.ErrTaskWorkerModelRoleUnavailable
	}
	if provider.routing != nil {
		provider, err = provider.routing.candidate(ctx, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", work.ErrTaskWorkerModelRoleUnavailable, err)
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	ctx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	reply, err := provider.readText(ctx, responseRequest{
		Instructions: `你是个人助手的候选资产审阅步骤。只能返回 JSON：{"action":"允许结果中的唯一动作","reply":"简短说明"}。仅使用提供的候选引用及状态，不猜序列号、地址、归属、位置、角色或运维权限。说明候选还没纳管，填写资料或核对预览不等于执行。不得输出执行命令、接口、其他动作、表单字段值或可执行组件，不声称已同步、扫节点或完成纳管。不需要复述候选引用。`,
		Input:        []any{responseMessage{Type: "message", Role: "user", Content: string(data)}}, MaxOutputTokens: 512, noRetry: true,
	})
	return json.RawMessage(reply), err
}
