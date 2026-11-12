package httpapi

import (
	"net/http"
	"strconv"

	"clusbot/internal/work"
)

func (s Server) memories(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.Memories(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, items)
}

func (s Server) memorySource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "invalid_memory_source", "需要有效的消息标识")
		return
	}
	source, err := s.Store.MemorySource(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, source)
}

func (s Server) saveMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind          string `json:"kind"`
		Title         string `json:"title"`
		Content       string `json:"content"`
		Pinned        bool   `json:"pinned"`
		Revision      int    `json:"revision"`
		SourceMessage int64  `json:"source_message"`
	}
	if !decode(w, r, &in) {
		return
	}
	key := r.PathValue("id")
	if key != "" && !validID(key) {
		fail(w, 400, "invalid_memory", "需要有效的记忆标识")
		return
	}
	saved, err := s.Store.SaveMemory(r.Context(), key, work.Memory{
		Kind: in.Kind, Title: in.Title, Content: in.Content, Pinned: in.Pinned, Revision: in.Revision,
	}, in.SourceMessage)
	if err != nil {
		writeError(w, err)
		return
	}
	status := 200
	if key == "" {
		status = 201
	}
	jsonResponse(w, status, saved)
}

func (s Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	key := r.PathValue("id")
	if !validID(key) || in.Revision < 1 {
		fail(w, 400, "invalid_memory", "需要有效的记忆标识和版本")
		return
	}
	if err := s.Store.DeleteMemory(r.Context(), key, in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
