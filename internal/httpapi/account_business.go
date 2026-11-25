package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"clusbot/internal/work"
)

type accountStreamCheckKey struct{}

// PersonalHandler binds the existing business handlers to a single account's
// store. It is reached only after the instance has authenticated the request.
func (s Server) PersonalHandler(store *work.Store) http.Handler {
	s.instanceStore = s.Store
	s.Store = store
	return s.routes()
}

func (s Server) serveAccountBusiness(w http.ResponseWriter, r *http.Request, route string, account work.Account, instance http.Handler) {
	switch route {
	case "GET /api/v1/assistant/tools", "GET /api/v1/assistant/model-routing", "GET /api/v1/assistant/connections",
		"POST /api/v1/assistant/tools", "POST /api/v1/assistant/confirm", "DELETE /api/v1/assistant/confirm/{id}",
		"POST /api/v1/assistant/interpret", "POST /api/v1/assistant/recommendations/{id}/selection":
		instance.ServeHTTP(w, r)
		return
	}
	if route == "GET /api/v1/assistant/models" {
		if !account.IsSuperadmin {
			fail(w, 403, "forbidden", "只有超级管理员可查看模型目录")
			return
		}
		instance.ServeHTTP(w, r)
		return
	}
	_, routePath, _ := strings.Cut(route, " ")
	if routePath == "/api/v1/admin/tasks" || strings.HasPrefix(routePath, "/api/v1/admin/tasks/") || routePath == "/api/v1/admin/task-notices" {
		if !account.IsSuperadmin {
			fail(w, 403, "forbidden", "只有超级管理员可管理全站任务")
			return
		}
		if s.AdminTaskStores == nil {
			fail(w, 503, "server_mode_unavailable", "全站任务管理尚未配置")
			return
		}
		// 注入已认证超管主体到上下文，确保 scope=mine/others 与通知列表严格基于服务端身份过滤。
		instance.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountContextKey{}, account)))
		return
	}
	if route == "GET /api/v1/domain/discovery/setup-status" && (!account.IsSuperadmin || s.DiscoverySource == nil) {
		instance.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(routePath, "/api/v1/domain/discovery/") && !account.IsSuperadmin {
		fail(w, 403, "forbidden", "只有超级管理员可管理服务器发现来源")
		return
	}
	if sharedConnectionRoute(route) && (route != "GET /api/v1/tool-results/{id}/content" || s.PersonalAPI == nil) {
		r.Pattern = route
		s.serveSharedConnection(w, r, account, instance)
		return
	}
	if strings.Contains(route, " /api/v1/settings/model/") || route == "GET /api/v1/settings" || route == "PATCH /api/v1/settings" || accountSkillAdminRoute(route) {
		if !account.IsSuperadmin {
			fail(w, 403, "forbidden", "只有超级管理员可管理实例设置")
			return
		}
		instance.ServeHTTP(w, r)
		return
	}
	if route == "GET /api/v1/skills" && account.IsSuperadmin {
		instance.ServeHTTP(w, r)
		return
	}
	if route == "/api/" {
		instance.ServeHTTP(w, r)
		return
	}
	if !accountPersonalRoute(route) {
		fail(w, 503, "server_mode_unavailable", "此功能在多人模式下尚不可用")
		return
	}
	if s.PersonalAPI == nil {
		fail(w, 503, "server_mode_unavailable", "服务器业务服务尚未配置")
		return
	}
	handler, err := s.PersonalAPI(account.PrincipalID)
	if err != nil {
		slog.Error("打开账号业务存储失败", "error", err)
		fail(w, 503, "personal_storage_unavailable", "个人数据暂不可用")
		return
	}
	if route == "GET /api/v1/assistant/composer-options" || route == "GET /api/v1/assistant/token-usage-stats" {
		r = r.WithContext(context.WithValue(r.Context(), accountContextKey{}, account))
	}
	if route == "GET /api/v1/runs/{id}/events" {
		digest := s.accountSessionDigest(r)
		ctx := r.Context()
		check := func() bool {
			current, err := s.Store.AccountForSession(ctx, digest)
			return err == nil && !current.MustChangePassword && current.PrincipalID == account.PrincipalID
		}
		r = r.WithContext(context.WithValue(r.Context(), accountStreamCheckKey{}, check))
	}
	handler.ServeHTTP(w, r)
}

func accountPersonalRoute(route string) bool {
	switch route {
	case "GET /api/v1/preferences", "PUT /api/v1/preferences", "GET /api/v1/settings/data/storage",
		"GET /api/v1/domain/snapshot", "GET /api/v1/domain/cluster-trends", "GET /api/v1/domain/cluster-inventory",
		"GET /api/v1/domain/benchmarks/{id}", "GET /api/v1/domain/benchmarks/{id}/report",
		"POST /api/v1/domain/benchmarks/{id}/dispatch-control", "GET /api/v1/domain/benchmarks/{id}/dispatch-control",
		"GET /api/v1/domain/benchmarks/{id}/evidence", "GET /api/v1/domain/benchmarks/{id}/evidence/{artifact}",
		"POST /api/v1/domain/asset-acceptance", "GET /api/v1/domain/asset-acceptance/{task_id}",
		"GET /api/v1/domain/discovery/setup-status", "POST /api/v1/domain/discovery/authorizations",
		"POST /api/v1/domain/discovery/plans", "DELETE /api/v1/domain/discovery/plans/{id}",
		"GET /api/v1/domain/discovery/plans/{id}/observations", "GET /api/v1/domain/discovery/candidates",
		"GET /api/v1/domain/discovery/candidates/{id}/observations", "POST /api/v1/domain/discovery/candidates/{id}/admit",
		"GET /api/v1/assistant/composer-options",
		"GET /api/v1/assistant/token-usage-stats",
		"GET /api/v1/assistant/runs/{id}/model-receipt", "GET /api/v1/overview", "GET /api/v1/skills",
		"GET /api/v1/conversations/{id}/skill-binding", "PUT /api/v1/conversations/{id}/skill-binding",
		"GET /api/v1/runs/{id}/skill-binding",
		"GET /api/v1/workspace/files", "POST /api/v1/attachments/workspace",
		"POST /api/v1/attachments", "POST /api/v1/attachments/uploads/{id}/part",
		"GET /api/v1/materials/{id}/preview", "GET /api/v1/materials/{id}/download", "DELETE /api/v1/materials/{id}",
		"GET /api/v1/incidents", "POST /api/v1/incidents", "GET /api/v1/incidents/{id}/workflow", "GET /api/v1/incidents/{id}/steps/{step}/logs",
		"GET /api/v1/memories", "POST /api/v1/memories", "GET /api/v1/memories/source/{message}",
		"PATCH /api/v1/memories/{id}", "DELETE /api/v1/memories/{id}",
		"GET /api/v1/experience-candidates", "POST /api/v1/experience-candidates/from-acceptance",
		"POST /api/v1/experience-candidates/from-correction", "POST /api/v1/experience-candidates/{id}/review",
		"GET /api/v1/experience-evaluations", "POST /api/v1/experience-evaluations",
		"GET /api/v1/capabilities/bindings", "POST /api/v1/capabilities/bindings",
		"GET /api/v1/tasks", "POST /api/v1/tasks", "GET /api/v1/tasks/{id}",
		"GET /api/v1/tasks/{id}/runs", "GET /api/v1/tasks/{id}/runs/{run_id}", "GET /api/v1/tasks/{id}/manifest",
		"PATCH /api/v1/tasks/{id}", "POST /api/v1/tasks/{id}/acceptance", "POST /api/v1/tasks/{id}/pause",
		"POST /api/v1/tasks/{id}/cancel", "POST /api/v1/tasks/{id}/resume", "POST /api/v1/tasks/{id}/rerun", "POST /api/v1/tasks/{id}/ack-read",
		"POST /api/v1/tasks/{id}/publications", "POST /api/v1/publications/{id}/decision", "POST /api/v1/publications/{id}/reconcile",
		"GET /api/v1/runtime/queue/explicit-tasks", "GET /api/v1/admissions", "GET /api/v1/admissions/{key}",
		"GET /api/v1/conversations", "POST /api/v1/conversations", "GET /api/v1/conversations/{id}",
		"GET /api/v1/conversations/history", "GET /api/v1/conversations/{id}/history", "GET /api/v1/conversations/{id}/events",
		"POST /api/v1/conversations/{id}/cancel", "PATCH /api/v1/conversations/{id}", "DELETE /api/v1/conversations/{id}",
		"POST /api/v1/conversations/{id}/messages", "POST /api/v1/conversations/{id}/resume",
		"POST /api/v1/conversations/{id}/benchmark-revision-preview",
		"GET /api/v1/conversations/{id}/benchmark-revision-requests/{request_id}",
		"GET /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}",
		"POST /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}/confirm",
		"POST /api/v1/conversations/{id}/task", "POST /api/v1/conversations/isolated-replies", "POST /api/v1/conversations/{id}/isolated-replies",
		"GET /api/v1/runs/{id}", "GET /api/v1/runs/{id}/events",
		"GET /api/v1/conversations/{id}/mcp-selection", "PUT /api/v1/conversations/{id}/mcp-selection",
		"GET /api/v1/runs/{id}/mcp-bindings", "GET /api/v1/tool-results/{id}/content",
		"GET /api/v1/artifacts", "GET /api/v1/artifacts/{id}", "GET /api/v1/artifacts/{id}/content",
		"POST /api/v1/workflows", "GET /api/v1/tasks/{id}/workflow",
		"GET /api/v1/decisions", "GET /api/v1/approvals", "POST /api/v1/approvals/{id}/decision",
		"GET /api/v1/attention", "GET /api/v1/attention/events", "GET /api/v1/attention/{id}":
		return true
	default:
		return false
	}
}

func accountSkillAdminRoute(route string) bool {
	switch route {
	case "POST /api/v1/skills", "POST /api/v1/skills/import-preview",
		"POST /api/v1/skills/import", "POST /api/v1/skills/{id}/activate", "POST /api/v1/skills/{id}/disable":
		return true
	default:
		return false
	}
}
