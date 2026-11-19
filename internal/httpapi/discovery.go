package httpapi

import (
	"errors"
	"net/http"

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
	id, err := s.Store.GrantDiscovery(r.Context(), "user:session", g, s.DiscoverySource)
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
	if err := s.Store.PublishDiscoveryPlan(r.Context(), "user:session", p, s.DiscoverySource); err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"id": p.ID})
}
func (s Server) stopDiscovery(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DisableDiscoveryPlan(r.Context(), "user:session", r.PathValue("id")); err != nil {
		discoveryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s Server) discoveryCandidates(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListDiscoveryCandidates(r.Context(), "user:session")
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
	item, err := s.Store.AdmitDiscoveryCandidate(r.Context(), "user:session", p)
	if err != nil {
		discoveryError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, item)
}
