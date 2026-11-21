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

func (s Server) incidentWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		fail(w, http.StatusBadRequest, "invalid_incident", "故障事件标识无效")
		return
	}
	wf, err := s.Store.IncidentWorkflow(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, wf)
}

func (s Server) incidentStepLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	step := strings.TrimSpace(r.PathValue("step"))
	if !validID(id) || step == "" || len(step) > 40 {
		fail(w, http.StatusBadRequest, "invalid_incident_step", "故障事件或阶段参数无效")
		return
	}
	logs, err := s.Store.IncidentStepLogs(r.Context(), id, step)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, logs)
}
