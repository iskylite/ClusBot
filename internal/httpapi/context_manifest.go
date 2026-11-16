package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
)

func (s Server) taskManifest(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.LatestContextManifest(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		jsonResponse(w, http.StatusOK, map[string]any{"manifest": nil})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, m)
}
