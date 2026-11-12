package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"clusbot/internal/work"
)

func (s Server) conversationHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if value := query.Get("archived"); value != "" && value != "0" && value != "1" {
		fail(w, 400, "invalid_history_cursor", "历史分页参数无效")
		return
	}
	page, err := s.Store.ConversationsPage(r.Context(), query.Get("archived") == "1", query.Get("after"))
	if errors.Is(err, work.ErrHistoryCursor) {
		fail(w, 400, "invalid_history_cursor", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, page)
}

func (s Server) messageHistory(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("id")) {
		fail(w, 400, "invalid_conversation", "会话标识无效")
		return
	}
	values := make([]int64, 3)
	for index, name := range []string{"before", "after", "around"} {
		if value := r.URL.Query().Get(name); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 {
				fail(w, 400, "invalid_history_cursor", "历史分页参数无效")
				return
			}
			values[index] = parsed
		}
	}
	page, err := s.Store.MessageHistory(r.Context(), r.PathValue("id"), values[0], values[1], values[2])
	if errors.Is(err, work.ErrHistoryCursor) {
		fail(w, 400, "invalid_history_cursor", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, page)
}

func (s Server) cancelConversation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RunID string `json:"run_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validID(input.RunID) || !validID(r.PathValue("id")) {
		fail(w, 400, "invalid_run", "执行标识无效")
		return
	}
	run, err := s.Store.StopConversation(r.Context(), r.PathValue("id"), input.RunID)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, run)
}
