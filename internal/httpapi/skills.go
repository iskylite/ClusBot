package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) listSkills(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.Skills(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) draftSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope        string `json:"scope_label"`
		Instructions string `json:"instructions"`
		Revision     int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.DraftSkill(r.Context(), in.Scope, in.Instructions, in.Revision)
	if errors.Is(err, work.ErrSkill) {
		fail(w, http.StatusBadRequest, "invalid_skill", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, result)
}

func (s Server) activateSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version  int    `json:"version"`
		Revision int    `json:"revision"`
		Digest   string `json:"sha256"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.ActivateSkill(r.Context(), r.PathValue("id"), in.Version, in.Revision, in.Digest); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) disableSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.DisableSkill(r.Context(), r.PathValue("id"), in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
