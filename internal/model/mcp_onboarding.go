package model

import (
	"context"
	"encoding/json"

	"clusbot/internal/work"
)

// The model sees a closed vocabulary. The original message, account, object
// IDs, connection config, executable, URLs and credentials are not inputs.
func (s *Settings) MCPOnboardingDecision(ctx context.Context, action, choice string, authorize func(context.Context) error) (json.RawMessage, error) {
	if !(action == "choose_mcp_transport" && (choice == "http" || choice == "stdio")) && !(action == "request_mcp_setup_step" && (choice == "probe" || choice == "permissions" || choice == "binding")) {
		return nil, work.ErrAuthorizationDenied
	}
	return s.interpretScopedAction(ctx, struct {
		Action    string `json:"requested_action"`
		Selection string `json:"selection"`
	}{action, choice},
		`你是 MCP 接入助手。用户已选择接入方式或要查看的后续步骤；决定下一步，只能返回 {"next":"open_trusted_form"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。表单只在宿主中显示，不要生成地址、凭据、程序、参数或工具调用。打开表单不会创建、运行或授权任何连接。`, authorize)
}
