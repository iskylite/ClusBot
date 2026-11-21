package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	"clusbot/internal/work"
)

// These handlers are intentionally unregistered until the surrounding task,
// file, control and event routes enforce the same per-principal boundary.
// The authenticated principal comes from the session rechecked in the same
// transaction, not request JSON.
func (s Server) teamTaskResponsibility(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	taskID := r.PathValue("id")
	if !validID(taskID) {
		fail(w, http.StatusNotFound, "task_not_found", "任务不存在或无权访问")
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := s.Store.ReadTeamTaskResponsibilityForSession(r.Context(), s.accountSessionDigest(r), taskID)
		if err != nil {
			teamTaskResponsibilityError(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, view)
	case http.MethodPost:
		var in work.TeamTaskResponsibilityAction
		if !decode(w, r, &in) {
			return
		}
		receipt, err := s.Store.ApplyTeamTaskResponsibilityForSession(r.Context(), s.accountSessionDigest(r), taskID, in)
		if err != nil {
			teamTaskResponsibilityError(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, receipt)
	default:
		w.Header().Set("Allow", "GET, POST")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不支持")
	}
}

func (s Server) teamTaskResponsibilityNotices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不支持")
		return
	}
	notices, err := s.Store.TeamTaskNoticesForSession(r.Context(), s.accountSessionDigest(r))
	if err != nil {
		teamTaskResponsibilityError(w, err)
		return
	}
	if notices == nil {
		notices = []work.TeamTaskNotice{}
	}
	jsonResponse(w, http.StatusOK, notices)
}

func teamTaskResponsibilityError(w http.ResponseWriter, err error) {
	var stale *work.ErrTeamTaskVersion
	switch {
	case errors.As(err, &stale):
		jsonResponse(w, http.StatusConflict, struct {
			Code    string                      `json:"code"`
			Message string                      `json:"message"`
			Current work.TeamTaskResponsibility `json:"current"`
		}{"responsibility_version", stale.Error(), stale.Current})
	case errors.Is(err, work.ErrAuthorizationDenied), errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "task_not_found", "任务不存在或无权访问")
	case errors.Is(err, work.ErrConflict):
		fail(w, http.StatusConflict, "request_conflict", err.Error())
	case errors.Is(err, work.ErrState):
		fail(w, http.StatusConflict, "responsibility_conflict", err.Error())
	default:
		writeError(w, err)
	}
}
