package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"clusbot/internal/work"
)

func discoveryError(w http.ResponseWriter, err error) {
	if errors.Is(err, work.ErrDiscoveryInput) {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeError(w, err)
}
func (s Server) grantDiscovery(w http.ResponseWriter, r *http.Request) {
	var g work.DiscoveryGrant
	if !decode(w, r, &g) {
		return
	}
	id, err := s.Store.GrantDiscovery(r.Context(), s.principal(r), g, s.DiscoverySource)
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"authorization_id": id})
}
func (s Server) publishDiscovery(w http.ResponseWriter, r *http.Request) {
	var p work.DiscoveryPlanRequest
	if !decode(w, r, &p) {
		return
	}
	if err := s.Store.PublishDiscoveryPlan(r.Context(), s.principal(r), p, s.DiscoverySource); err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"id": p.ID})
}
func (s Server) stopDiscovery(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DisableDiscoveryPlan(r.Context(), s.principal(r), r.PathValue("id")); err != nil {
		discoveryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s Server) discoverySetupStatus(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for _, field := range []string{"source", "scope"} {
		if values, exists := query[field]; exists && (len(values) != 1 || values[0] == "") {
			discoveryError(w, work.ErrDiscoveryInput)
			return
		}
	}
	source := s.DiscoverySource
	if account, ok := accountFromContext(r); s.ServerMode && (!ok || !account.IsSuperadmin) {
		source = nil
	}
	out, err := s.Store.ReadDiscoverySetupStatus(r.Context(), s.principal(r), source, query.Get("source"), query.Get("scope"))
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) discoveryCandidates(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListDiscoveryCandidates(r.Context(), s.principal(r))
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, items)
}
func (s Server) admitDiscovery(w http.ResponseWriter, r *http.Request) {
	var p work.DiscoveryAdmission
	if !decode(w, r, &p) {
		return
	}
	p.Candidate = r.PathValue("id")
	item, err := s.Store.AdmitDiscoveryCandidate(r.Context(), s.principal(r), p)
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, item)
}

func (s Server) discoveryObservationHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	limit := 25
	if values, exists := q["limit"]; exists {
		if len(values) != 1 {
			discoveryError(w, work.ErrDiscoveryInput)
			return
		}
		var err error
		limit, err = strconv.Atoi(values[0])
		if err != nil {
			discoveryError(w, work.ErrDiscoveryInput)
			return
		}
	}
	if values, exists := q["before"]; exists && (len(values) != 1 || values[0] == "") {
		discoveryError(w, work.ErrDiscoveryInput)
		return
	}
	out, err := s.Store.ListDiscoveryObservations(r.Context(), s.principal(r), r.PathValue("id"), q.Get("before"), limit)
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) discoveryCandidateObservations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	limit := 25
	if values, exists := q["limit"]; exists {
		if len(values) != 1 {
			discoveryError(w, work.ErrDiscoveryInput)
			return
		}
		var err error
		if limit, err = strconv.Atoi(values[0]); err != nil {
			discoveryError(w, work.ErrDiscoveryInput)
			return
		}
	}
	if values, exists := q["before"]; exists && (len(values) != 1 || values[0] == "") {
		discoveryError(w, work.ErrDiscoveryInput)
		return
	}
	page, err := s.Store.ListDiscoveryCandidateObservations(r.Context(), s.principal(r), r.PathValue("id"), q.Get("before"), limit)
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, page)
}
