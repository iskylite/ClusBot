package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"clusbot/internal/work"
)

const assistantTaskProgressAction = "query_task_progress"

// Only bounded personal-list shortcuts persist these cards. The explicit tool
// catalog and deterministic mode keep their existing, direct read contracts.
func (s Server) submitAssistantReadCard(w http.ResponseWriter, r *http.Request, in submission, key string, task bool) bool {
	if task || readOnlyAssistant(r) || r.Header.Get("X-ClusBot-Assistant-Cards") != "1" ||
		in.TaskRef != nil || len(in.AttachmentIDs) > 0 || len(in.Reads) > 0 || in.Read != nil || in.Criteria != nil || in.Filename != nil ||
		in.Scope != "" || in.ReplaceMaterials || in.Revision != 0 || in.BenchmarkExecutionTarget != nil || in.BenchmarkRevisionTarget != nil || in.InterpretBenchmarkExecution {
		return false
	}
	if s.submitAssistantBenchmarkComparison(w, r, in.Text, key) {
		return true
	}
	var open func(context.Context, string, string, string, string) (work.Run, error)
	switch assistantReadShortcut(in.Text) {
	case "list_tasks":
		open = s.Store.OpenAssistantTaskList
	case "list_attention":
		open = s.Store.OpenAssistantAttentionList
	default:
		switch strings.TrimRight(strings.TrimSpace(in.Text), "？?。！!") {
		case "查看我的资产", "查看资产台账", "列出我的资产":
			open = s.Store.OpenAssistantAssetList
		case "查看我的成果", "列出我的成果", "查看成果清单":
			open = s.Store.OpenAssistantArtifactList
		case "查看本会话可绑定的 Skill", "查看本会话的 Skill", "预览会话 Skill 绑定":
			open = s.Store.OpenAssistantSkillOptions
		case "查看定时发现状态", "核对定时发现计划", "查看我的定时发现":
			s.submitAssistantDiscoverySchedule(w, r, key, in.Text)
			return true
		default:
			return false
		}
	}
	run, err := open(r.Context(), s.principal(r), r.PathValue("id"), key, in.Text)
	if err != nil {
		writeError(w, err)
		return true
	}
	writeSubmission(w, run)
	return true
}

func (s Server) assistantTaskCardScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantTaskListCard, bool) {
	runtime, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !runtime.ready || s.assistantSession(r) == "" {
		fail(w, 503, "a2ui_task_card_unavailable", "任务清单暂不可用")
		return nil, work.AssistantTaskListCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "a2ui_task_card_unavailable", "当前会话没有这张任务清单")
		return nil, work.AssistantTaskListCard{}, false
	}
	card, err := s.Store.AssistantTaskListCard(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return runtime, card, true
}

func (s Server) assistantTaskCard(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantTaskCardScope(w, r)
	if !ok {
		return
	}
	s.writeAssistantTaskCard(w, r, runtime, card)
}

func taskProgressComponent(runtime *discoveryA2UIRuntime, surface, task string) string {
	return "progress_" + runtime.mac(surface, task)
}

func (s Server) writeAssistantTaskCard(w http.ResponseWriter, r *http.Request, runtime *discoveryA2UIRuntime, card work.AssistantTaskListCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := runtime.surface(s.assistantSession(r), conversation, assistantTaskProgressAction, run, card.Revision)
	if err != nil {
		fail(w, 503, "a2ui_task_card_unavailable", "任务清单暂不可用")
		return
	}
	children := []string{"heading"}
	recordTasks := []map[string]string{}
	nodes := []map[string]any{nil, a2uiText("heading", "我的任务")}
	status := func(raw string) string {
		return assistantEnum(raw, "待核实", map[string]string{
			"active": "处理中", "waiting_user": "待处理", "interrupted": "已中断", "completed": "已完成", "cancelled": "已取消",
			"queued": "排队中", "running": "运行中", "succeeded": "已成功", "failed": "失败",
		})
	}
	for _, row := range card.Rows {
		btn := taskProgressComponent(runtime, surface, row.ID)
		text := "item_" + row.ID
		title := assistantSurfaceClip(assistantSurfaceValue(row.Title))
		if title == "" {
			title = "未命名任务"
		}
		children = append(children, text)
		nodes = append(nodes, a2uiText(text, assistantSurfaceClip(assistantSurfaceParts(title, status(row.Status), row.Scope))))
		if result, exists := card.Progress[row.ID]; exists {
			id := "receipt_" + row.ID
			children = append(children, id)
			message := "助手暂不能核对这项任务，未重新执行或修改任务。"
			switch result.Decision {
			case "show_progress":
				recordTasks = append(recordTasks, map[string]string{"task_id": row.ID, "label": assistantTaskEvidenceLabel(title, "这项任务")})
				message = "核对时：任务" + status(result.Status)
				if result.LatestRun != "" {
					message += "，最近一轮" + status(result.LatestRun)
				}
				message += "。这是本地记录，不代表远端作业或验收。"
			case "ask_for_details":
				message = "助手需要更多信息。请在对话里补充你要核对的进度。"
			case "task_unavailable":
				message = "这项任务已不可查看，未读取进度。"
			}
			nodes = append(nodes, a2uiText(id, message))
			continue
		}
		label := "label_" + row.ID
		children = append(children, btn)
		nodes = append(nodes, a2uiText(label, assistantSurfaceClip("查看进度："+title)),
			a2uiButton(btn, label, assistantTaskProgressAction, map[string]any{"task_id": row.ID, "revision": card.Revision}))
	}
	if len(card.Rows) == 0 {
		children = append(children, "empty")
		nodes = append(nodes, a2uiText("empty", "暂无任务。"))
	}
	if card.More {
		children = append(children, "limit")
		nodes = append(nodes, a2uiText("limit", "这里只显示最近 10 项任务。"))
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil), "record_tasks": recordTasks})
}

func (s Server) assistantTaskProgress(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantTaskCardScope(w, r)
	if !ok {
		return
	}
	reqKey := r.Header.Get("X-ClusBot-Request-Key")
	if readOnlyAssistant(r) || len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(reqKey) {
		fail(w, 400, "a2ui_invalid_task_action", "缺少有效的交互标识。")
		return
	}
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var msg struct {
		Version string          `json:"version"`
		Action  json.RawMessage `json:"action"`
	}
	var a a2uiDiscoveryAction
	var selected struct {
		Task     string `json:"task_id"`
		Revision int64  `json:"revision"`
	}
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &a) != nil || a.Name != assistantTaskProgressAction || !validA2UIUserMessage(a.UserMessage) || !validA2UIMetadata(a.Metadata) ||
		!a2uiStrictObject(a.Context, "task_id", "revision") || json.Unmarshal(a.Context, &selected) != nil || !validID(selected.Task) {
		fail(w, 400, "a2ui_invalid_task_action", "只接受本文明确提供的任务进度查询。")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, a.Timestamp); err != nil {
		fail(w, 400, "a2ui_invalid_task_action", "交互时间无效。")
		return
	}
	actor, session, conversation, run := s.principal(r), s.assistantSession(r), r.PathValue("id"), r.PathValue("run")
	if selected.Revision != card.Revision || !card.HasTask(selected.Task) ||
		!runtime.validSurface(a.SurfaceID, session, conversation, a.Name, run, card.Revision) ||
		a.SourceComponentID != taskProgressComponent(runtime, a.SurfaceID, selected.Task) {
		fail(w, 409, "a2ui_task_card_stale", "此选择已失效或不属于当前会话，请重新打开任务清单。")
		return
	}
	recheck := func(ctx context.Context) error {
		if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
			return err
		}
		fresh, err := s.Store.AssistantTaskListCard(ctx, actor, conversation, run)
		if err != nil {
			return err
		}
		if fresh.Revision != selected.Revision || !fresh.HasTask(selected.Task) {
			return work.ErrConversationRevision
		}
		card = fresh
		return nil
	}
	if err := recheck(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	invocation := work.AssistantTaskProgressAction{Actor: actor, Conversation: conversation, Run: run, Target: selected.Task, Request: reqKey, Revision: selected.Revision}
	claim := func() (done, ok bool) {
		done, err := s.Store.BeginAssistantTaskProgress(r.Context(), invocation)
		switch {
		case err == nil:
			return done, true
		case errors.Is(err, work.ErrA2UIActionPending):
			fail(w, 409, "a2ui_task_progress_pending", "正在核对或回执未确定。可重新读取；不会自动重发。")
		case errors.Is(err, work.ErrA2UIActionFailed):
			fail(w, 409, "a2ui_task_progress_failed", "这次查询没有完成。请重新读取或明确发起新查询。")
		default:
			writeError(w, err)
		}
		return false, false
	}
	restored := func() {
		if err := recheck(r.Context()); err != nil {
			writeError(w, err)
			return
		}
		s.writeAssistantTaskCard(w, r, runtime, card)
	}
	// A completed request is purely a receipt lookup. It remains usable if
	// the model is down or its concurrency budget is exhausted.
	if _, exists := card.Progress[selected.Task]; exists {
		if done, ok := claim(); ok {
			if !done {
				fail(w, 409, "a2ui_task_card_stale", "回执已变化，请重新打开任务清单。")
				return
			}
			restored()
		}
		return
	}
	select {
	case runtime.modelGate <- struct{}{}:
		defer func() { <-runtime.modelGate }()
	default:
		fail(w, 429, "a2ui_task_card_busy", "助手繁忙，请稍后重试。")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
	release, err := s.Store.AcquireDiscoveryA2UIModelSlot(wait)
	stop()
	if err != nil {
		fail(w, 429, "a2ui_task_card_busy", "助手繁忙，请稍后重试。")
		return
	}
	defer release()
	if done, ok := claim(); !ok {
		return
	} else if done {
		restored()
		return
	}
	finished := false
	defer func() {
		if !finished && r.Context().Err() == nil {
			ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer stop()
			_ = s.Store.FailAssistantTaskProgress(ctx, invocation)
		}
	}()
	response, err := s.Settings.AssistantTaskProgressDecision(r.Context(), recheck)
	if err != nil {
		fail(w, 503, "a2ui_task_progress_model_unavailable", "助手模型未能核对本次选择；任务没有改变。")
		return
	}
	var d struct {
		Next string `json:"next"`
	}
	if len(response) > 512 || !a2uiStrictObject(response, "next") || json.Unmarshal(response, &d) != nil ||
		(d.Next != "show_progress" && d.Next != "ask_for_details" && d.Next != "cannot_prepare") {
		fail(w, 502, "a2ui_task_progress_invalid_model", "助手没有返回受支持的只读决定。")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	if err := recheck(ctx); err != nil {
		writeError(w, err)
		return
	}
	if err := s.Store.FinishAssistantTaskProgress(ctx, invocation, d.Next); err != nil {
		writeError(w, err)
		return
	}
	finished = true
	if err := recheck(ctx); err != nil {
		writeError(w, err)
		return
	}
	s.writeAssistantTaskCard(w, r, runtime, card)
}
