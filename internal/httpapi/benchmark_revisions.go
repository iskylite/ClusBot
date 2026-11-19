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
	var input work.BenchmarkRevisionAIInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.Store.ProposeBenchmarkRevisionAI(r.Context(), s.principal(r), r.PathValue("id"), input, s.BenchmarkProvider, s.Settings.InterpretBenchmarkRevision)
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
