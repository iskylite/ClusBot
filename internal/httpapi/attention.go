package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"clusbot/internal/work"
)

// listAttention is independent of model availability. Only the authenticated
// request principal is used; the client cannot pick a different owner.
func (s Server) listAttention(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	var err error
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	var before work.AttentionCursor
	if raw := q.Get("before_at"); raw != "" && err == nil {
		before.OccurredAt, err = strconv.ParseInt(raw, 10, 64)
	}
	before.ID = q.Get("before_id")
	if err != nil || limit < 1 || limit > 100 || before.OccurredAt < 0 || len(before.ID) > 512 ||
		(before.OccurredAt == 0) != (before.ID == "") {
		fail(w, http.StatusBadRequest, "invalid_attention_cursor", "无效的待处理事项分页")
		return
	}
	page, err := s.Store.ListAttention(r.Context(), s.principal(r), time.Now().UnixMilli(), limit, before)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, page)
}

func (s Server) attentionDetail(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.ReadAttention(r.Context(), s.principal(r), r.PathValue("id"), time.Now().UnixMilli())
	if work.IsAttentionNotFound(err) {
		fail(w, http.StatusNotFound, "attention_not_found", "未找到待处理事项")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, item)
}

func (s Server) attentionEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	var err error
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	if err != nil || limit < 1 || limit > 100 {
		fail(w, http.StatusBadRequest, "invalid_attention_limit", "无效的待处理事件页大小")
		return
	}
	page, err := s.Store.ReadAttentionFeed(r.Context(), s.principal(r), q.Get("after"), limit)
	if errors.Is(err, work.ErrAttentionCursorReset) {
		fail(w, http.StatusConflict, "attention_cursor_reset", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, page)
}
