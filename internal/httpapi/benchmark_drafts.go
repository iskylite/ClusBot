package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) readBenchmarkDraft(w http.ResponseWriter, r *http.Request) {
	draft, err := s.Store.ReadBenchmarkDraft(r.Context(), s.principal(r), r.PathValue("key"))
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, draft)
}

func (s Server) saveBenchmarkDraft(w http.ResponseWriter, r *http.Request) {
	var req work.BenchmarkDraftRequest
	if !decode(w, r, &req) {
		return
	}
	draft, err := s.Store.SaveBenchmarkDraft(r.Context(), s.principal(r), r.PathValue("key"), req)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, draft)
}

func (s Server) startBenchmarkDraftFromPublished(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		BaseVersion      int  `json:"base_version"`
		ExpectedRevision *int `json:"expected_revision"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.ExpectedRevision == nil {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	draft, err := s.Store.StartBenchmarkDraftFromPublished(r.Context(), s.principal(r), r.PathValue("key"), req.BaseVersion, *req.ExpectedRevision)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, draft)
}

func (s Server) publishBenchmarkDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedRevision int `json:"expected_revision"`
	}
	if !decode(w, r, &req) {
		return
	}
	draft, err := s.Store.PublishBenchmarkDraft(r.Context(), s.principal(r), r.PathValue("key"), req.ExpectedRevision)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, draft)
}

func (s Server) previewBenchmarkDraftParameter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in work.BenchmarkDraftParameterInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.PreviewBenchmarkDraftParameter(r.Context(), s.principal(r), r.PathValue("key"), in, s.Settings.InterpretBenchmarkDraftParameter)
	if errors.Is(err, work.ErrBenchmarkRevisionModel) {
		fail(w, http.StatusBadGateway, "benchmark_draft_model_failed", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) previewBenchmarkDraftDependency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in work.BenchmarkDraftDependencyInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.PreviewBenchmarkDraftDependency(r.Context(), s.principal(r), r.PathValue("key"), in, s.Settings.InterpretBenchmarkDraftDependency)
	if errors.Is(err, work.ErrBenchmarkRevisionModel) {
		fail(w, http.StatusBadGateway, "benchmark_draft_model_failed", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
