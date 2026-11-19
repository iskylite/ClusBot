package httpapi

import (
	"fmt"
	"net/http"
	"strconv"

	"clusbot/internal/work"
)

func benchmarkQueryInt(r *http.Request, name string, fallback int) (int, error) {
	values, ok := r.URL.Query()[name]
	if !ok {
		return fallback, nil
	}
	if len(values) == 1 {
		if value, err := strconv.Atoi(values[0]); err == nil && value > 0 {
			return value, nil
		}
	}
	return 0, fmt.Errorf("%w：%s 应为正整数", work.ErrBenchmarkInput, name)
}

func (s Server) benchmarkProfiles(w http.ResponseWriter, r *http.Request) {
	limit, err := benchmarkQueryInt(r, "limit", 25)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	page, err := s.Store.ListBenchmarkProfiles(r.Context(), s.principal(r), r.URL.Query().Get("after"), limit)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, page)
}

func (s Server) benchmarkProfileVersions(w http.ResponseWriter, r *http.Request) {
	limit, err := benchmarkQueryInt(r, "limit", 25)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	before, err := benchmarkQueryInt(r, "before", 0)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	page, err := s.Store.ListBenchmarkVersions(r.Context(), s.principal(r), r.PathValue("key"), before, limit)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, page)
}

func (s Server) benchmarkProfileVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	base, err := benchmarkQueryInt(r, "base_version", 0)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	info, err := s.Store.ReadBenchmarkProfileDetails(r.Context(), s.principal(r), r.PathValue("key"), version, base)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, info)
}

func (s Server) benchmarkChatTarget(w http.ResponseWriter, r *http.Request) {
	ref, err := s.Store.ReadBenchmarkChatTarget(r.Context(), s.principal(r), r.PathValue("target"))
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, ref)
}

func (s Server) benchmarkPreflight(w http.ResponseWriter, r *http.Request) {
	var ref work.BenchmarkProfileRef
	if !decode(w, r, &ref) {
		return
	}
	check, err := s.Store.PreflightBenchmark(r.Context(), s.principal(r), ref, s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, check)
}
