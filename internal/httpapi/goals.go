package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) listGoals(w http.ResponseWriter, r *http.Request) {
	goals, err := s.Store.Goals(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, goals)
}

func (s Server) createGoal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		work.Goal
		RequestID string `json:"client_request_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.RequestID) < 8 || len(in.RequestID) > 128 {
		fail(w, http.StatusBadRequest, "invalid_submission", "需要有效的请求标识")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	goal, err := s.Store.CreateGoal(r.Context(), in.RequestID, in.Goal)
	if errors.Is(err, work.ErrGoal) {
		fail(w, http.StatusBadRequest, "invalid_goal", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, goal)
}

func (s Server) stopGoal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.StopGoal(r.Context(), r.PathValue("id"), in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
