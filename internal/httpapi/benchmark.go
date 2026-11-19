package httpapi

import (
	"errors"
	"io"
	"net/http"

	"clusbot/internal/work"
)

func benchmarkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrBenchmarkInput):
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, work.ErrBenchmarkBinding):
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeError(w, err)
	}
}
func (s Server) publishBenchmarkProfile(w http.ResponseWriter, r *http.Request) {
	var p work.BenchmarkProfile
	if !decode(w, r, &p) {
		return
	}
	if err := s.Store.PublishBenchmarkProfile(r.Context(), "user:session", p); err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"key": p.Key, "version": p.Version})
}
func (s Server) setBenchmarkChatTarget(w http.ResponseWriter, r *http.Request) {
	var selector work.BenchmarkProfileRef
	if !decode(w, r, &selector) {
		return
	}
	if err := s.Store.SetBenchmarkChatTarget(r.Context(), s.principal(r), r.PathValue("target"), &selector); err != nil {
		benchmarkError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) clearBenchmarkChatTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetBenchmarkChatTarget(r.Context(), s.principal(r), r.PathValue("target"), nil); err != nil {
		benchmarkError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) submitBenchmark(w http.ResponseWriter, r *http.Request) {
	var req work.BenchmarkRunRequest
	if !decode(w, r, &req) {
		return
	}
	id, err := s.Store.SubmitBenchmarkRun(r.Context(), "user:session", req, s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{"task_id": id})
}
func (s Server) benchmarkProgress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := s.Store.BenchmarkProgress(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, p)
}

func (s Server) benchmarkReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, err := s.Store.ReadBenchmarkReport(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, report)
}

func (s Server) benchmarkEvidence(w http.ResponseWriter, r *http.Request) {
	body, err := s.Store.ReadBenchmarkEvidence(r.Context(), "user:session", r.PathValue("id"), r.PathValue("artifact"))
	if err != nil {
		benchmarkError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="benchmark-evidence.bin"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

func (s Server) benchmarkEvent(w http.ResponseWriter, r *http.Request) {
	if s.BenchmarkEvents == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_event", "作业回执过大或无法读取")
		return
	}
	dup, err := s.BenchmarkEvents.Receive(r.Context(), s.Store, r.Header.Get("X-ClusBot-Timestamp"), r.Header.Get("X-ClusBot-Signature"), body)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]bool{"duplicate": dup})
}
