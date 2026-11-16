package httpapi

import "net/http"

func (s Server) connectionCalls(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ConnectionCalls(r.Context(), r.PathValue("id"), 50)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, items)
}
