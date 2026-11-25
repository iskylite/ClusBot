package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"clusbot/internal/work"
)

// Surfaces are built here, not by the model or by business endpoints. They
// carry no credentials, navigation targets, or executable business actions.
const (
	assistantA2UICatalog = "https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json"
	assistantSurfaceRows = 10
	assistantSurfaceText = 200
)

type assistantA2UIMessage struct {
	Version          string                         `json:"version"`
	CreateSurface    *assistantA2UICreateSurface    `json:"createSurface,omitempty"`
	UpdateComponents *assistantA2UIUpdateComponents `json:"updateComponents,omitempty"`
}

type assistantA2UICreateSurface struct {
	SurfaceID string `json:"surfaceId"`
	CatalogID string `json:"catalogId"`
}

type assistantA2UIUpdateComponents struct {
	SurfaceID  string                   `json:"surfaceId"`
	Components []assistantA2UIComponent `json:"components"`
}

type assistantA2UIComponent struct {
	ID        string               `json:"id"`
	Component string               `json:"component"`
	Children  []string             `json:"children,omitempty"`
	Text      string               `json:"text,omitempty"`
	Child     string               `json:"child,omitempty"`
	Variant   string               `json:"variant,omitempty"`
	Action    *assistantA2UIAction `json:"action,omitempty"`
}

type assistantA2UIAction struct {
	Event assistantA2UIEvent `json:"event"`
}

type assistantA2UIEvent struct {
	Name string `json:"name"`
}

func assistantToolLabel(name string) string {
	forms := map[string]string{
		"configure_model_provider": "配置模型提供方",
		"configure_model_profile":  "配置模型档案",
		"configure_model_role":     "配置模型角色",
		"configure_mcp_connection": "配置 MCP 连接",
		"configure_mcp_permission": "配置 MCP 授权",
		"manage_accounts":          "管理账号",
		"change_my_password":       "修改密码",
	}
	if label := forms[name]; label != "" {
		return label
	}
	for _, tool := range registeredAssistantTools() {
		if tool.Name != name {
			continue
		}
		label := tool.Description
		if i := strings.IndexAny(label, "。；;"); i >= 0 {
			label = label[:i]
		}
		return label
	}
	return "助手工具"
}

// Dynamic strings come from fixed result fields. Removing URL-like text and
// Markdown link delimiters keeps a Basic catalog Text from creating navigation.
var assistantSurfaceURL = regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+`)

func assistantSurfaceValue(value string) string {
	value = assistantSurfaceURL.ReplaceAllString(value, "（链接已省略）")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		switch r {
		case '[':
			return '［'
		case ']':
			return '］'
		case '(':
			return '（'
		case ')':
			return '）'
		case '<':
			return '＜'
		case '>':
			return '＞'
		}
		return r
	}, value)
	return strings.TrimSpace(value)
}

func assistantSurfaceClip(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > assistantSurfaceText {
		return string(runes[:assistantSurfaceText-1]) + "…"
	}
	return string(runes)
}

func assistantSurface(lines []string, details bool) []assistantA2UIMessage {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil
	}
	surfaceID := "assistant_" + hex.EncodeToString(nonce[:])
	components := []assistantA2UIComponent{{ID: "root", Component: "Column"}}
	for _, line := range lines {
		line = assistantSurfaceClip(line)
		if line == "" {
			continue
		}
		id := fmt.Sprintf("text_%d", len(components)-1)
		components[0].Children = append(components[0].Children, id)
		components = append(components, assistantA2UIComponent{ID: id, Component: "Text", Text: line})
	}
	if details {
		components[0].Children = append(components[0].Children, "details_button")
		components = append(components,
			assistantA2UIComponent{ID: "details_button", Component: "Button", Child: "details_label", Variant: "borderless",
				Action: &assistantA2UIAction{Event: assistantA2UIEvent{Name: "toggle_details"}}},
			assistantA2UIComponent{ID: "details_label", Component: "Text", Text: "展开详情"})
	}
	return []assistantA2UIMessage{
		{Version: "1.0", CreateSurface: &assistantA2UICreateSurface{SurfaceID: surfaceID, CatalogID: assistantA2UICatalog}},
		{Version: "1.0", UpdateComponents: &assistantA2UIUpdateComponents{SurfaceID: surfaceID, Components: components}},
	}
}

func assistantPreviewSurface(tool string) []assistantA2UIMessage {
	// Confirmation parameters and tokens remain in the established envelope;
	// the host's fixed confirmation UI is responsible for reviewing them.
	return assistantSurface([]string{"操作预览", assistantToolLabel(tool), "请核对操作内容后确认。"}, false)
}

type assistantSurfaceTask struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	Scope  string `json:"scope_label"`
}

type assistantSurfaceAttention struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	ObjectRef string `json:"object_ref"`
	Action    string `json:"action"`
}

type assistantSurfaceDiscovery struct {
	Source  string `json:"source"`
	Scope   string `json:"scope"`
	Serial  string `json:"serial"`
	Address string `json:"address"`
	State   string `json:"state"`
}

func assistantSurfaceParts(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value = assistantSurfaceValue(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func assistantSurfaceRowsFrom[T any](header, empty string, items []T, row func(T) string) []string {
	lines := []string{header}
	for _, item := range items[:min(len(items), assistantSurfaceRows)] {
		if text := row(item); text != "" {
			lines = append(lines, text)
		}
	}
	if len(items) == 0 {
		lines = append(lines, empty)
	} else if len(items) > assistantSurfaceRows {
		lines = append(lines, "仅显示前 10 条。")
	}
	return lines
}

func assistantTaskRow(task assistantSurfaceTask) string {
	return assistantSurfaceParts(task.Title, task.Status, task.Scope)
}

func assistantAttentionRow(item assistantSurfaceAttention) string {
	return assistantSurfaceParts(item.Kind, item.State, item.ObjectRef, item.Action)
}

func assistantDiscoveryRow(item assistantSurfaceDiscovery) string {
	return assistantSurfaceParts(item.Serial, item.Address, item.State, item.Source, item.Scope)
}

func assistantModelRole(value string) string {
	return assistantEnum(value, "未知角色", map[string]string{
		"general_router": "通用路由", "conversation_reply": "对话回复", "task_worker": "任务执行",
		"benchmark_admission": "基准受理", "benchmark_revision": "基准方案修订",
		"benchmark_execution": "基准执行建议", "deterministic": "确定性处理",
	})
}

func assistantModelSource(value string) string {
	return assistantEnum(value, "来源未知", map[string]string{
		"global_default": "全局默认模型", "explicit_profile": "专用档案", "go": "内置逻辑",
	})
}

func assistantModelStatus(value string) string {
	return assistantEnum(value, "状态未知", map[string]string{
		"configured": "本地已配置", "locally_configured": "专用档案已绑定",
		"not_configured": "未配置", "settings_unavailable": "设置不可用", "default_unavailable": "默认模型不可用",
		"local_demo": "本地演示", "demo_role_unavailable": "演示模式暂不支持此角色",
		"demo_profile_disabled": "演示模式不启用专用档案", "model_not_used": "无需模型",
		"profile_missing": "绑定的档案已不存在", "profile_revision_changed": "档案版本已变更，需要重新绑定",
		"profile_invalid": "档案配置不可用", "profile_disabled": "档案已停用", "provider_disabled": "提供方已停用",
		"credential_missing": "凭据不可用", "credential_changed": "凭据已变化，需要重新绑定",
		"invalid_configuration": "配置不可用", "default_destination_mismatch": "与默认模型的连接不一致",
	})
}

func assistantModelReceiptStatus(value string) string {
	return assistantEnum(value, "回执无法核对", map[string]string{
		"reported": "提供方已报告", "not_reported": "未报告模型标识", "conflicting": "报告互相冲突，无法确认",
		"not_observed": "未观察到提供方回执", "selection_unavailable": "模型选择不可用",
	})
}

func assistantEnum(value, fallback string, labels map[string]string) string {
	if label := labels[value]; label != "" {
		return label
	}
	return fallback
}

func assistantResultSurface(tool, method string, raw []byte) []assistantA2UIMessage {
	switch tool {
	case "get_current_model_routing":
		var result struct {
			Roles []struct {
				Role, Source, Status string
				ModelID              string `json:"model_id"`
			} `json:"roles"`
		}
		if json.Unmarshal(raw, &result) == nil {
			lines := assistantSurfaceRowsFrom("当前模型配置", "暂无模型配置", result.Roles, func(role struct {
				Role, Source, Status string
				ModelID              string `json:"model_id"`
			}) string {
				model := ""
				if work.ValidModelReceiptIdentifier(role.ModelID) {
					model = role.ModelID
				}
				return assistantSurfaceParts(assistantModelRole(role.Role), model, assistantModelSource(role.Source), assistantModelStatus(role.Status))
			})
			lines = append(lines, "这是当前配置，不代表某次请求的实际回执。")
			return assistantSurface(lines, false)
		}
	case "list_available_models":
		var models []struct {
			Name    string `json:"name"`
			ModelID string `json:"model_id"`
			Enabled bool   `json:"enabled"`
		}
		if json.Unmarshal(raw, &models) == nil {
			lines := assistantSurfaceRowsFrom("模型档案", "暂无模型档案", models, func(model struct {
				Name    string `json:"name"`
				ModelID string `json:"model_id"`
				Enabled bool   `json:"enabled"`
			}) string {
				id, state := "", "已停用"
				if work.ValidModelReceiptIdentifier(model.ModelID) {
					id = model.ModelID
				}
				if model.Enabled {
					state = "已启用"
				}
				return assistantSurfaceParts(model.Name, id, state)
			})
			return assistantSurface(lines, false)
		}
	case "get_run_model_receipt":
		var result struct {
			RunStatus string `json:"run_status"`
			Receipts  []struct {
				Generation     int                `json:"execution_generation"`
				RequestedModel string             `json:"requested_model"`
				Receipt        *work.ModelReceipt `json:"model_receipt"`
			} `json:"receipts"`
		}
		if json.Unmarshal(raw, &result) == nil {
			lines := assistantSurfaceRowsFrom("模型请求回执", "暂无模型请求回执", result.Receipts, func(entry struct {
				Generation     int                `json:"execution_generation"`
				RequestedModel string             `json:"requested_model"`
				Receipt        *work.ModelReceipt `json:"model_receipt"`
			}) string {
				requested, reported, state := "", "", "未记录提供方回执"
				if work.ValidModelReceiptIdentifier(entry.RequestedModel) {
					requested = "请求模型：" + entry.RequestedModel
				}
				if entry.Receipt != nil {
					state = "回执状态：" + assistantModelReceiptStatus(entry.Receipt.State)
					if work.ValidModelReceiptIdentifier(entry.Receipt.ReportedModel) {
						reported = "提供方报告：" + entry.Receipt.ReportedModel
					}
				}
				return assistantSurfaceParts(fmt.Sprintf("执行代次 %d", entry.Generation), requested, reported, state)
			})
			lines = append(lines, "提供方报告不等于独立验证。")
			return assistantSurface(lines, false)
		}
	case "get_admission_model_receipt":
		var result struct {
			State       string                      `json:"state"`
			RouterModel *work.RouterModelProvenance `json:"router_model"`
		}
		if json.Unmarshal(raw, &result) == nil {
			state := assistantEnum(result.State, "状态未知", map[string]string{"received": "已受理", "applying": "处理中", "applied": "已处理", "quarantined": "内容冲突待核对", "failed": "处理失败"})
			lines := []string{"受理模型回执", "受理状态：" + state}
			if model := result.RouterModel; model != nil {
				if work.ValidModelReceiptIdentifier(model.RequestedModel) {
					lines = append(lines, "请求模型："+model.RequestedModel)
				}
				if work.ValidModelReceiptIdentifier(model.ReportedModel) {
					lines = append(lines, "提供方报告："+model.ReportedModel)
				}
				lines = append(lines, "回执状态："+assistantModelReceiptStatus(model.State))
			} else {
				lines = append(lines, "暂无模型回执。")
			}
			lines = append(lines, "提供方报告不等于独立验证。")
			return assistantSurface(lines, false)
		}
	case "list_tasks", "admin_list_tasks":
		var tasks []assistantSurfaceTask
		if tool == "admin_list_tasks" {
			var page struct {
				Items []assistantSurfaceTask `json:"items"`
			}
			if json.Unmarshal(raw, &page) != nil {
				break
			}
			tasks = page.Items
		} else if json.Unmarshal(raw, &tasks) != nil {
			break
		}
		return assistantSurface(assistantSurfaceRowsFrom("任务列表", "暂无任务", tasks, assistantTaskRow), tool == "list_tasks" && len(tasks) > 0)
	case "admin_list_task_notices":
		var page struct {
			Items []struct {
				Owner  string `json:"owner_id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &page) != nil {
			break
		}
		// 跨账号只读动态卡片：系统中仅一位超级管理员；对其他成员任务逐条独立呈现“来自谁 / 任务标题 / 状态”（不聚合防漏看），
		// 不默认展开详情、不带并发写按钮，避免与原任务负责人产生版本竞争。
		lines := assistantSurfaceRowsFrom("跨账号任务动态（只读 · 逐条）", "暂无其他成员任务动态", page.Items, func(item struct {
			Owner  string `json:"owner_id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		}) string {
			state := assistantEnum(item.Status, "状态未知", map[string]string{
				"active": "运行中", "waiting_user": "待处理", "interrupted": "已中断", "completed": "已完成", "cancelled": "已取消",
			})
			return assistantSurfaceParts(item.Owner, item.Title, state)
		})
		return assistantSurface(lines, false)
	case "list_artifacts":
		var artifacts []struct {
			Name       string `json:"name"`
			AcceptedAt int64  `json:"accepted_at"`
		}
		if json.Unmarshal(raw, &artifacts) == nil {
			lines := assistantSurfaceRowsFrom("成果列表", "暂无成果", artifacts, func(artifact struct {
				Name       string `json:"name"`
				AcceptedAt int64  `json:"accepted_at"`
			}) string {
				state := "尚未验收"
				if artifact.AcceptedAt > 0 {
					state = "已验收"
				}
				return assistantSurfaceParts(artifact.Name, state)
			})
			return assistantSurface(lines, false)
		}
	case "list_skills":
		var skills []struct {
			Scope   string `json:"scope_label"`
			Version int    `json:"version"`
			Active  bool   `json:"active"`
		}
		if json.Unmarshal(raw, &skills) == nil {
			lines := assistantSurfaceRowsFrom("Skill 目录", "暂无 Skill", skills, func(skill struct {
				Scope   string `json:"scope_label"`
				Version int    `json:"version"`
				Active  bool   `json:"active"`
			}) string {
				state := "未启用"
				if skill.Active {
					state = "已启用"
				}
				return assistantSurfaceParts(skill.Scope, fmt.Sprintf("版本 %d", skill.Version), state)
			})
			return assistantSurface(lines, false)
		}
	case "list_mcp_connections":
		var connections []struct {
			Name     string `json:"name"`
			Revision int    `json:"revision"`
			Enabled  bool   `json:"enabled"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(raw, &connections) == nil {
			lines := assistantSurfaceRowsFrom("MCP 连接", "暂无 MCP 连接", connections, func(connection struct {
				Name     string `json:"name"`
				Revision int    `json:"revision"`
				Enabled  bool   `json:"enabled"`
				Status   string `json:"status"`
			}) string {
				state := "待测试"
				if !connection.Enabled {
					state = "已停用"
				} else if connection.Status == "ready" {
					state = "已就绪"
				} else if connection.Status == "failed" {
					state = "测试失败"
				}
				return assistantSurfaceParts(connection.Name, fmt.Sprintf("版本 %d", connection.Revision), state)
			})
			return assistantSurface(lines, false)
		}
	case "get_task", "admin_get_task":
		var task assistantSurfaceTask
		if tool == "get_task" {
			var detail struct {
				Task *assistantSurfaceTask `json:"task"`
			}
			if json.Unmarshal(raw, &detail) != nil || detail.Task == nil {
				break
			}
			task = *detail.Task
		} else if json.Unmarshal(raw, &task) != nil {
			break
		}
		return assistantSurface([]string{"任务详情", assistantTaskRow(task)}, false)
	case "list_attention":
		var page struct {
			Items []assistantSurfaceAttention `json:"items"`
		}
		if json.Unmarshal(raw, &page) == nil {
			return assistantSurface(assistantSurfaceRowsFrom("待处理事项", "暂无待处理事项", page.Items, assistantAttentionRow), len(page.Items) > 0)
		}
	case "get_attention":
		var item assistantSurfaceAttention
		if json.Unmarshal(raw, &item) == nil {
			return assistantSurface([]string{"待处理事项", assistantAttentionRow(item)}, false)
		}
	case "list_discovery_candidates":
		var items []assistantSurfaceDiscovery
		if json.Unmarshal(raw, &items) == nil {
			return assistantSurface(assistantSurfaceRowsFrom("发现候选", "暂无发现候选", items, assistantDiscoveryRow), len(items) > 0)
		}
	case "get_discovery_observations":
		var page struct {
			Source string `json:"source"`
			Scope  string `json:"scope"`
			Items  []struct {
				Address string `json:"address"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &page) == nil {
			lines := assistantSurfaceRowsFrom("发现观测", "暂无发现观测", page.Items, func(item struct {
				Address string `json:"address"`
			}) string {
				return assistantSurfaceParts(item.Address)
			})
			if source := assistantSurfaceParts(page.Source, page.Scope); source != "" {
				lines = append(lines, "来源与范围："+source)
			}
			return assistantSurface(lines, false)
		}
	case "get_discovery_setup_status":
		var result struct {
			State   string                          `json:"state"`
			Plan    *struct{ Source, Scope string } `json:"plan"`
			LastRun *struct {
				Status string `json:"status"`
			} `json:"last_run"`
		}
		if json.Unmarshal(raw, &result) == nil {
			lines := []string{"资产发现配置", "状态：" + assistantSurfaceValue(result.State)}
			if result.Plan != nil {
				lines = append(lines, assistantSurfaceParts(result.Plan.Source, result.Plan.Scope))
			}
			if result.LastRun != nil {
				lines = append(lines, "最近执行："+assistantSurfaceValue(result.LastRun.Status))
			}
			return assistantSurface(lines, false)
		}
	}
	if method == "GET" {
		return assistantSurface([]string{assistantToolLabel(tool), "查询已返回，请查看结果。"}, false)
	}
	return assistantSurface([]string{"操作回执", assistantToolLabel(tool), "请求已处理，具体状态以业务结果为准。"}, false)
}
