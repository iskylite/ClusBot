package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"clusbot/internal/work"
)

const assistantMemoryReviewAction = "review_memory_provenance"

func (s Server) assistantMemoryProvenance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rt, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	session, actor, conversation, run := s.assistantSession(r), s.principal(r), r.PathValue("id"), r.PathValue("run")
	if rt == nil || !rt.ready || session == "" {
		fail(w, 503, "a2ui_memory_unavailable", "记忆来源暂不可核对。")
		return
	}
	if !validConversationID(conversation) || !validID(run) {
		fail(w, 404, "a2ui_memory_missing", "当前会话没有这条回复。")
		return
	}
	if err := s.verifyA2UIAccount(r.Context(), session, actor, false); err != nil {
		writeError(w, err)
		return
	}
	card, err := s.Store.ReadAssistantMemoryProvenance(r.Context(), actor, conversation, run, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.writeAssistantMemoryProvenance(w, r, rt, card, "", "")
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantMemoryReviewAction, field: "memory_slot", component: "memorysource", decision: "show_source", revision: card.Revision,
		visible: func(slot string) bool { _, ok := card.SourceSlot(slot); return ok },
		proof:   func(slot string) string { row, _ := card.SourceSlot(slot); return row.Fingerprint },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.ReadAssistantMemoryProvenance(ctx, actor, conversation, run, nil)
			card = fresh
			return card.Revision, err
		},
		interpret: s.Settings.AssistantMemoryProvenanceDecision,
		present: func(ctx context.Context, a work.AssistantReadCardAction, decision string) error {
			slot, _, _ := strings.Cut(a.Target, ":")
			if decision == "show_source" {
				// Fetch the source text only after the model returned a permitted
				// choice, and in the same read transaction as authority validation.
				fresh, err := s.Store.ReadAssistantMemoryProvenance(ctx, actor, conversation, run, &a)
				if err != nil {
					return err
				}
				card = fresh
			}
			s.writeAssistantMemoryProvenance(w, r, rt, card, slot, decision)
			return nil
		},
	})
}

func (s Server) writeAssistantMemoryProvenance(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime,
	card work.AssistantMemoryProvenance, selected, decision string) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := rt.surface(s.assistantSession(r), conversation, assistantMemoryReviewAction, run, card.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	var children []string
	nodes := []map[string]any{nil}
	text := func(id, line string) {
		children = append(children, id)
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(line)))
	}
	text("heading", "本条回复的记忆来源")
	for _, row := range card.Rows {
		suffix := row.Slot
		line := fmt.Sprintf("第 %s 条 · 本轮使用第 %d 版", suffix, row.UsedRevision)
		switch row.State {
		case "memory_removed":
			line += " · 记忆已删除，不再展示来源"
		case "source_removed":
			line += " · 来源已撤回，不再展示原文"
		case "version_changed":
			line += fmt.Sprintf(" · 当前为第 %d 版，原引用与当前版本不同", row.CurrentRevision)
		case "unlinked":
			line += " · 无关联的来源消息"
		case "different_conversation":
			line += " · 来源位于其他会话，此处不展开"
		case "local_source":
			line += " · 来源位于本会话"
		}
		text("row_"+suffix, line)
		if row.State != "local_source" {
			continue
		}
		if selected == suffix {
			switch decision {
			case "show_source":
				excerpt := assistantSurfaceValue(card.Excerpt)
				if excerpt == "" || assistantCredentialValue.MatchString(excerpt) {
					text("excerpt_"+suffix, "原消息没有可安全展示的片段。")
				} else {
					text("excerpt_"+suffix, "本会话原消息摘录："+excerpt)
					if card.ExcerptTruncated {
						text("limit_"+suffix, fmt.Sprintf("仅显示前 %d 个字符。", work.AssistantMemoryExcerptLimit))
					}
				}
				continue
			case "ask_for_details":
				text("next_"+suffix, "需要更多信息。可在原对话补充你要核对的来源。")
			default:
				text("next_"+suffix, "助手这次未能核对来源。")
			}
		}
		btn := assistantReadRowComponent(rt, "memorysource", surface, suffix, row.Fingerprint)
		label := "label_" + suffix
		children = append(children, btn)
		node := a2uiButton(btn, label, assistantMemoryReviewAction, map[string]any{"memory_slot": suffix, "revision": card.Revision})
		node["variant"] = "borderless"
		title := assistantSurfaceValue(row.Title)
		if title == "" || assistantCredentialValue.MatchString(title) {
			title = "第 " + suffix + " 条"
		}
		nodes = append(nodes, a2uiText(label, assistantSurfaceClip("核对来源："+title+"（"+suffix+"）")), node)
	}
	if len(card.Rows) == 0 {
		text("empty", "本条回复没有记录使用个人记忆。")
	}
	if card.More {
		text("limit", "这里只列出本轮前 16 条记录。")
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}
