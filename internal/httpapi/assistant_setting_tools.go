package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"clusbot/internal/work"
)

// assistantSettingTools lists only fixed routes whose requests and responses do
// not contain credentials, connection commands or permission grants. Mutating
// calls use the underlying API's body and are subject to user confirmation.
func assistantSettingTools() []assistantTool {
	return []assistantTool{
		{Name: "get_current_model_routing", Description: "查看当前默认模型与各角色实际选择、状态和版本；这是当前配置，不代表某次请求的实际回执。", Method: "GET", Path: "/api/v1/assistant/model-routing"},
		{Name: "list_available_models", Description: "查看模型档案名称、标识、状态和版本；多人模式仅管理员可用。", Method: "GET", Path: "/api/v1/assistant/models"},
		{Name: "get_default_model_setting", Description: "查看默认模型选择及版本；多人模式仅管理员可用。", Method: "GET", Path: "/api/v1/settings/model/default"},
		{Name: "set_default_model", Description: "按原接口的 source、profile_id、profile_revision 和 revision 更新默认模型；多人模式仅管理员可用。", Method: "PUT", Path: "/api/v1/settings/model/default", Confirm: true},
		{Name: "get_run_model_receipt", Description: "查看指定执行每次模型请求的已记录模型标识和提供方回执；提供方报告不等于独立验证。", Method: "GET", Path: "/api/v1/assistant/runs/{id}/model-receipt"},
		{Name: "get_token_usage_stats", Description: "按时间窗口（24h/7d/30d）汇总模型调用与输入/输出 Token 趋势分布。", Method: "GET", Path: "/api/v1/assistant/token-usage-stats"},
		{Name: "get_admission_model_receipt", Description: "查看指定受理标识的路由模型选择和回执。", Method: "GET", Path: "/api/v1/admissions/{key}"},

		{Name: "list_skills", Description: "查看当前可用的 Skill 和版本。", Method: "GET", Path: "/api/v1/skills"},
		{Name: "draft_skill", Description: "按原接口的 scope_label、instructions 和 revision 新建 Skill 草稿。", Method: "POST", Path: "/api/v1/skills", Confirm: true},
		{Name: "preview_skill_document", Description: "按原接口提交 SKILL.md 文本，预览导入内容；不会执行文档中的命令。", Method: "POST", Path: "/api/v1/skills/import-preview", Confirm: true},
		{Name: "import_skill_document", Description: "按原接口提交文档与预览绑定，将 Skill 文本导入为草稿。", Method: "POST", Path: "/api/v1/skills/import", Confirm: true},
		{Name: "activate_skill", Description: "使用 version、revision 和 sha256 激活指定 Skill 版本。", Method: "POST", Path: "/api/v1/skills/{id}/activate", Confirm: true},
		{Name: "disable_skill", Description: "使用 revision 停用指定 Skill。", Method: "POST", Path: "/api/v1/skills/{id}/disable", Confirm: true},
		{Name: "get_conversation_skill", Description: "查看指定对话选择的 Skill。", Method: "GET", Path: "/api/v1/conversations/{id}/skill-binding"},
		{Name: "set_conversation_skill", Description: "按原接口设置或清除指定对话的 Skill，需提供绑定版本。", Method: "PUT", Path: "/api/v1/conversations/{id}/skill-binding", Confirm: true},
		{Name: "get_run_skill_binding", Description: "查看指定执行实际绑定的 Skill 版本。", Method: "GET", Path: "/api/v1/runs/{id}/skill-binding"},

		{Name: "list_mcp_connections", Description: "查看 MCP 连接的名称、状态和版本，不返回地址、命令或凭据配置。", Method: "GET", Path: "/api/v1/assistant/connections"},
		{Name: "list_authorized_mcp_tools", Description: "查看当前已授权且可用于执行的 MCP 工具。", Method: "GET", Path: "/api/v1/task-tools"},
		{Name: "get_conversation_mcp_selection", Description: "查看指定对话当前选择的 MCP 工具。", Method: "GET", Path: "/api/v1/conversations/{id}/mcp-selection"},
		{Name: "set_conversation_mcp_selection", Description: "按原接口选择已有授权的 MCP 工具或清除选择；不会授予新权限。", Method: "PUT", Path: "/api/v1/conversations/{id}/mcp-selection", Confirm: true},
		{Name: "get_run_mcp_bindings", Description: "查看指定执行实际使用的 MCP 绑定和当前有效性。", Method: "GET", Path: "/api/v1/runs/{id}/mcp-bindings"},
		{Name: "list_mcp_calls", Description: "查看指定 MCP 连接的调用记录；多人模式只返回本账号的记录。", Method: "GET", Path: "/api/v1/connections/{id}/calls"},

		{Name: "list_memories", Description: "查看个人记忆。", Method: "GET", Path: "/api/v1/memories"},
		{Name: "get_memory_source", Description: "查看指定消息标识对应的记忆来源。", Method: "GET", Path: "/api/v1/memories/source/{message}"},
		{Name: "create_memory", Description: "按原接口创建个人记忆。", Method: "POST", Path: "/api/v1/memories", Confirm: true},
		{Name: "update_memory", Description: "按原接口更新指定个人记忆，需包含 revision。", Method: "PATCH", Path: "/api/v1/memories/{id}", Confirm: true},
		{Name: "delete_memory", Description: "按原接口删除指定个人记忆，需包含 revision。", Method: "DELETE", Path: "/api/v1/memories/{id}", Confirm: true},
		{Name: "list_experience_candidates", Description: "查看个人经验候选。", Method: "GET", Path: "/api/v1/experience-candidates"},
		{Name: "review_experience_candidate", Description: "按原接口审阅指定经验候选，需提供 revision 和 decision。", Method: "POST", Path: "/api/v1/experience-candidates/{id}/review", Confirm: true},

		{Name: "get_my_account", Description: "查看当前登录状态和账号资料。", Method: "GET", Path: "/api/v1/session"},
		{Name: "list_accounts", Description: "查看账号列表，仅管理员可用。", Method: "GET", Path: "/api/v1/admin/users"},
		{Name: "get_storage_diagnostics", Description: "查看本地存储状态摘要，不返回文件路径或内容。", Method: "GET", Path: "/api/v1/settings/data/storage"},
		{Name: "get_discovery_setup_status", Description: "查看资产发现功能的配置状态。", Method: "GET", Path: "/api/v1/domain/discovery/setup-status"},
	}
}

// Form targets are descriptive UI destinations, not API paths to dispatch.
// Account management has no web UI yet; its symbolic target must not be
// presented as a working link until that UI is implemented.
func assistantSettingForms() map[string]string {
	return map[string]string{
		"configure_conversation":   "conversation_configuration",
		"configure_model_provider": "/settings#model-default",
		"configure_model_profile":  "/settings#model-profiles",
		"configure_model_role":     "/settings#model-roles",
		"configure_mcp_connection": "/connections/new",
		"configure_mcp_permission": "/connections",
		"manage_accounts":          "account_management",
		"change_my_password":       "account_password",
	}
}

// assistantModelRouting exposes the existing non-probing routing projection,
// with only a model identifier that cannot be confused with its credential.
func (s Server) assistantModelRouting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := s.Settings.ModelRouting(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	for i := range out.Roles {
		selection := &out.Roles[i]
		profileID := selection.ProfileID
		if profileID == "" {
			profileID = selection.DefaultProfileID
		}
		selection.ModelID = s.assistantSafeModelID(r.Context(), profileID, selection.ModelID)
		for j := range selection.Candidates {
			candidate := &selection.Candidates[j]
			candidate.ModelID = s.assistantSafeModelID(r.Context(), candidate.ProfileID, candidate.ModelID)
		}
	}
	jsonResponse(w, http.StatusOK, out)
}

type assistantModelView struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	ModelID              string `json:"model_id,omitempty"`
	Revision             int    `json:"revision"`
	Enabled              bool   `json:"enabled"`
	ProviderEnabled      bool   `json:"provider_enabled"`
	CredentialConfigured bool   `json:"credential_configured"`
}

func (s Server) assistantModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, ok := accountFromContext(r)
	if !ok || !account.IsSuperadmin {
		fail(w, http.StatusForbidden, "forbidden", "只有超级管理员可管理实例设置")
		return
	}
	profiles, err := s.Settings.Profiles(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	views := make([]assistantModelView, 0, len(profiles))
	for _, profile := range profiles {
		views = append(views, assistantModelView{ID: profile.ID, Name: profile.Name,
			ModelID: s.assistantSafeModelID(r.Context(), profile.ID, profile.ModelID), Revision: profile.Revision,
			Enabled: profile.Enabled, ProviderEnabled: profile.ProviderEnabled, CredentialConfigured: profile.CredentialConfigured})
	}
	jsonResponse(w, http.StatusOK, views)
}

func (s Server) assistantSafeModelID(ctx context.Context, profileID, id string) string {
	if !work.ValidModelReceiptIdentifier(id) || s.Settings == nil || s.Settings.Store == nil {
		return ""
	}
	var settings work.ModelSettings
	var err error
	if profileID != "" {
		var profile work.ModelConnectionProfile
		profile, err = s.Settings.Store.ReadModelProfile(ctx, profileID)
		settings = profile.Settings
	} else {
		settings, err = s.Settings.Store.ModelSettings(ctx)
	}
	if err != nil {
		return ""
	}
	credential := settings.APIKey
	if credential == "" && strings.HasPrefix(settings.KeyRef, "env:") {
		credential = strings.TrimSpace(os.Getenv(strings.TrimPrefix(settings.KeyRef, "env:")))
	}
	for _, secret := range []string{credential, settings.KeyRef, strings.TrimPrefix(settings.KeyRef, "env:")} {
		if secret != "" && (id == secret || len(secret) >= 8 && strings.Contains(id, secret)) {
			return ""
		}
	}
	return id
}

// assistantConnections deliberately reuses the account-aware visibility rule
// but always returns the restricted view, even for a local user or admin.
func (s Server) assistantConnections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var connections []work.Connection
	var err error
	if account, ok := accountFromContext(r); ok && !account.IsSuperadmin {
		connections, err = s.Store.SharedConnections(r.Context())
	} else {
		connections, err = s.Store.Connections(r.Context())
	}
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]sharedConnectionView, 0, len(connections))
	for _, connection := range connections {
		views = append(views, sharedConnection(connection))
	}
	jsonResponse(w, http.StatusOK, views)
}

type assistantModelReceipt struct {
	Generation     int                `json:"execution_generation"`
	RecordedAt     int64              `json:"recorded_at"`
	RequestedModel string             `json:"requested_model,omitempty"`
	Receipt        *work.ModelReceipt `json:"model_receipt,omitempty"`
}

func (s Server) assistantRunModelReceipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !validID(r.PathValue("id")) {
		fail(w, http.StatusBadRequest, "invalid_run", "需要有效的执行标识")
		return
	}
	record, err := s.Store.RunRecord(r.Context(), r.PathValue("id"), "")
	if err != nil {
		writeError(w, err)
		return
	}
	receipts := make([]assistantModelReceipt, 0, len(record.Contexts))
	for _, entry := range record.Contexts {
		var manifest struct {
			RequestedModel string `json:"requested_model"`
		}
		_ = json.Unmarshal(entry.Manifest, &manifest)
		if !work.ValidModelReceiptIdentifier(manifest.RequestedModel) {
			manifest.RequestedModel = ""
		}
		receipts = append(receipts, assistantModelReceipt{Generation: entry.Generation,
			RecordedAt: entry.RecordedAt, RequestedModel: manifest.RequestedModel, Receipt: entry.ModelReceipt})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"run_id": record.ID, "run_status": record.Status, "receipts": receipts})
}
