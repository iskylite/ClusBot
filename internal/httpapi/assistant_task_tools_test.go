package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestAssistantTaskToolRoutesAndConfirmations(t *testing.T) {
	// Check the executable route rather than only the catalog spelling. The
	// admin routes are registered by the server-mode mux.
	mux := (Server{ServerMode: true}).routes()
	seen := make(map[string]bool)
	for _, tool := range assistantTaskTools() {
		if tool.Name == "" || tool.Description == "" || seen[tool.Name] {
			t.Errorf("invalid or duplicated tool: %+v", tool)
		}
		seen[tool.Name] = true
		if tool.Method != http.MethodGet && tool.Method != http.MethodPost && tool.Method != http.MethodPatch {
			t.Errorf("unexpected method for %s: %q", tool.Name, tool.Method)
		}
		if tool.Confirm != (tool.Method != http.MethodGet) {
			t.Errorf("wrong confirmation policy for %s", tool.Name)
		}
		if strings.Contains(tool.Path, "/conversations") {
			t.Errorf("conversation route exposed as a task tool: %s", tool.Path)
		}
		path := assistantPathParameter.ReplaceAllString(tool.Path, "0123456789abcdef0123456789abcdef")
		request := httptest.NewRequest(tool.Method, path, nil)
		_, pattern := mux.Handler(request)
		if want := tool.Method + " " + tool.Path; pattern != want {
			t.Errorf("%s maps to %q, want %q", tool.Name, pattern, want)
		}
	}
	for _, name := range []string{
		"list_tasks", "get_task", "list_task_runs", "create_task", "accept_task", "list_artifacts", "get_artifact",
		"admin_list_tasks", "admin_get_task", "admin_list_task_runs", "admin_list_task_artifacts", "admin_get_task_artifact_content", "admin_accept_task",
		"get_task_workflow", "create_workflow", "list_goals", "create_goal", "stop_goal", "list_attention", "get_attention", "decide_approval",
		"list_assets", "query_asset_acceptance", "get_asset_acceptance", "lend_asset", "return_asset",
	} {
		if !seen[name] {
			t.Errorf("missing task capability %q", name)
		}
	}
}

func TestAssistantTaskToolAccountAndModeScope(t *testing.T) {
	requestFor := func(account work.Account) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		return r.WithContext(context.WithValue(r.Context(), accountContextKey{}, account))
	}
	regular := requestFor(work.Account{PrincipalID: "user:regular"})
	admin := requestFor(work.Account{PrincipalID: "user:admin", IsSuperadmin: true})
	server := Server{ServerMode: true}
	local := Server{}
	for _, tool := range assistantTaskTools() {
		adminOnly := strings.HasPrefix(tool.Path, "/api/v1/admin/")
		localOnly := strings.HasPrefix(tool.Path, "/api/v1/loans")
		if got, want := server.assistantAllowed(regular, tool), !adminOnly && !localOnly; got != want {
			t.Errorf("regular account availability for %s: got %v, want %v", tool.Name, got, want)
		}
		if got, want := server.assistantAllowed(admin, tool), !localOnly; got != want {
			t.Errorf("admin availability for %s: got %v, want %v", tool.Name, got, want)
		}
		if got, want := local.assistantAllowed(regular, tool), !adminOnly; got != want {
			t.Errorf("local availability for %s: got %v, want %v", tool.Name, got, want)
		}
	}
}
