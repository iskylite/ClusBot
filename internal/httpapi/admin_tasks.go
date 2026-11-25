package httpapi

import (
	"cmp"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"clusbot/internal/work"
)

type adminTask struct {
	Owner          string                          `json:"owner_id"`
	ID             string                          `json:"id"`
	Title          string                          `json:"title"`
	Goal           string                          `json:"goal"`
	Scope          string                          `json:"scope_label"`
	Criteria       string                          `json:"acceptance_criteria,omitempty"`
	RequiredFile   string                          `json:"required_filename,omitempty"`
	Revision       int                             `json:"revision"`
	Status         string                          `json:"status"`
	StatusReason   string                          `json:"status_reason"`
	Priority       string                          `json:"priority"`
	StateChangedAt int64                           `json:"state_changed_at"`
	UpdatedAt      int64                           `json:"updated_at"`
	CompletedAt    *int64                          `json:"completed_at,omitempty"`
	Execution      *work.AdminTaskExecutionSummary `json:"execution_summary,omitempty"`
	CanRerun       bool                            `json:"can_rerun,omitempty"`
	CanRename      bool                            `json:"can_rename"`
	CanCancel      bool                            `json:"can_cancel"`
	StatusAction   string                          `json:"status_action,omitempty"`
}

func (s Server) registerAdminTasks(api *http.ServeMux) {
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
	api.HandleFunc("POST "+path+"/rerun", s.adminTaskMutation(func(scoped Server) http.HandlerFunc { return scoped.adminRerunCompletedTask }))
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
	filter.MatchID = true
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
	scopeCounts := map[string]int{"all": 0, "mine": 0, "others": 0}
	statusCounts := make(map[string]int)
	stores := s.AdminTaskStores()
	for owner, store := range stores {
		unfiltered, err := store.AdminTaskStatusCounts(r.Context(), work.TaskFilter{})
		if err != nil {
			writeError(w, err)
			return
		}
		for _, count := range unfiltered {
			scopeCounts["all"] += count
			if owner == account.PrincipalID {
				scopeCounts["mine"] += count
			} else {
				scopeCounts["others"] += count
			}
		}
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
		counts := unfiltered
		if filter.Scope != "" || filter.Query != "" {
			counts, err = store.AdminTaskStatusCounts(r.Context(), filter)
			if err != nil {
				writeError(w, err)
				return
			}
		}
		for status, count := range counts {
			statusCounts[status] += count
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
		tasks, err := store.ListAdminTasks(r.Context(), local)
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
		page = page[:min(limit+1, len(page))]
	}
	hasMore := len(page) > limit
	page = page[:min(limit, len(page))]
	byOwner := make(map[string][]int)
	for i, item := range page {
		byOwner[item.Owner] = append(byOwner[item.Owner], i)
	}
	for owner, entries := range byOwner {
		ids := make([]string, 0, len(entries))
		for _, i := range entries {
			ids = append(ids, page[i].ID)
		}
		summaries, err := stores[owner].AdminTaskExecutionSummaries(r.Context(), ids)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, i := range entries {
			if summary, ok := summaries[page[i].ID]; ok {
				page[i].Execution = &summary
			}
			task, err := stores[owner].ReadTaskSummary(r.Context(), page[i].ID)
			if err != nil {
				writeError(w, err)
				return
			}
			actions, err := stores[owner].TaskListActions(r.Context(), task)
			if err != nil {
				writeError(w, err)
				return
			}
			page[i].CanRename, page[i].CanCancel, page[i].StatusAction = actions.CanRename, actions.CanCancel, actions.StatusAction
			page[i].CanRerun = actions.StatusAction == "rerun"
		}
	}
	total := statusCounts[filter.Status]
	if filter.Status == "" {
		for _, count := range statusCounts {
			total += count
		}
	}
	out := struct {
		Items        []adminTask    `json:"items"`
		Next         string         `json:"next_cursor,omitempty"`
		ScopeCounts  map[string]int `json:"scope_counts"`
		StatusCounts map[string]int `json:"status_counts"`
		Total        int            `json:"total"`
	}{Items: page, ScopeCounts: scopeCounts, StatusCounts: statusCounts, Total: total}
	if hasMore {
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
			out := adminTaskView(owner, task)
			actions, err := store.TaskListActions(r.Context(), task)
			if err != nil {
				writeError(w, err)
				return
			}
			out.CanRename, out.CanCancel, out.StatusAction = actions.CanRename, actions.CanCancel, actions.StatusAction
			out.CanRerun = actions.StatusAction == "rerun"
			jsonResponse(w, 200, out)
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

func (s Server) adminRerunCompletedTask(w http.ResponseWriter, r *http.Request) {
	s.rerunCompletedTask(w, r, "admin-task-rerun:")
}

func (s Server) personalRerunCompletedTask(w http.ResponseWriter, r *http.Request) {
	s.rerunCompletedTask(w, r, "task-rerun:")
}

func (s Server) rerunCompletedTask(w http.ResponseWriter, r *http.Request, keyPrefix string) {
	var in struct {
		Revision  int    `json:"revision"`
		RequestID string `json:"client_request_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Revision < 1 || len(in.RequestID) < 8 || len(in.RequestID) > 128 || strings.TrimSpace(in.RequestID) != in.RequestID {
		fail(w, http.StatusBadRequest, "invalid_submission", "需要有效的任务版本和请求标识")
		return
	}
	key := keyPrefix + s.principal(r) + ":" + in.RequestID
	run, err := s.Store.ReadCompletedTaskRerun(r.Context(), r.PathValue("id"), key, in.Revision)
	if err == nil && run.ID == "" {
		if !s.Settings.RoleAvailable(r.Context(), work.TaskWorkerModelRole) {
			fail(w, http.StatusServiceUnavailable, "model_not_configured", "请先配置可用的事项执行模型")
			return
		}
		run, err = s.Store.RerunCompletedTask(r.Context(), r.PathValue("id"), key, in.Revision)
	}
	if err != nil {
		if errors.Is(err, work.ErrCompletedTaskRerunUnsupported) {
			fail(w, http.StatusConflict, "task_rerun_unavailable", err.Error())
			return
		}
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{"task_id": run.TaskID, "run_id": run.ID, "status": run.Status})
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
