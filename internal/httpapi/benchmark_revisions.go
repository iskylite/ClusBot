package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

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
	if r.URL.Query().Get("metadata") == "1" {
		out, err := s.Store.ReadBenchmarkRevisionChatMetadata(r.Context(), s.principal(r), conversation, run)
		if err != nil {
			benchmarkError(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, out)
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

const a2uiBenchmarkPreview = work.BenchmarkRevisionPreviewAction
const a2uiBenchmarkSyncActual = work.BenchmarkRevisionSyncActualAction

func benchmarkRevisionActionButton(action string) string {
	switch action {
	case a2uiBenchmarkPreview:
		return "preview_revision"
	case a2uiBenchmarkSyncActual:
		return "preview_with_actual"
	default:
		return ""
	}
}

// The renderer may request a preview, never choose the original instruction,
// change the task/version, or apply the preview. Those belong to the Go runtime.
func (s Server) benchmarkRevisionSurface(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	conversation, run := r.PathValue("id"), r.PathValue("run_id")
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	runtime, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	session := s.assistantSession(r)
	if runtime == nil || !runtime.ready || session == "" {
		fail(w, 403, "a2ui_session_required", "请先登录当前账号")
		return
	}
	actor := s.principal(r)
	target, err := s.Store.ReadBenchmarkRevisionSuggestionForActor(r.Context(), actor, conversation, run)
	if err != nil {
		benchmarkError(w, err)
		return
	}
	if target == nil {
		fail(w, 404, "benchmark_suggestion_unavailable", "本会话没有这条可核对的修订建议")
		return
	}
	bound := run + ":" + target.TaskID
	if r.Method == http.MethodGet {
		var surfaces []map[string]any
		for _, option := range []struct{ action, label string }{
			{a2uiBenchmarkPreview, "查看方案修订预览"},
			{a2uiBenchmarkSyncActual, "同步核对值并预演"},
		} {
			// Each option has its own action in the signature. Replacing an
			// ordinary button or changing its payload cannot grant this choice.
			id, err := runtime.surface(session, conversation, option.action, bound, int64(target.ExpectedRevision))
			if err != nil {
				writeError(w, err)
				return
			}
			button := benchmarkRevisionActionButton(option.action)
			components := []map[string]any{a2uiColumn([]string{button}), a2uiText("label", option.label),
				a2uiButton(button, "label", option.action, map[string]any{"source_run_id": run, "task_id": target.TaskID, "revision": target.ExpectedRevision})}
			surfaces = append(surfaces, a2uiCreateSurface(id, components, nil)...)
		}
		jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "surface": surfaces})
		return
	}
	key := r.Header.Get("X-ClusBot-Request-Key")
	if len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(key) || readOnlyAssistant(r) {
		fail(w, 400, "a2ui_invalid_request", "需要有效的交互请求，固定查询不能执行模型预演")
		return
	}
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var msg struct {
		Version string
		Action  json.RawMessage
	}
	var action a2uiDiscoveryAction
	var data struct {
		Run      string `json:"source_run_id"`
		Task     string `json:"task_id"`
		Revision int    `json:"revision"`
	}
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &action) != nil || benchmarkRevisionActionButton(action.Name) == "" ||
		action.SourceComponentID != benchmarkRevisionActionButton(action.Name) ||
		!validA2UIUserMessage(action.UserMessage) || !validA2UIMetadata(action.Metadata) || !a2uiStrictObject(action.Context, "source_run_id", "task_id", "revision") ||
		json.Unmarshal(action.Context, &data) != nil {
		fail(w, 400, "a2ui_invalid_action", "无效的方案修订界面操作")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, action.Timestamp); err != nil {
		fail(w, 400, "a2ui_invalid_action", "界面操作时间无效")
		return
	}
	if data.Run != run || data.Task != target.TaskID || data.Revision != target.ExpectedRevision ||
		!runtime.validSurface(action.SurfaceID, session, conversation, action.Name, bound, int64(data.Revision)) {
		fail(w, 409, "a2ui_surface_stale", "此操作不属于当前修订建议，请重新打开原建议")
		return
	}
	if s.Settings == nil {
		fail(w, 503, "a2ui_model_unavailable", "未配置可用的修订模型，未生成或应用预览")
		return
	}
	out, err := s.Store.PreviewBenchmarkRevisionAction(r.Context(), actor, conversation, run, key, action.Name, s.BenchmarkProvider,
		func(ctx context.Context, input work.BenchmarkRevisionModelInput, budget int) (string, error) {
			// The account may have logged out or been disabled while waiting
			// for a model slot. No browser metadata or connection strings leave.
			if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
				return "", work.ErrAuthorizationDenied
			}
			if assistantCredentialValue.MatchString(input.Text) || assistantSurfaceURL.MatchString(input.Text) {
				return "", fmt.Errorf("%w：修订文本含连接地址或凭据，请移除后重新提交", work.ErrBenchmarkInput)
			}
			decision, err := s.Settings.InterpretBenchmarkRevision(ctx, input, budget)
			if err != nil {
				return "", err
			}
			// A detached model request may outlive its original login. Do not
			// record a draft or return private previews to a revoked session.
			if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
				return "", work.ErrAuthorizationDenied
			}
			return decision, nil
		})
	if errors.Is(err, work.ErrBenchmarkRevisionActualConsent) {
		fail(w, http.StatusBadRequest, "benchmark_revision_actual_consent_required", err.Error())
		return
	}
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
	jsonResponse(w, 200, out)
}
