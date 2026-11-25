package httpapi

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"clusbot/internal/work"
)

type composerSlashCommand struct {
	Command     string `json:"command"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
}

type composerTaskOption struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	OwnerID       string `json:"owner_id,omitempty"`
	Referenceable bool   `json:"referenceable,omitempty"`
}

type composerReplyModel struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
	ModelID   string `json:"model_id"`
	Brand     string `json:"brand"`
	IsDefault bool   `json:"is_default"`
	Revision  int    `json:"revision"`
	Available bool   `json:"available"`
	Vision    bool   `json:"vision,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
}

type composerReplyStrategy struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     bool   `json:"default,omitempty"`
	CallsModel  bool   `json:"calls_model"`
	Scope       string `json:"scope"`
}

type composerOptionsResponse struct {
	SlashCommands   []composerSlashCommand    `json:"slash_commands"`
	Tasks           []composerTaskOption      `json:"tasks"`
	Users           []work.MentionableAccount `json:"users"`
	ReplyStrategies []composerReplyStrategy   `json:"reply_strategies"`
	ReplyModels     []composerReplyModel      `json:"reply_models"`
}

// inferModelBrand 参考 ZeroWork ai_brand.zig，根据模型 ID 或名称映射提供商品牌标识，供前端渲染对应 LOGO。
func inferModelBrand(modelID, name string) string {
	text := strings.ToLower(modelID + " " + name)
	for _, entry := range []struct {
		substr string
		brand  string
	}{
		{"deepseek", "deepseek"},
		{"qwen", "qwen"},
		{"qwq", "qwen"},
		{"claude", "anthropic"},
		{"anthropic", "anthropic"},
		{"gpt-", "openai"},
		{"chatgpt", "openai"},
		{"openai", "openai"},
		{"o1", "openai"},
		{"o3", "openai"},
		{"o4", "openai"},
		{"gemini", "gemini"},
		{"gemma", "gemini"},
		{"ollama", "ollama"},
		{"vllm", "ollama"},
		{"kimi", "kimi"},
		{"moonshot", "moonshot"},
		{"glm", "zhipu"},
		{"zhipu", "zhipu"},
		{"mistral", "mistral"},
		{"codestral", "mistral"},
		{"llama", "meta"},
		{"minimax", "minimax"},
		{"grok", "grok"},
	} {
		if strings.Contains(text, entry.substr) {
			return entry.brand
		}
	}
	return "generic"
}

func composerModelCapabilities(attributes *work.ModelAttributes) (vision, reasoning bool) {
	if attributes == nil {
		return false, false
	}
	vision = attributes.Vision != nil && *attributes.Vision
	if attributes.Reasoning == nil || *attributes.Reasoning {
		reasoning = len(attributes.ReasoningLevels) > 0 || attributes.Reasoning != nil && *attributes.Reasoning
	}
	return vision, reasoning
}

func defaultComposerSlashCommands() []composerSlashCommand {
	return []composerSlashCommand{
		{Command: "/benchmark", Usage: "/benchmark <方案>@<版本> [本次要求]", Description: "使用已发布的基准方案，附加要求时先给出规划建议", Mode: "task"},
		{Command: "/benchmark-status", Usage: "/benchmark-status <任务ID>", Description: "只读查询指定基准任务的当前阶段与实时进度", Mode: "read"},
		{Command: "/assistant", Usage: "/assistant <指令>", Description: "调用对话助手工具（也可使用别名 /助手）", Mode: "assistant"},
		{Command: "记住", Usage: "记住：<偏好或事实>", Description: "在本地对话中记录个人偏好或事实记忆（不派发任务）", Mode: "memory"},
	}
}

func defaultComposerReplyStrategies() []composerReplyStrategy {
	return []composerReplyStrategy{
		{
			ID:          "role_routing",
			Label:       "按角色路由策略",
			Description: "由服务端按请求阶段自动匹配角色默认模型",
			Default:     true,
			CallsModel:  true,
			Scope:       "configured_roles",
		},
		{
			ID:          "deterministic_only",
			Label:       "仅确定性查询",
			Description: "直接读取任务/资产/日志事实，不调用生成模型",
			CallsModel:  false,
			Scope:       "deterministic_read",
		},
		{
			ID:          "single_turn_override",
			Label:       "仅本轮指定模型",
			Description: "仅覆盖当前回复角色，不影响后台任务与其它阶段路由",
			CallsModel:  true,
			Scope:       "current_turn_reply",
		},
	}
}

// composerOptions 为 Composer 提供 `/` 指令补全、`@任务` / `@成员` 引用候选项以及「模型策略」（与本轮可选模型）列表。
// 按会话身份隔离任务与成员视野，绝不回传敏感凭据或私有对话。
func (s Server) composerOptions(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "任务存储暂不可用")
		return
	}
	resp := composerOptionsResponse{
		SlashCommands:   defaultComposerSlashCommands(),
		Tasks:           []composerTaskOption{},
		Users:           []work.MentionableAccount{},
		ReplyStrategies: defaultComposerReplyStrategies(),
		ReplyModels:     []composerReplyModel{},
	}

	if s.BenchmarkProvider == nil {
		resp.SlashCommands = slices.DeleteFunc(resp.SlashCommands, func(command composerSlashCommand) bool {
			return command.Command == "/benchmark"
		})
	}
	account, hasAccount := accountFromContext(r)
	if r.URL.Query().Get("scope") == "all" && hasAccount && account.IsSuperadmin && s.AdminTaskStores != nil {
		for owner, store := range s.AdminTaskStores() {
			items, err := store.ListTasks(r.Context(), work.TaskFilter{Limit: 15})
			if err != nil {
				writeError(w, err)
				return
			}
			for _, t := range items {
				resp.Tasks = append(resp.Tasks, composerTaskOption{
					ID: t.ID, Title: t.Title, Status: t.Status, OwnerID: owner,
				})
			}
		}
		slices.SortFunc(resp.Tasks, func(a, b composerTaskOption) int {
			return cmp.Compare(a.ID, b.ID)
		})
		if len(resp.Tasks) > 30 {
			resp.Tasks = resp.Tasks[:30]
		}
	} else {
		conversation := r.URL.Query().Get("conversation_id")
		if conversation != "" && !validConversationID(conversation) {
			fail(w, http.StatusBadRequest, "invalid_conversation", "会话标识无效")
			return
		}
		references, err := s.Store.MessageTaskReferenceOptions(r.Context(), s.principal(r), conversation)
		if err != nil {
			writeError(w, err)
			return
		}
		items, err := s.Store.ListTasks(r.Context(), work.TaskFilter{Limit: 30})
		if err != nil {
			writeError(w, err)
			return
		}
		for _, t := range items {
			resp.Tasks = append(resp.Tasks, composerTaskOption{
				ID: t.ID, Title: t.Title, Status: t.Status, OwnerID: account.PrincipalID, Referenceable: references[t.ID],
			})
		}
	}

	accountStore := s.instanceStore
	if accountStore == nil {
		accountStore = s.Store
	}
	users, err := accountStore.ListMentionableAccounts(r.Context(), 50)
	if err != nil {
		writeError(w, err)
		return
	}
	resp.Users = users

	if s.Settings != nil {
		defaultProfileID := ""
		if def, err := s.Settings.DefaultModel(r.Context()); err == nil {
			defaultProfileID = def.ProfileID
		}
		if profiles, err := s.Settings.Profiles(r.Context()); err == nil {
			for _, p := range profiles {
				if !p.Enabled || !p.ProviderEnabled {
					continue
				}
				_, credentialErr := s.Settings.IsolatedReplyCredential(r.Context(), p.ID, p.Revision)
				vision, reasoning := composerModelCapabilities(p.Attributes)
				resp.ReplyModels = append(resp.ReplyModels, composerReplyModel{
					ProfileID: p.ID,
					Name:      p.Name,
					ModelID:   s.assistantSafeModelID(r.Context(), p.ID, p.ModelID),
					Brand:     inferModelBrand(p.ModelID, p.Name),
					IsDefault: p.ID == defaultProfileID,
					Revision:  p.Revision, Available: credentialErr == nil,
					Vision: vision, Reasoning: reasoning,
				})
			}
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, resp)
}
