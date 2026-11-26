package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"clusbot/internal/work"
)

const assistantAdviceActionName = "review_operation_advice_choice"

func (s Server) assistantAdviceScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantAdviceCard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	runtime, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if runtime == nil || !runtime.ready || s.assistantSession(r) == "" {
		fail(w, 403, "a2ui_session_required", "请先登录当前账号")
		return nil, work.AssistantAdviceCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "assistant_advice_unavailable", "本会话没有这条运维建议")
		return nil, work.AssistantAdviceCard{}, false
	}
	card, err := s.Store.AssistantAdvice(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return runtime, card, true
}

func (s Server) assistantAdviceCard(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantAdviceScope(w, r)
	if ok {
		s.writeAssistantAdvice(w, r, runtime, card)
	}
}

func adviceComponent(runtime *discoveryA2UIRuntime, surface, slot string) string {
	return "choose_" + runtime.mac(surface, slot)
}

func (s Server) writeAssistantAdvice(w http.ResponseWriter, r *http.Request, runtime *discoveryA2UIRuntime, card work.AssistantAdviceCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := runtime.surface(s.assistantSession(r), conversation, assistantAdviceActionName, run, card.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	children := []string{"title", "boundary"}
	nodes := []map[string]any{nil, a2uiText("title", card.Draft.Title),
		a2uiText("boundary", "这些是供选择的文字建议。选择不会批准、启动或修改任何运维操作。")}
	if card.Draft.Summary != "" {
		children = append(children, "summary")
		nodes = append(nodes, a2uiText("summary", card.Draft.Summary))
	}
	for index, option := range card.Draft.Options {
		slot := strconv.Itoa(index + 1)
		itemID := "option_" + slot
		display := option.Label
		if option.Recommended {
			display += "（模型推荐）"
		}
		children = append(children, itemID)
		nodes = append(nodes, a2uiText(itemID, display))
		if option.Detail != "" {
			id := "detail_" + slot
			children = append(children, id)
			nodes = append(nodes, a2uiText(id, option.Detail))
		}
		if card.CanSelect() {
			id, label := adviceComponent(runtime, surface, slot), "label_"+slot
			children = append(children, id)
			nodes = append(nodes, a2uiText(label, "记录选择："+option.Label),
				a2uiButton(id, label, assistantAdviceActionName, map[string]any{"advice_slot": slot, "revision": card.Revision}))
		}
	}
	state := "ready"
	switch {
	case card.Choice != nil:
		state = "recorded"
	case card.Attempt == "applying":
		state = "pending"
	case card.Attempt == "failed":
		state = "not_completed"
	case !card.CanSelect():
		state = "unavailable"
	}
	nodes[0] = a2uiColumn(children)
	var items []json.RawMessage
	var factsUnavailable bool
	if scope, ok := parseAssistantOperationScope(card.SourceText); ok {
		if bridge, _ := r.Context().Value(assistantBridgeKey{}).(*assistantBridge); bridge != nil {
			capture := &assistantResponse{headers: make(http.Header)}
			_, pod, facts, ok := s.assistantOperationFacts(capture, r, bridge, card.SourceText, scope)
			if ok && (card.Draft.Scope == "global" || len(card.Draft.References) > 0 && pod == card.Draft.References[0]) {
				items = facts
			} else {
				factsUnavailable = true
			}
		}
	}
	jsonResponse(w, 200, struct {
		Conversation     string                      `json:"conversation_id"`
		Run              string                      `json:"run_id"`
		State            string                      `json:"state"`
		Surface          []map[string]any            `json:"surface"`
		Choice           *work.AssistantAdviceChoice `json:"choice,omitempty"`
		Items            []json.RawMessage           `json:"items,omitempty"`
		FactsUnavailable bool                        `json:"facts_unavailable,omitempty"`
	}{conversation, run, state, a2uiCreateSurface(surface, nodes, nil), card.Choice, items, factsUnavailable})
}

func (s Server) assistantAdviceAction(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantAdviceScope(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("X-ClusBot-Request-Key")
	if len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(key) || readOnlyAssistant(r) {
		fail(w, 400, "a2ui_invalid_request", "请选择已签发的建议")
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
	var selected struct {
		Slot     string `json:"advice_slot"`
		Revision int64  `json:"revision"`
	}
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &action) != nil || action.Name != assistantAdviceActionName || !validA2UIUserMessage(action.UserMessage) || !validA2UIMetadata(action.Metadata) ||
		!a2uiStrictObject(action.Context, "advice_slot", "revision") || json.Unmarshal(action.Context, &selected) != nil {
		fail(w, 400, "a2ui_invalid_advice_action", "只接受明确选择的一项建议")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, action.Timestamp); err != nil {
		fail(w, 400, "a2ui_invalid_advice_action", "交互时间无效")
		return
	}
	actor, session, conversation, run := s.principal(r), s.assistantSession(r), r.PathValue("id"), r.PathValue("run")
	if selected.Revision != card.Revision || !card.HasSlot(selected.Slot) ||
		!runtime.validSurface(action.SurfaceID, session, conversation, action.Name, run, card.Revision) ||
		action.SourceComponentID != adviceComponent(runtime, action.SurfaceID, selected.Slot) {
		fail(w, 409, "a2ui_advice_stale", "此选择不属于当前登录或原建议")
		return
	}
	a := work.AssistantReadCardAction{Actor: actor, Conversation: conversation, Run: run, Target: selected.Slot, Request: key, Revision: selected.Revision}
	recheck := func(ctx context.Context) error {
		if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
			return work.ErrAuthorizationDenied
		}
		fresh, err := s.Store.AssistantAdvice(ctx, actor, conversation, run)
		if err != nil {
			return err
		}
		if fresh.Archived || fresh.Revision != a.Revision || !fresh.HasSlot(a.Target) {
			return work.ErrConversationRevision
		}
		card = fresh
		return nil
	}
	if err := recheck(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	if card.Choice != nil {
		// An exact replay is a receipt lookup, even with no model available.
		done, err := s.Store.BeginAssistantAdviceChoice(r.Context(), a)
		if err != nil {
			writeAdviceActionError(w, err)
			return
		}
		if !done {
			writeAdviceActionError(w, work.ErrState)
			return
		}
		s.writeAssistantAdvice(w, r, runtime, card)
		return
	}
	select {
	case runtime.modelGate <- struct{}{}:
		defer func() { <-runtime.modelGate }()
	default:
		fail(w, 429, "a2ui_advice_busy", "助手繁忙，尚未记录选择")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
	release, err := s.Store.AcquireDiscoveryA2UIModelSlot(wait)
	stop()
	if err != nil {
		fail(w, 429, "a2ui_advice_busy", "助手繁忙，尚未记录选择")
		return
	}
	defer release()
	done, err := s.Store.BeginAssistantAdviceChoice(r.Context(), a)
	if err != nil {
		writeAdviceActionError(w, err)
		return
	}
	if done {
		if err := recheck(r.Context()); err != nil {
			writeError(w, err)
			return
		}
		s.writeAssistantAdvice(w, r, runtime, card)
		return
	}
	finished := false
	defer func() {
		if !finished {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			_ = s.Store.FailAssistantAdviceChoice(ctx, a)
		}
	}()
	decision, err := s.Settings.InterpretAssistantAdviceChoice(r.Context(), card.Draft.Scope, card.Draft.Focus, a.Target, recheck)
	if err != nil {
		if errors.Is(err, work.ErrAuthorizationDenied) || errors.Is(err, work.ErrConversationRevision) {
			writeError(w, err)
			return
		}
		fail(w, 502, "a2ui_advice_not_recorded", "未取得助手回执，未记录为选择，也未执行")
		return
	}
	if err = recheck(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	if err = s.Store.FinishAssistantAdviceChoice(r.Context(), a, decision); err != nil {
		writeAdviceActionError(w, err)
		return
	}
	finished = true
	if err = recheck(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	s.writeAssistantAdvice(w, r, runtime, card)
}

func writeAdviceActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrA2UIActionPending):
		fail(w, 409, "a2ui_advice_pending", "选择尚未确认。请读取原建议，不会自动重发。")
	case errors.Is(err, work.ErrA2UIActionFailed):
		fail(w, 409, "a2ui_advice_failed", "未记录选择。请核对或重新提出建议。")
	default:
		writeError(w, err)
	}
}
