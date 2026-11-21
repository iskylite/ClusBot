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
	if !validIncidentID(id) {
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
	if !validIncidentID(id) || !validIncidentStep(step) {
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

// Older local demo databases use incident identifiers such as inc-117.
func validIncidentID(id string) bool {
	if validID(id) {
		return true
	}
	suffix, ok := strings.CutPrefix(id, "inc-")
	return ok && len(suffix) > 0 && len(suffix) <= 16 && strings.Trim(suffix, "0123456789") == ""
}

func validIncidentStep(step string) bool {
	switch step {
	case "observe", "root_cause", "self_heal", "ticket", "acceptance":
		return true
	default:
		return false
	}
}

func (s Server) recentIncidents(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.RecentIncidents(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, items)
}
