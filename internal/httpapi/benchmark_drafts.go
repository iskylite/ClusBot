package httpapi

import (
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
