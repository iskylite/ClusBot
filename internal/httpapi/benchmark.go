package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"

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
	if err := s.Store.PublishBenchmarkProfile(r.Context(), s.principal(r), p); err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"key": p.Key, "version": p.Version})
}
func benchmarkChatTargetError(w http.ResponseWriter, err error) {
	if errors.Is(err, work.ErrBenchmarkChatTargetConflict) {
		fail(w, http.StatusConflict, "benchmark_chat_target_conflict", err.Error())
		return
	}
	benchmarkError(w, err)
}

func (s Server) setBenchmarkChatTarget(w http.ResponseWriter, r *http.Request) {
	var input struct {
		work.BenchmarkProfileRef
		ExpectedBindingRevision *int `json:"expected_binding_revision"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.ExpectedBindingRevision != nil && *input.ExpectedBindingRevision < 0 {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	if err := s.Store.UpdateBenchmarkChatTarget(r.Context(), s.principal(r), r.PathValue("target"), &input.BenchmarkProfileRef, input.ExpectedBindingRevision); err != nil {
		benchmarkChatTargetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) clearBenchmarkChatTarget(w http.ResponseWriter, r *http.Request) {
	var expected *int
	if fields, ok := r.URL.Query()["expected_binding_revision"]; ok {
		if len(fields) != 1 {
			benchmarkError(w, work.ErrBenchmarkInput)
			return
		}
		revision, err := strconv.Atoi(fields[0])
		if err != nil || revision < 0 {
			benchmarkError(w, work.ErrBenchmarkInput)
			return
		}
		expected = &revision
	}
	if err := s.Store.UpdateBenchmarkChatTarget(r.Context(), s.principal(r), r.PathValue("target"), nil, expected); err != nil {
		benchmarkChatTargetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) submitBenchmark(w http.ResponseWriter, r *http.Request) {
	var req work.BenchmarkRunRequest
	if !decode(w, r, &req) {
		return
	}
	id, err := s.Store.SubmitBenchmarkRun(r.Context(), s.principal(r), req, s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{"task_id": id})
}
func (s Server) benchmarkProgress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := s.Store.BenchmarkProgressWithProvider(r.Context(), s.principal(r), r.PathValue("id"), s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	if s.instanceStore != nil && !s.AccountBenchmarkDispatch && p.DispatchControl.Available {
		// Without an opted-in account worker, do not offer a pause that this
		// runtime cannot subsequently resume and execute.
		p.DispatchControl.Available = false
		p.DispatchControl.UnavailableReason = "账号运行时尚未接入基准派发控制"
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

func (s Server) benchmarkEvidenceInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := benchmarkQueryInt(r, "limit", 25)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	after := r.URL.Query()["after"]
	if len(after) > 1 || (len(after) == 1 && after[0] == "") {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	page, err := s.Store.ListBenchmarkEvidence(r.Context(), s.principal(r), r.PathValue("id"), r.URL.Query().Get("after"), limit)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, page)
}

func (s Server) benchmarkEvidence(w http.ResponseWriter, r *http.Request) {
	body, err := s.Store.ReadBenchmarkEvidence(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("artifact"))
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

func (s Server) benchmarkAdmissionPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in work.BenchmarkAdmissionPreviewInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.PreviewBenchmarkAdmissionAI(r.Context(), s.principal(r), in, s.BenchmarkProvider, s.Settings.InterpretBenchmarkAdmission)
	switch {
	case errors.Is(err, work.ErrBenchmarkAdmissionScale):
		fail(w, http.StatusConflict, "benchmark_scale_unverified", err.Error())
	case errors.Is(err, work.ErrBenchmarkAdmissionClarify):
		fail(w, http.StatusBadRequest, "benchmark_admission_clarify", err.Error())
	case errors.Is(err, work.ErrBenchmarkAdmissionModelBinding):
		fail(w, http.StatusConflict, "benchmark_admission_model_binding_unavailable", err.Error())
	case errors.Is(err, work.ErrBenchmarkAdmissionModel):
		fail(w, http.StatusBadGateway, "benchmark_admission_model_failed", err.Error())
	case err != nil:
		benchmarkError(w, err)
	default:
		jsonResponse(w, http.StatusOK, out)
	}
}

func (s Server) confirmBenchmarkAdmission(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !validID(r.PathValue("id")) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	var in work.BenchmarkAdmissionConfirmationInput
	if !decode(w, r, &in) {
		return
	}
	ok, err := s.Store.ConfirmBenchmarkAdmission(r.Context(), s.principal(r), r.PathValue("id"), in, s.BenchmarkProvider)
	if errors.Is(err, work.ErrBenchmarkAdmissionPreviewStale) {
		fail(w, http.StatusConflict, "benchmark_admission_preview_stale", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, ok)
}

func (s Server) previewBenchmarkSuggestion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	conversation, run := r.PathValue("conversation"), r.PathValue("run")
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	var in work.BenchmarkSuggestionPreviewInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.PreviewBenchmarkSuggestion(r.Context(), s.principal(r), conversation, run, in, s.BenchmarkProvider)
	if errors.Is(err, work.ErrBenchmarkAdmissionPreviewStale) {
		fail(w, http.StatusConflict, "benchmark_suggestion_stale", "基准建议已过期或绑定已变化，请重新发起基准委托")
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) benchmarkExecutionControl(w http.ResponseWriter, r *http.Request) {
	task, step := r.PathValue("id"), r.PathValue("step")
	if !validID(task) || !validID(step) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	var in work.BenchmarkExecutionControlInput
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.ApplyBenchmarkExecutionControl(r.Context(), s.principal(r), task, step, in)
	if errors.Is(err, work.ErrBenchmarkDispatchNotPending) {
		fail(w, http.StatusConflict, "benchmark_dispatch_not_pending", err.Error())
		return
	}
	if errors.Is(err, work.ErrBenchmarkCaseNotPending) {
		fail(w, http.StatusConflict, "benchmark_case_not_pending", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) benchmarkTaskDispatchControl(w http.ResponseWriter, r *http.Request) {
	task := r.PathValue("id")
	if !validID(task) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	if s.instanceStore != nil && !s.AccountBenchmarkDispatch {
		fail(w, http.StatusServiceUnavailable, "benchmark_dispatch_unavailable", "账号运行时尚未接入基准派发控制")
		return
	}
	var in work.BenchmarkTaskDispatchInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.ControlBenchmarkTaskDispatch(r.Context(), s.principal(r), task, in, s.BenchmarkProvider)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) benchmarkTaskDispatchReceipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	task, ids := r.PathValue("id"), r.URL.Query()["client_request_id"]
	if !validID(task) || len(ids) != 1 {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	out, err := s.Store.ReadBenchmarkTaskDispatchReceipt(r.Context(), s.principal(r), task, ids[0])
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) confirmBenchmarkExecutionSuggestion(w http.ResponseWriter, r *http.Request) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.ConfirmBenchmarkExecutionSuggestion(r.Context(), s.principal(r), conversation, run, in.Action)
	if errors.Is(err, work.ErrBenchmarkCaseNotPending) {
		fail(w, 409, "benchmark_case_not_pending", err.Error())
		return
	}
	if err != nil {
		benchmarkError(w, err)
		return
	}
	jsonResponse(w, 200, out)
}
