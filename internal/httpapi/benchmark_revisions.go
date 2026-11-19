package httpapi

import (
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
