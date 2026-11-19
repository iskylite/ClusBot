package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) benchmarkRevision(w http.ResponseWriter, r *http.Request) {
	revision, err := benchmarkQueryInt(r, "revision", 0)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	data, err := s.Store.ReadBenchmarkRevision(r.Context(), s.principal(r), r.PathValue("id"), revision)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, data)
}

func (s Server) reviseBenchmark(apply bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input work.BenchmarkRevisionInput
		if !decode(w, r, &input) {
			return
		}
		data, err := s.Store.ReviseBenchmark(r.Context(), s.principal(r), r.PathValue("id"), input, s.BenchmarkProvider, apply)
		if err != nil {
			benchmarkError(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, data)
	}
}

func (s Server) proposeBenchmarkRevision(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input work.BenchmarkRevisionProposalInput
	if !decode(w, r, &input) {
		return
	}
	proposal, err := s.Store.ProposeBenchmarkRevision(r.Context(), s.principal(r), r.PathValue("id"), input, s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, proposal)
}

func (s Server) proposeBenchmarkRevisionAI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		work.BenchmarkRevisionAIInput
		UseConfiguredRevisionProfile bool `json:"use_configured_revision_profile"`
	}
	if !decode(w, r, &input) {
		return
	}
	interpret := s.Settings.InterpretBenchmarkRevision
	if input.UseConfiguredRevisionProfile {
		if input.AllowExpectedActualChange {
			fail(w, http.StatusBadRequest, "benchmark_revision_profile_restricted", "专用模型预演不接收实测验收条件；请用现有默认入口修改这类条件")
			return
		}
		interpret = s.Settings.InterpretBenchmarkRevisionWithProfile
	}
	result, err := s.Store.ProposeBenchmarkRevisionAI(r.Context(), s.principal(r), r.PathValue("id"), input.BenchmarkRevisionAIInput, s.BenchmarkProvider, interpret)
	if errors.Is(err, work.ErrBenchmarkRevisionModelBinding) {
		fail(w, http.StatusConflict, "benchmark_revision_model_binding_unavailable", err.Error())
		return
	}
	if errors.Is(err, work.ErrBenchmarkRevisionModel) {
		fail(w, http.StatusBadGateway, "benchmark_revision_model_failed", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) benchmarkRevisionChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input work.BenchmarkRevisionChatInput
	if !decode(w, r, &input) {
		return
	}
	if !validID(r.PathValue("id")) || input.SourceRunID == "" && !validID(input.TaskID) || input.SourceRunID != "" && !validID(input.SourceRunID) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	out, err := s.Store.SubmitBenchmarkRevisionChat(r.Context(), s.principal(r), r.PathValue("id"), input, s.BenchmarkProvider, s.Settings.InterpretBenchmarkRevision)
	if errors.Is(err, work.ErrBenchmarkRevisionRequestInterrupted) {
		fail(w, http.StatusConflict, "benchmark_revision_request_interrupted", err.Error())
		return
	}
	if errors.Is(err, work.ErrBenchmarkRevisionModel) {
		fail(w, http.StatusBadGateway, "benchmark_revision_model_failed", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) reopenBenchmarkRevisionChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	conversation, run := r.PathValue("id"), r.PathValue("run_id")
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	out, err := s.Store.ReopenBenchmarkRevisionChat(r.Context(), s.principal(r), conversation, run, s.BenchmarkProvider)
	if errors.Is(err, work.ErrBenchmarkRevisionPreviewStale) {
		fail(w, http.StatusConflict, "benchmark_revision_preview_stale", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) confirmBenchmarkRevisionChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	conversation, run := r.PathValue("id"), r.PathValue("run_id")
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	var in work.BenchmarkRevisionChatConfirmInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.ConfirmBenchmarkRevisionChat(r.Context(), s.principal(r), conversation, run, in, s.BenchmarkProvider)
	if errors.Is(err, work.ErrBenchmarkRevisionPreviewStale) {
		fail(w, http.StatusConflict, "benchmark_revision_preview_stale", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) benchmarkRevisionRequestState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	conversation, request := r.PathValue("id"), r.PathValue("request_id")
	if !validID(conversation) || len(request) < 8 || len(request) > 128 {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	out, err := s.Store.ReadBenchmarkRevisionRequestState(r.Context(), s.principal(r), conversation, request)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
