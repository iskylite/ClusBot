package httpapi

import (
	"cmp"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"clusbot/internal/work"
)

type adminTask struct {
	Owner          string `json:"owner_id"`
	ID             string `json:"id"`
	Title          string `json:"title"`
	Goal           string `json:"goal"`
	Scope          string `json:"scope_label"`
	Criteria       string `json:"acceptance_criteria,omitempty"`
	RequiredFile   string `json:"required_filename,omitempty"`
	Revision       int    `json:"revision"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason"`
	Priority       string `json:"priority"`
	StateChangedAt int64  `json:"state_changed_at"`
	UpdatedAt      int64  `json:"updated_at"`
	CompletedAt    *int64 `json:"completed_at,omitempty"`
}

func (s Server) registerAdminTasks(api *http.ServeMux) {
	if !s.ServerMode {
		return
	}
	const list = "GET /api/v1/admin/tasks"
	const notices = "GET /api/v1/admin/task-notices"
	const path = "/api/v1/admin/tasks/{owner}/{id}"
	const task = "GET " + path
	api.HandleFunc(list, s.adminTasks)
	// 只读跨账号精简动态接口：供超管会话展示“其他成员任务动态（单条只读提示，不提供并发写操作）”。
	api.HandleFunc(notices, s.adminTaskNotices)
	api.HandleFunc(task, s.adminTaskRead("detail"))
	api.HandleFunc(task+"/runs", s.adminTaskRead("runs"))
	api.HandleFunc(task+"/artifacts", s.adminTaskRead("artifacts"))
	api.HandleFunc(task+"/artifacts/{artifact}/content", s.adminTaskRead("content"))
	api.HandleFunc("PATCH "+path, s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.editTask }))
	api.HandleFunc("POST "+path+"/pause", s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.control("pause") }))
	api.HandleFunc("POST "+path+"/cancel", s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.control("stop") }))
	api.HandleFunc("POST "+path+"/resume", s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.resume }))
	api.HandleFunc("POST "+path+"/acceptance", s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.accept }))
}

// adminTaskNotice 为单超级管理员在个人会话中提供跨账号任务的只读精简动态。
// 系统中仅存在一位超级管理员；此处每条成员任务独立返回（不聚合打包、不默认展开全貌、也不携带并发写操作按钮），
// 既避免超管在会话内漏看单条任务变化，又防止超管与原任务所有者并发修改引发版本（revision）竞争；
// 需要查证或治理时可通过前端「查看详情」或前往统一任务管理页处理。
type adminTaskNotice struct {
	Owner        string `json:"owner_id"`
	TaskID       string `json:"task_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`
	Revision     int    `json:"revision"`
	UpdatedAt    int64  `json:"updated_at"`
	NoticeOnly   bool   `json:"notice_only"`
}

func adminTaskView(owner string, task work.Task) adminTask {
	// The task definition belongs in this view. Conversation links and
	// free-form runtime reasons can reference private inputs.
	return adminTask{
		Owner: owner, ID: task.ID, Title: task.Title, Goal: task.Goal, Scope: task.Scope, Criteria: task.Criteria,
		Status: task.Status, StatusReason: task.StatusReason, Priority: task.Priority,
		Revision: task.Revision, RequiredFile: task.RequiredFile,
		StateChangedAt: task.StateChangedAt, UpdatedAt: task.UpdatedAt, CompletedAt: task.CompletedAt,
	}
}

func (s Server) adminTasks(w http.ResponseWriter, r *http.Request) {
	filter, ok := readTaskFilter(w, r)
	if !ok {
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "mine" && scope != "others" {
		fail(w, http.StatusBadRequest, "invalid_scope", "任务范围参数无效，仅支持 all、mine 或 others")
		return
	}
	ownerFilter := r.URL.Query().Get("owner")
	if ownerFilter != "" && !validTaskOwner(ownerFilter) {
		fail(w, http.StatusBadRequest, "invalid_owner", "负责人标识无效")
		return
	}
	account, hasAccount := accountFromContext(r)
	if (scope == "mine" || scope == "others") && !hasAccount {
		fail(w, http.StatusUnauthorized, "unauthorized", "当前请求缺少操作人身份")
		return
	}

	limit := filter.Limit
	if limit == 0 {
		limit = 50
	}
	// Fetch at most the requested page per owner. A full page with no extra
	// rows may offer one final cursor that returns an empty page.
	filter.Limit = limit
	var cursorOwner string
	if after := r.URL.Query().Get("after"); after != "" {
		parts := strings.Split(after, "/")
		if len(parts) != 2 || !validTaskOwner(parts[0]) {
			fail(w, 400, "invalid_cursor", "事项游标无效")
			return
		}
		cursor, ok := parseTaskCursor(parts[1])
		if !ok {
			fail(w, 400, "invalid_cursor", "事项游标无效")
			return
		}
		cursorOwner = parts[0]
		filter.AfterUpdatedAt, filter.AfterID = cursor.Timestamp, cursor.ID
	}
	page := make([]adminTask, 0, limit)
	for owner, store := range s.AdminTaskStores() {
		// 按认证主体与显式 owner 筛选跨账号存储：避免超管自身视角被他人任务干扰，也禁止客户端伪造 mine 身份。
		if ownerFilter != "" && owner != ownerFilter {
			continue
		}
		if scope == "mine" && owner != account.PrincipalID {
			continue
		}
		if scope == "others" && owner == account.PrincipalID {
			continue
		}
		local := filter
		if cursorOwner != "" {
			// IDs are lowercase hex. At the cursor timestamp, retain all
			// IDs from later owners and none from earlier owners.
			switch {
			case owner < cursorOwner:
				local.AfterID = "~"
			case owner > cursorOwner:
				local.AfterID = "!"
			}
		}
		tasks, err := store.ListTasks(r.Context(), local)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, task := range tasks {
			page = append(page, adminTaskView(owner, task))
		}
		slices.SortFunc(page, func(a, b adminTask) int {
			return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.ID, b.ID))
		})
		page = page[:min(limit, len(page))]
	}
	out := struct {
		Items []adminTask `json:"items"`
		Next  string      `json:"next_cursor,omitempty"`
	}{Items: page}
	if len(page) == limit {
		last := page[len(page)-1]
		out.Next = last.Owner + "/" + strconv.FormatInt(last.UpdatedAt, 10) + ":" + last.ID
	}
	jsonResponse(w, 200, out)
}

// adminTaskNotices 返回单超级管理员会话内可用的跨账号只读精简动态。
// 默认 scope=others（排除超管本人任务），按每条成员任务独立返回列表（不聚合、不默认展开、notice_only=true，绝不含私有对话）。
func (s Server) adminTaskNotices(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "others"
	}
	if scope != "all" && scope != "others" {
		fail(w, http.StatusBadRequest, "invalid_scope", "通知范围参数无效，仅支持 others 或 all")
		return
	}
	account, hasAccount := accountFromContext(r)
	if scope == "others" && !hasAccount {
		fail(w, http.StatusUnauthorized, "unauthorized", "当前请求缺少操作人身份")
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 50 {
			fail(w, http.StatusBadRequest, "invalid_limit", "通知条数限制必须在 1 到 50 之间")
			return
		}
		limit = n
	}

	notices := make([]adminTaskNotice, 0, limit)
	filter := work.TaskFilter{Limit: limit}
	for owner, store := range s.AdminTaskStores() {
		if scope == "others" && owner == account.PrincipalID {
			continue
		}
		tasks, err := store.ListTasks(r.Context(), filter)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, task := range tasks {
			notices = append(notices, adminTaskNotice{
				Owner:        owner,
				TaskID:       task.ID,
				Title:        task.Title,
				Status:       task.Status,
				StatusReason: task.StatusReason,
				Revision:     task.Revision,
				UpdatedAt:    task.UpdatedAt,
				NoticeOnly:   true,
			})
		}
		slices.SortFunc(notices, func(a, b adminTaskNotice) int {
			return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.TaskID, b.TaskID))
		})
		notices = notices[:min(limit, len(notices))]
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, struct {
		Items      []adminTaskNotice `json:"items"`
		NoticeOnly bool              `json:"notice_only"`
	}{Items: notices, NoticeOnly: true})
}

func validTaskOwner(owner string) bool {
	id, ok := strings.CutPrefix(owner, "user:")
	return ok && validID(id)
}

func (s Server) adminTaskRead(view string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner, id := r.PathValue("owner"), r.PathValue("id")
		store := s.AdminTaskStores()[owner]
		if store == nil || !validID(id) {
			http.NotFound(w, r)
			return
		}
		task, err := store.ReadTaskSummary(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		after := r.URL.Query().Get("after")
		if after != "" && !validID(after) {
			fail(w, 400, "invalid_cursor", "事项游标无效")
			return
		}
		switch view {
		case "detail":
			jsonResponse(w, 200, adminTaskView(owner, task))
		case "runs":
			items, err := store.ListTaskExecutionStates(r.Context(), id, after)
			if err != nil {
				writeError(w, err)
				return
			}
			jsonResponse(w, 200, items)
		case "artifacts":
			items, err := store.Artifacts(id, "", after)
			if err != nil {
				writeError(w, err)
				return
			}
			jsonResponse(w, 200, items)
		case "content":
			artifact, err := store.Artifact(r.PathValue("artifact"))
			if err != nil {
				writeError(w, err)
				return
			}
			if artifact.TaskID != id {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Name}))
			w.Write([]byte(artifact.Content))
		}
	}
}

func (s Server) adminTaskMutation(action func(Server) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := r.PathValue("owner")
		store := s.AdminTaskStores()[owner]
		if store == nil || !validID(r.PathValue("id")) {
			http.NotFound(w, r)
			return
		}
		scoped := s
		scoped.Store, scoped.adminTaskOwner = store, owner
		action(scoped)(w, r)
	}
}

// Cross-account mutations return the same restricted projection as admin reads.
func (s Server) taskMutationDetail(w http.ResponseWriter, r *http.Request) {
	if s.adminTaskOwner == "" {
		s.detail(true)(w, r)
		return
	}
	task, err := s.Store.ReadTaskSummary(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, adminTaskView(s.adminTaskOwner, task))
}
