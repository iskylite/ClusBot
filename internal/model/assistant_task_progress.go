package model

import (
	"context"
	"encoding/json"
)

func (s *Settings) AssistantTaskProgressDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "query_task_progress"},
		`用户在自己的任务清单里选中一项，要求只读查询进度。只能返回 {"next":"show_progress"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主负责核对并展示本地事实；不要生成任务状态、执行指令或其他参数，也不需要索取凭据。`, authorize)
}

func (s *Settings) AssistantAssetLedgerDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "query_asset_ledger"},
		`用户从自己的资产台账选中一项，请求核对本地记录。只能返回 {"next":"show_ledger"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主负责读取本地事实。不要填写资产身份、地址、状态、指令或额外参数。`, authorize)
}

func (s *Settings) AssistantAttentionReasonDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "query_attention_reason"},
		`用户选中自己的一个待处理事项，要求只读核对等待原因及来源。只能返回 {"next":"show_reason"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主将核对记录。不要替用户批准或拒绝，不要派发任务，不要提供对象、状态或其他参数。`, authorize)
}

func (s *Settings) AssistantSkillBindingPreviewDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "preview_conversation_skill", "scope_kind": "future_conversation"},
		`用户在当前会话选择一条现有 Skill，只要预览绑定。只能返回 {"next":"show_binding_preview"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。不能保存会话设置、改动原有配置、读取或者执行文档正文，不请求工具、ID 或凭据。`, authorize)
}

func (s *Settings) AssistantApprovalPreviewDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "preview_pending_approval"},
		`用户仅请求预览本人一项待审批的详细范围。只能返回 {"next":"show_approval_preview"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。应用独立核对审批，模型不能决定、同意、拒绝、执行或填写对象、参数、权限。`, authorize)
}

func (s *Settings) AssistantArtifactPreviewDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "preview_artifact_text"},
		`用户从自己的成果清单选中一项，要求只读预览本地正文。只能返回 {"next":"show_preview"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。不要输出成果内容、文件名、下载地址、额外参数或对成果验收。宿主将核对记录并显示有限的纯文本。`, authorize)
}

func (s *Settings) AssistantMemoryProvenanceDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "review_memory_provenance", "scope_kind": "original_reply"},
		`用户选择仅核对本条回复引用的一条记忆的来源。只能返回 {"next":"show_source"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主负责核对和显示来源。不需要记忆、原文、身份或凭据；不能保存、更新、删除或调取其他会话。`, authorize)
}

func (s *Settings) AssistantBenchmarkComparisonDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "preview_benchmark_comparison", "scope_kind": "original_report_pair"},
		`用户要求核对两份现有报告的同一测项。只能返回 {"next":"show_comparison"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主决定能否比较并计算差值；不要请求或生成报告、指标、原始证据、对象、因果判断或执行动作。`, authorize)
}

func (s *Settings) AssistantDiscoveryScheduleDecision(ctx context.Context, authorize func(context.Context) error) (json.RawMessage, error) {
	return s.interpretScopedAction(ctx, map[string]string{"requested_action": "preview_discovery_schedule", "scope_kind": "existing_discovery_plan"},
		`用户仅核对已经存在的定时发现记录。只能返回 {"next":"show_schedule"}、{"next":"ask_for_details"} 或 {"next":"cannot_prepare"}。宿主会检查当前状态。不要推断计划 ID、时间、来源、授权或回执，不得更改、启停、触发任何计划。`, authorize)
}
