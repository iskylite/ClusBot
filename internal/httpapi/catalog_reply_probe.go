package httpapi

import (
	"net/http"

	"clusbot/internal/model"
)

func (s Server) testCatalogModelReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("模型标识无效"))
		return
	}
	var input struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &input) {
		return
	}
	d, err := s.Settings.TestCatalogModelReply(r.Context(), id, input.Revision)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]int64{"duration_ms": d.Milliseconds(), "revision": int64(input.Revision)})
}
