package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"clusbot/internal/work"
)

const benchmarkAdmissionButton = "preview_admission"

type benchmarkAdmissionCaseView struct {
	Key      string                         `json:"key"`
	Blockers []work.BenchmarkPreflightIssue `json:"blockers"`
	Warnings []work.BenchmarkPreflightIssue `json:"warnings"`
}

type benchmarkAdmissionPreviewView struct {
	ID               string                       `json:"preview_id"`
	AssetCount       int                          `json:"asset_count"`
	CanQueue         bool                         `json:"can_queue"`
	Facility         string                       `json:"facility_status"`
	Cases            []benchmarkAdmissionCaseView `json:"cases"`
	ExecutionStarted bool                         `json:"execution_started"`
}

type benchmarkAdmissionReply struct {
	Conversation string                         `json:"conversation_id"`
	Run          string                         `json:"run_id"`
	State        string                         `json:"state"`
	Target       string                         `json:"target,omitempty"`
	Profile      *work.BenchmarkProfileRef      `json:"profile,omitempty"`
	Surface      []map[string]any               `json:"surface,omitempty"`
	Preview      *benchmarkAdmissionPreviewView `json:"preview,omitempty"`
	AcceptedTask string                         `json:"task_id,omitempty"`
}

func (s Server) benchmarkAdmissionSurface(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	runtime, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	actor, session, conversation, run := s.principal(r), s.assistantSession(r), r.PathValue("id"), r.PathValue("run")
	if runtime == nil || !runtime.ready || session == "" {
		fail(w, 403, "a2ui_session_required", "请先登录当前账号")
		return
	}
	if !validID(conversation) || !validID(run) {
		benchmarkError(w, work.ErrBenchmarkInput)
		return
	}
	if s.instanceStore != nil && !s.AccountBenchmarkDispatch {
		fail(w, 503, "benchmark_admission_unavailable", "当前未启用基准受理，未创建任务")
		return
	}
	card, err := s.Store.ReadBenchmarkAdmissionCard(r.Context(), actor, conversation, run, s.BenchmarkProvider)
	if err != nil {
		writeError(w, err)
		return
	}
	reply := func(card work.BenchmarkAdmissionCard) {
		out := benchmarkAdmissionReply{Conversation: conversation, Run: run, State: card.State, AcceptedTask: card.AcceptedTask}
		switch card.State {
		case "ready", "pending", "previewed", "clarify":
			out.Target, out.Profile = card.Suggestion.Target, &card.Suggestion.Profile
		}
		if card.State == "ready" {
			surface, err := runtime.surface(session, conversation, work.BenchmarkAdmissionPreviewAction, run+":"+card.Snapshot, card.Revision)
			if err != nil {
				writeError(w, err)
				return
			}
			out.Surface = a2uiCreateSurface(surface, []map[string]any{
				a2uiColumn([]string{benchmarkAdmissionButton}),
				a2uiText("preview_label", "查看受理预览"),
				a2uiButton(benchmarkAdmissionButton, "preview_label", work.BenchmarkAdmissionPreviewAction, map[string]any{"source_run_id": run, "revision": card.Revision}),
			}, nil)
		}
		if card.Preview != nil {
			p := card.Preview
			cases := make([]benchmarkAdmissionCaseView, 0, len(p.Preflight.Cases))
			for _, item := range p.Preflight.Cases {
				cases = append(cases, benchmarkAdmissionCaseView{item.Key, item.Blockers, item.Warnings})
			}
			out.Preview = &benchmarkAdmissionPreviewView{ID: p.PreviewID, AssetCount: len(p.Selection.AssetRefs),
				CanQueue: p.Preflight.CanQueue, Facility: p.Preflight.FacilityStatus, Cases: cases,
				ExecutionStarted: p.ExecutionStarted}
		}
		jsonResponse(w, http.StatusOK, out)
	}
	if r.Method == http.MethodGet {
		reply(card)
		return
	}
	key := r.Header.Get("X-ClusBot-Request-Key")
	if len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(key) || readOnlyAssistant(r) {
		fail(w, 400, "a2ui_invalid_request", "只能请求本会话签发的基准受理预览")
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
	var event a2uiDiscoveryAction
	var input struct {
		Run      string `json:"source_run_id"`
		Revision int64  `json:"revision"`
	}
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &event) != nil || event.Name != work.BenchmarkAdmissionPreviewAction || event.SourceComponentID != benchmarkAdmissionButton ||
		!validA2UIUserMessage(event.UserMessage) || !validA2UIMetadata(event.Metadata) ||
		!a2uiStrictObject(event.Context, "source_run_id", "revision") || json.Unmarshal(event.Context, &input) != nil {
		fail(w, 400, "a2ui_invalid_benchmark_admission", "只能预览原基准建议，不能指定新目标或执行")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
		fail(w, 400, "a2ui_invalid_benchmark_admission", "界面操作时间无效")
		return
	}
	if input.Run != run || input.Revision != card.Revision || card.Snapshot == "" ||
		!runtime.validSurface(event.SurfaceID, session, conversation, event.Name, run+":"+card.Snapshot, card.Revision) {
		writeBenchmarkAdmissionActionError(w, work.ErrBenchmarkAdmissionPreviewStale)
		return
	}
	request := work.AssistantReadCardAction{Actor: actor, Conversation: conversation, Run: run, Target: card.Snapshot, Request: key, Revision: card.Revision}
	recheck := func(ctx context.Context) (work.BenchmarkAdmissionCard, error) {
		if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
			return work.BenchmarkAdmissionCard{}, work.ErrAuthorizationDenied
		}
		fresh, err := s.Store.ReadBenchmarkAdmissionCard(ctx, actor, conversation, run, s.BenchmarkProvider)
		if err != nil {
			return fresh, err
		}
		if fresh.Revision != request.Revision || fresh.Snapshot != request.Target {
			return fresh, work.ErrBenchmarkAdmissionPreviewStale
		}
		return fresh, nil
	}
	if _, err := recheck(r.Context()); err != nil {
		writeBenchmarkAdmissionActionError(w, err)
		return
	}
	restore := func() {
		fresh, err := recheck(r.Context())
		if err != nil {
			writeBenchmarkAdmissionActionError(w, err)
			return
		}
		reply(fresh)
	}
	claim := func() (bool, bool) {
		done, err := s.Store.BeginBenchmarkAdmissionAction(r.Context(), request, s.BenchmarkProvider)
		if err != nil {
			writeBenchmarkAdmissionActionError(w, err)
			return false, false
		}
		return done, true
	}
	if card.State == "previewed" || card.State == "clarify" {
		if done, ok := claim(); ok {
			if !done {
				writeBenchmarkAdmissionActionError(w, work.ErrState)
				return
			}
			restore()
		}
		return
	}
	select {
	case runtime.modelGate <- struct{}{}:
		defer func() { <-runtime.modelGate }()
	default:
		fail(w, 429, "a2ui_preview_busy", "助手繁忙，尚未生成预览")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
	release, err := s.Store.AcquireDiscoveryA2UIModelSlot(wait)
	stop()
	if err != nil {
		fail(w, 429, "a2ui_preview_busy", "助手繁忙，尚未生成预览")
		return
	}
	defer release()
	if done, ok := claim(); !ok {
		return
	} else if done {
		restore()
		return
	}
	finished := false
	defer func() {
		if !finished {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			_ = s.Store.FailBenchmarkAdmissionAction(ctx, request)
		}
	}()
	if _, err := recheck(r.Context()); err != nil {
		writeBenchmarkAdmissionActionError(w, err)
		return
	}
	decision, err := s.Settings.InterpretBenchmarkAdmissionAction(r.Context())
	if err != nil {
		writeBenchmarkAdmissionActionError(w, err)
		return
	}
	if _, err := recheck(r.Context()); err != nil {
		writeBenchmarkAdmissionActionError(w, err)
		return
	}
	if err := s.Store.FinishBenchmarkAdmissionAction(r.Context(), request, decision, s.BenchmarkProvider); err != nil {
		writeBenchmarkAdmissionActionError(w, err)
		return
	}
	finished = true
	restore()
}

func writeBenchmarkAdmissionActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrBenchmarkAdmissionPreviewStale):
		fail(w, 409, "benchmark_admission_preview_stale", "原基准建议、目标或授权已变化；请重新发起，没有创建任务")
	case errors.Is(err, work.ErrBenchmarkAdmissionModelBinding):
		fail(w, 409, "benchmark_admission_model_binding_unavailable", "基准受理模型不可用，未生成预览")
	case errors.Is(err, work.ErrBenchmarkAdmissionModel):
		fail(w, 502, "benchmark_admission_model_failed", "未取得本次预览回执，未创建任务")
	case errors.Is(err, work.ErrA2UIActionPending):
		fail(w, 409, "a2ui_preview_pending", "正在形成受理预览，请核对原回执，不会自动重发")
	case errors.Is(err, work.ErrA2UIActionFailed):
		fail(w, 409, "a2ui_preview_failed", "这次预览没有完成，请重新发起基准建议")
	default:
		writeError(w, err)
	}
}
