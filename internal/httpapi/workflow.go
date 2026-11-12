package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var input struct {
		work.WorkflowRequest
		RequestID string `json:"client_request_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.RequestID) < 8 || len(input.RequestID) > 128 {
		fail(w, http.StatusBadRequest, "invalid_submission", "需要有效的请求标识")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	result, err := s.Store.CreateWorkflow(r.Context(), input.RequestID, input.WorkflowRequest)
	if errors.Is(err, work.ErrWorkflow) {
		fail(w, http.StatusBadRequest, "invalid_workflow", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, result)
}

func (s Server) workflow(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.Workflow(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}
