package httpapi

import (
	"net/http"
	"strings"
)

// The ticket worker and independent checker currently run only in local mode.
func (s Server) localIncidentAction(w http.ResponseWriter, r *http.Request) bool {
	if s.ServerMode {
		fail(w, http.StatusForbidden, "incident_action_unavailable", "多人模式暂不支持此操作")
		return false
	}
	if !validIncidentID(r.PathValue("id")) {
		fail(w, http.StatusBadRequest, "invalid_incident", "故障事件标识无效")
		return false
	}
	return true
}

func (s Server) incidentTicketOptions(w http.ResponseWriter, r *http.Request) {
	if !s.localIncidentAction(w, r) {
		return
	}
	after := r.URL.Query().Get("after")
	if len(after) > 256 {
		fail(w, http.StatusBadRequest, "invalid_cursor", "资产游标无效")
		return
	}
	items, err := s.Store.IncidentTicketOptions(r.Context(), s.principal(r), r.PathValue("id"), after)
	if err != nil {
		writeError(w, err)
		return
	}
	reason := ""
	if s.TicketProvider == nil {
		reason = "尚未配置工单提供者"
	} else if !items.CanDispatch {
		reason = "缺少工单派发授权"
	}
	jsonResponse(w, http.StatusOK, map[string]any{"tickets": items.Tickets, "operations": items.Operations, "assets": items.Assets, "next_cursor": items.NextCursor, "ticket_available": s.TicketProvider != nil && items.CanDispatch, "ticket_reason": reason, "recheck_available": s.TicketEffectChecker != nil})
}

func (s Server) previewIncidentTicket(w http.ResponseWriter, r *http.Request) {
	if !s.localIncidentAction(w, r) {
		return
	}
	if s.TicketProvider == nil {
		fail(w, http.StatusServiceUnavailable, "ticket_provider_unavailable", "尚未配置工单提供者")
		return
	}
	var in struct {
		ObjectRef   string `json:"object_ref"`
		Description string `json:"description"`
	}
	if !decode(w, r, &in) {
		return
	}
	preview, err := s.Store.PreviewIncidentTicket(r.Context(), s.principal(r), r.PathValue("id"), strings.TrimSpace(in.ObjectRef), strings.TrimSpace(in.Description))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, preview)
}

func (s Server) confirmIncidentTicket(w http.ResponseWriter, r *http.Request) {
	if !s.localIncidentAction(w, r) {
		return
	}
	var in struct {
		PreviewID string `json:"preview_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.PreviewID) {
		fail(w, http.StatusBadRequest, "invalid_preview", "确认标识无效")
		return
	}
	result, err := s.Store.ConfirmIncidentTicket(r.Context(), s.principal(r), r.PathValue("id"), in.PreviewID, s.TicketProvider)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, result)
}

func (s Server) previewIncidentRecheck(w http.ResponseWriter, r *http.Request) {
	if !s.localIncidentAction(w, r) {
		return
	}
	var in struct {
		TicketID string `json:"ticket_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.TicketID == "" || len(in.TicketID) > 256 || strings.ContainsAny(in.TicketID, "/\\\x00\r\n") {
		fail(w, http.StatusBadRequest, "invalid_ticket", "工单标识无效")
		return
	}
	preview, err := s.Store.PreviewIncidentRecheck(r.Context(), s.principal(r), r.PathValue("id"), in.TicketID, s.TicketEffectChecker)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, preview)
}

func (s Server) confirmIncidentRecheck(w http.ResponseWriter, r *http.Request) {
	if !s.localIncidentAction(w, r) {
		return
	}
	var in struct {
		PreviewID string `json:"preview_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.PreviewID) {
		fail(w, http.StatusBadRequest, "invalid_preview", "确认标识无效")
		return
	}
	result, err := s.Store.ConfirmIncidentRecheck(r.Context(), s.principal(r), r.PathValue("id"), in.PreviewID, s.TicketEffectChecker)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

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
