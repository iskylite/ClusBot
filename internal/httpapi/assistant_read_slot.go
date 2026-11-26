package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"clusbot/internal/work"
)

// These options are fixed application code for numbered rows in the asset
// and attention read cards. The A2UI renderer cannot provide callbacks,
// destinations, SQL, or model tools.
type assistantReadSlotHooks struct {
	action, field, component, decision string
	surfaceAction                      string
	proof                              func(string) string
	revision                           int64
	visible, saved                     func(string) bool
	refresh                            func(context.Context) (int64, error)
	claim                              func(context.Context, work.AssistantReadCardAction) (bool, error)
	finish                             func(context.Context, work.AssistantReadCardAction, string) error
	fail                               func(context.Context, work.AssistantReadCardAction) error
	interpret                          func(context.Context, func(context.Context) error) (json.RawMessage, error)
	render                             func()
	// An ephemeral read projects current facts into the response without a
	// claim, effect, durable receipt, or additional context persistence.
	present func(context.Context, work.AssistantReadCardAction, string) error
}

func assistantReadRowComponent(rt *discoveryA2UIRuntime, prefix, surface, slot string, bindings ...string) string {
	return prefix + "_" + rt.mac(append([]string{surface, slot}, bindings...)...)
}

func (s Server) assistantReadSlotAction(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime, h assistantReadSlotHooks) {
	key := r.Header.Get("X-ClusBot-Request-Key")
	if readOnlyAssistant(r) || len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(key) {
		fail(w, 400, "a2ui_read_action_invalid", "缺少有效的只读交互标识。")
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
	var event a2uiDiscoveryAction
	var arguments map[string]json.RawMessage
	var slot string
	var revision int64
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &event) != nil || event.Name != h.action || !validA2UIUserMessage(event.UserMessage) || !validA2UIMetadata(event.Metadata) ||
		!a2uiStrictObject(event.Context, h.field, "revision") || json.Unmarshal(event.Context, &arguments) != nil ||
		json.Unmarshal(arguments[h.field], &slot) != nil || json.Unmarshal(arguments["revision"], &revision) != nil {
		fail(w, 400, "a2ui_read_action_invalid", "只接受这张卡明确提供的只读核对。")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
		fail(w, 400, "a2ui_read_action_invalid", "交互时间无效。")
		return
	}
	actor, session, conversation, run := s.principal(r), s.assistantSession(r), r.PathValue("id"), r.PathValue("run")
	surfaceAction := h.action
	if h.surfaceAction != "" {
		surfaceAction = h.surfaceAction
	}
	target := slot
	var proof string
	var bindings []string
	if h.proof != nil {
		proof = h.proof(slot)
		target = slot + ":" + proof
		bindings = []string{proof}
	}
	if revision != h.revision || !h.visible(slot) || h.proof != nil && proof == "" ||
		!rt.validSurface(event.SurfaceID, session, conversation, surfaceAction, run, revision) ||
		event.SourceComponentID != assistantReadRowComponent(rt, h.component, event.SurfaceID, slot, bindings...) {
		fail(w, 409, "a2ui_read_card_stale", "这一项已变化或不可查看，请重新查询清单。")
		return
	}
	recheck := func(ctx context.Context) error {
		if err := s.verifyA2UIAccount(ctx, session, actor, false); err != nil {
			return err
		}
		current, err := h.refresh(ctx)
		if err != nil {
			return err
		}
		if current != revision || !h.visible(slot) || h.proof != nil && h.proof(slot) != proof {
			return work.ErrConversationRevision
		}
		return nil
	}
	if err := recheck(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	invocation := work.AssistantReadCardAction{Actor: actor, Conversation: conversation, Run: run, Target: target, Request: key, Revision: revision}
	claim := func() (bool, bool) {
		done, err := h.claim(r.Context(), invocation)
		switch {
		case err == nil:
			return done, true
		case errors.Is(err, work.ErrA2UIActionPending):
			fail(w, 409, "a2ui_read_pending", "正在核对或回执未确定。可以重新读取，不会自动重发。")
		case errors.Is(err, work.ErrA2UIActionFailed):
			fail(w, 409, "a2ui_read_failed", "这次核对没有完成，请重新读取或发起新查询。")
		default:
			writeError(w, err)
		}
		return false, false
	}
	restore := func(ctx context.Context) {
		if err := recheck(ctx); err != nil {
			writeError(w, err)
			return
		}
		h.render()
	}
	// A duplicate/restore is always available, even if the model is busy.
	if h.present == nil && h.saved(slot) {
		if done, ok := claim(); ok {
			if !done {
				fail(w, 409, "a2ui_read_card_stale", "回执已变化，请重新查询清单。")
				return
			}
			restore(r.Context())
		}
		return
	}
	select {
	case rt.modelGate <- struct{}{}:
		defer func() { <-rt.modelGate }()
	default:
		fail(w, 429, "a2ui_read_busy", "助手繁忙，请稍后重试。")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
	release, err := s.Store.AcquireDiscoveryA2UIModelSlot(wait)
	stop()
	if err != nil {
		fail(w, 429, "a2ui_read_busy", "助手繁忙，请稍后重试。")
		return
	}
	defer release()
	if h.present == nil {
		if done, ok := claim(); !ok {
			return
		} else if done {
			restore(r.Context())
			return
		}
	}
	finished := false
	defer func() {
		if h.present == nil && !finished && r.Context().Err() == nil {
			ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer stop()
			_ = h.fail(ctx, invocation)
		}
	}()
	modelReply, err := h.interpret(r.Context(), recheck)
	if err != nil {
		fail(w, 503, "a2ui_read_model_unavailable", "助手这次未能核对；没有提交任何业务操作。")
		return
	}
	var d struct {
		Next string `json:"next"`
	}
	if len(modelReply) > 512 || !a2uiStrictObject(modelReply, "next") || json.Unmarshal(modelReply, &d) != nil ||
		(d.Next != h.decision && d.Next != "ask_for_details" && d.Next != "cannot_prepare") {
		fail(w, 502, "a2ui_read_model_invalid", "助手没有返回受支持的只读决定。")
		return
	}
	parent := r.Context()
	if h.present == nil {
		parent = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if err := recheck(ctx); err != nil {
		writeError(w, err)
		return
	}
	if h.present != nil {
		if err := h.present(ctx, invocation, d.Next); err != nil {
			writeError(w, err)
		}
		return
	}
	if err := h.finish(ctx, invocation, d.Next); err != nil {
		writeError(w, err)
		return
	}
	finished = true
	restore(ctx)
}
