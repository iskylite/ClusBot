package httpapi

import "net/http"

func (s Server) experienceCandidates(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.ExperienceCandidates(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) captureAcceptedExperience(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskID     string `json:"task_id"`
		ArtifactID string `json:"artifact_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validID(input.TaskID) || !validID(input.ArtifactID) {
		fail(w, http.StatusBadRequest, "invalid_experience", "需要有效的任务和成果标识")
		return
	}
	result, err := s.Store.CaptureAcceptedExperience(r.Context(), input.TaskID, input.ArtifactID)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) captureExperienceCorrection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MessageID         int64  `json:"message_id"`
		CounterArtifactID string `json:"counter_artifact_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.MessageID < 1 || (input.CounterArtifactID != "" && !validID(input.CounterArtifactID)) {
		fail(w, http.StatusBadRequest, "invalid_experience", "需要有效的纠正消息和反例成果标识")
		return
	}
	result, err := s.Store.CaptureExperienceCorrection(r.Context(), input.MessageID, input.CounterArtifactID)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) reviewExperience(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision int    `json:"revision"`
		Decision string `json:"decision"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !validID(id) || input.Revision < 1 || (input.Decision != "approved" && input.Decision != "rejected") {
		fail(w, http.StatusBadRequest, "invalid_experience", "需要有效的候选、版本和审阅决定")
		return
	}
	result, err := s.Store.ReviewExperience(r.Context(), id, input.Revision, input.Decision)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}
