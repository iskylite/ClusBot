package httpapi

import "net/http"

func (s Server) changeConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int     `json:"revision"`
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.ChangeConversation(r.Context(), r.PathValue("id"), in.Revision, in.Title, in.Archived)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.DeleteConversation(r.Context(), r.PathValue("id"), in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) acknowledgeRead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RunID    string `json:"run_id"`
		Revision int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.RunID) {
		fail(w, 400, "invalid_run", "执行标识无效")
		return
	}
	if err := s.Store.AcknowledgeRead(r.Context(), r.PathValue("id"), in.RunID, in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
