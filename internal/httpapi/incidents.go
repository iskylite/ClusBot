package httpapi

import (
	"net/http"
	"strings"
)

func (s Server) createIncident(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Symptoms    string `json:"symptoms"`
		ImpactScope string `json:"impact_scope"`
	}
	if !decode(w, r, &in) {
		return
	}
	incident, err := s.Store.CreateIncident(r.Context(), strings.TrimSpace(in.Symptoms), strings.TrimSpace(in.ImpactScope))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, incident)
}
