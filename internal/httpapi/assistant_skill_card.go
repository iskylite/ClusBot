package httpapi

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"clusbot/internal/work"
)

const assistantSkillPreviewAction = "preview_conversation_skill"
const assistantSkillConfirmAction = "confirm_skill_preview"

type assistantSkillConfirmation struct {
	Run      string `json:"run_id"`
	Slot     string `json:"slot"`
	Revision int64  `json:"revision"`
	Expiry   int64  `json:"expires_at_unix_ms"`
	Proof    string `json:"proof"`
}

type assistantSkillConfirmationForm struct {
	assistantSkillConfirmation
	Label string `json:"label"`
}

func (in assistantSkillConfirmation) signature(rt *discoveryA2UIRuntime, session, actor, conversation string) string {
	return rt.mac("skill-preview-confirm", session, actor, conversation, in.Run, in.Slot,
		strconv.FormatInt(in.Revision, 10), strconv.FormatInt(in.Expiry, 10))
}

func assistantSkillPreviewSession(rt *discoveryA2UIRuntime, session, actor, conversation, run string) string {
	return rt.mac("skill-preview-session", session, actor, conversation, run)
}

func skillCardLabel(raw string) string {
	if assistantCredentialValue.MatchString(raw) {
		return "（敏感名称已省略）"
	}
	return assistantSurfaceClip(assistantSurfaceValue(raw))
}

func (s Server) assistantSkillCardScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantSkillCard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	rt, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !rt.ready || s.assistantSession(r) == "" {
		fail(w, 503, "a2ui_skill_unavailable", "会话 Skill 预览暂不可用")
		return nil, work.AssistantSkillCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "a2ui_skill_missing", "当前会话没有这张 Skill 清单")
		return nil, work.AssistantSkillCard{}, false
	}
	if err := s.verifyA2UIAccount(r.Context(), s.assistantSession(r), s.principal(r), false); err != nil {
		writeError(w, err)
		return nil, work.AssistantSkillCard{}, false
	}
	card, err := s.Store.AssistantSkillOptions(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return rt, card, true
}

func (s Server) assistantSkillCard(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantSkillCardScope(w, r)
	if ok {
		s.writeAssistantSkillCard(w, r, rt, card)
	}
}

func (s Server) writeAssistantSkillCard(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime, card work.AssistantSkillCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	session, actor := s.assistantSession(r), s.principal(r)
	sessionProof := assistantSkillPreviewSession(rt, session, actor, conversation, run)
	surface, err := rt.surface(session, conversation, assistantSkillPreviewAction, run, card.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	children := []string{}
	nodes := []map[string]any{nil}
	var forms []assistantSkillConfirmationForm
	text := func(id, value string) {
		children = append(children, id)
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(value)))
	}
	text("heading", "会话 Skill 绑定预览")
	current := "沿用默认范围（没有显式选用）"
	if b := card.Current.Binding; b != nil {
		// Revoked/unavailable stored bindings should not expose the old name.
		if b.Validity == "valid" {
			current = fmt.Sprintf("%s · 第 %d 版", skillCardLabel(b.Scope), b.Version)
		} else {
			current = "原选用版本不可用"
		}
	}
	text("current", "当前："+current)
	text("boundary", "预览不保存。确认只选用一个版本，供本会话以后范围匹配的任务使用；不处理普通聊天，不启用或执行 Skill，不授予工具权限。")
	switch {
	case card.Applied != nil:
		if card.Current.BindingRevision == card.Applied.BindingRevision && card.Current.Binding != nil && card.Current.Binding.Validity == "valid" {
			text("applied", "已保存本次 Skill 绑定。仅影响以后此会话。")
		} else {
			text("applied", "本次 Skill 绑定曾保存；当前选用已变化或失效。")
		}
	case card.Changed:
		text("changed", "当前会话绑定已变化，请重新查看本会话可绑定的 Skill。未保存任何预览为设置。")
	case card.Busy:
		text("busy", "会话正在处理。这一轮结束后才能查看绑定预览。")
	default:
		for _, row := range card.Rows {
			label := fmt.Sprintf("%s · 第 %d 版", skillCardLabel(row.Scope), row.Version)
			switch {
			case row.AlreadySelected:
				text("row_"+row.Slot, label+"（当前已选用）")
			case row.Outcome == "show_binding_preview":
				text("row_"+row.Slot, "拟选用："+label+"。仅影响以后此会话；尚未绑定。")
				if !readOnlyAssistant(r) && row.PreviewSession == sessionProof && row.PreviewRevision == card.Revision && row.ConfirmBy > time.Now().UnixMilli() {
					in := assistantSkillConfirmation{Run: run, Slot: row.Slot, Revision: row.PreviewRevision, Expiry: row.ConfirmBy}
					in.Proof = in.signature(rt, session, actor, conversation)
					forms = append(forms, assistantSkillConfirmationForm{assistantSkillConfirmation: in, Label: label})
				} else {
					text("next_"+row.Slot, "当前登录的这次预览不能确认。请重新查询，或在本会话配置中独立保存。")
				}
			case row.Outcome == "ask_for_details":
				text("row_"+row.Slot, label+"：请在原对话补充要核对的内容。未绑定。")
			case row.Outcome == "cannot_prepare":
				text("row_"+row.Slot, label+"：暂不能准备预览。未绑定。")
			default:
				text("row_"+row.Slot, label)
				id := assistantReadRowComponent(rt, "skillpreview", surface, row.Slot, row.Fingerprint)
				labelID := "label_" + row.Slot
				children = append(children, id)
				button := a2uiButton(id, labelID, assistantSkillPreviewAction, map[string]any{"skill_slot": row.Slot, "revision": card.Revision})
				button["variant"] = "borderless"
				nodes = append(nodes, a2uiText(labelID, assistantSurfaceClip("预览绑定："+label+"（"+row.Slot+"）")), button)
			}
		}
		if card.Suppressed {
			text("stale", "有条目已停用、来源变化或版本失效；请重新查询。")
		} else if len(card.Rows) == 0 && !card.More {
			text("empty", "没有可选的已批准且启用的 Skill。导入草稿不能绑定。")
		}
		if card.More {
			text("limit", "这里只显示有限条目，完整可选目录见本会话配置。")
		}
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil),
		"binding_confirmations": forms, "binding_receipt": card.Applied})
}

func (s Server) assistantSkillPreview(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantSkillCardScope(w, r)
	if !ok {
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantSkillPreviewAction, field: "skill_slot", component: "skillpreview", decision: "show_binding_preview", revision: card.Revision,
		visible: func(slot string) bool { _, ok := card.Selectable(slot); return ok },
		proof:   func(slot string) string { row, _ := card.Selectable(slot); return row.Fingerprint },
		saved:   func(slot string) bool { row, _ := card.Selectable(slot); return row.Outcome != "" },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.AssistantSkillOptions(ctx, s.principal(r), r.PathValue("id"), r.PathValue("run"))
			card = fresh
			return card.Revision, err
		},
		claim: s.Store.BeginAssistantSkillPreview,
		finish: func(ctx context.Context, a work.AssistantReadCardAction, outcome string) error {
			sessionProof := assistantSkillPreviewSession(rt, s.assistantSession(r), a.Actor, a.Conversation, a.Run)
			return s.Store.FinishAssistantSkillPreview(ctx, a, outcome, sessionProof)
		},
		fail: s.Store.FailAssistantSkillPreview, interpret: s.Settings.AssistantSkillBindingPreviewDecision,
		render: func() { s.writeAssistantSkillCard(w, r, rt, card) },
	})
}

// The fixed human confirmation reuses the normal conversation binding PUT.
// It does not accept an A2UI event, arbitrary Skill identity, or model output.
func (s Server) confirmAssistantSkillPreview(w http.ResponseWriter, r *http.Request, in assistantSkillConfirmation) {
	rt, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	session, actor, conversation := s.assistantSession(r), s.principal(r), r.PathValue("id")
	if readOnlyAssistant(r) || rt == nil || !rt.ready || session == "" || !validID(in.Run) ||
		len(in.Slot) != 1 || in.Slot < "1" || in.Slot > "8" || in.Revision < 0 || in.Expiry <= time.Now().UnixMilli() ||
		len(in.Proof) != 32 || !hmac.Equal([]byte(in.Proof), []byte(in.signature(rt, session, actor, conversation))) {
		fail(w, 409, "skill_binding_preview_stale", work.ErrAssistantSkillPreviewStale.Error())
		return
	}
	if err := s.verifyA2UIAccount(r.Context(), session, actor, false); err != nil {
		writeError(w, err)
		return
	}
	out, err := s.Store.ConfirmAssistantSkillPreview(r.Context(), work.AssistantSkillPreviewConfirmation{
		Actor: actor, Conversation: conversation, Run: in.Run, Slot: in.Slot,
		Session: assistantSkillPreviewSession(rt, session, actor, conversation, in.Run), Revision: in.Revision, ConfirmBy: in.Expiry,
	})
	if errors.Is(err, work.ErrAssistantSkillPreviewStale) {
		fail(w, 409, "skill_binding_preview_stale", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, out)
}
