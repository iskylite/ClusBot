package httpapi

import "net/http"

func (s Server) reclaimLeases(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Limit int `json:"limit"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.ReclaimExpiredLeases(r.Context(), in.Limit)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}
