package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"clusbot/internal/work"
)

const assistantArtifactPreviewAction = "preview_artifact_text"

func (s Server) assistantArtifactCardScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantArtifactCard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	rt, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !rt.ready || s.assistantSession(r) == "" {
		fail(w, 503, "a2ui_artifact_card_unavailable", "成果清单暂不可读取。")
		return nil, work.AssistantArtifactCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "a2ui_artifact_card_missing", "当前对话没有这张成果清单。")
		return nil, work.AssistantArtifactCard{}, false
	}
	if err := s.verifyA2UIAccount(r.Context(), s.assistantSession(r), s.principal(r), false); err != nil {
		writeError(w, err)
		return nil, work.AssistantArtifactCard{}, false
	}
	card, err := s.Store.AssistantArtifactCard(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return rt, card, true
}

func (s Server) assistantArtifactCard(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantArtifactCardScope(w, r)
	if ok {
		s.writeAssistantArtifactCard(w, r, rt, card)
	}
}

func (s Server) writeAssistantArtifactCard(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime, card work.AssistantArtifactCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := rt.surface(s.assistantSession(r), conversation, assistantArtifactPreviewAction, run, card.Revision)
	if err != nil {
		fail(w, 503, "a2ui_artifact_card_unavailable", "成果清单暂不可读取。")
		return
	}
	children := []string{"heading"}
	nodes := []map[string]any{nil, a2uiText("heading", "我的成果")}
	for _, row := range card.Rows {
		id := "row_" + row.Slot
		name := assistantSurfaceValue(row.Name)
		if name == "" {
			name = "未命名成果"
		}
		state := "尚未验收"
		if row.Accepted {
			state = "此份成果已验收"
		}
		children = append(children, id)
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(assistantSurfaceParts(name, row.TaskTitle, state))))
		if saved, exists := card.Previews[row.Slot]; exists {
			id := "preview_" + row.Slot
			children = append(children, id)
			var parts []string
			for i, line := range artifactPreviewLines(saved) {
				lineID := id + "_" + strconv.Itoa(i)
				parts = append(parts, lineID)
				nodes = append(nodes, a2uiText(lineID, line))
			}
			nodes = append(nodes, map[string]any{"id": id, "component": "Column", "children": parts})
			continue
		}
		btn := assistantReadRowComponent(rt, "artifact", surface, row.Slot)
		label := "label_" + row.Slot
		children = append(children, btn)
		component := a2uiButton(btn, label, assistantArtifactPreviewAction, map[string]any{"artifact_slot": row.Slot, "revision": card.Revision})
		component["variant"] = "borderless"
		nodes = append(nodes, a2uiText(label, assistantSurfaceClip("预览正文："+assistantSurfaceParts(name, row.TaskTitle)+"（"+row.Slot+"）")), component)
	}
	if card.Suppressed {
		children = append(children, "unavailable")
		nodes = append(nodes, a2uiText("unavailable", "有成果已变化或不可查看。原预览不再显示。"))
	} else if len(card.Rows) == 0 {
		children = append(children, "empty")
		nodes = append(nodes, a2uiText("empty", "当前没有可查看的成果。"))
	}
	if card.More {
		children = append(children, "limit")
		nodes = append(nodes, a2uiText("limit", "这里只列最近 10 项可查看的成果。"))
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}

func artifactPreviewLines(v work.AssistantArtifactPreview) []string {
	switch v.Decision {
	case "ask_for_details":
		return []string{"请在对话中补充你想查看的内容。"}
	case "show_preview":
		if v.Unreadable {
			return []string{"这个成果没有可安全显示的纯文本预览。未下载或执行文件。"}
		}
		// Basic Catalog Text is rendered by React as literal text. Never
		// hand these untrusted lines to Markdown, a href, or an HTML sink.
		// Use one text node for the body, keeping the total surface bounded.
		preview := strings.Join(v.Lines, "\n")
		if strings.TrimSpace(preview) == "" {
			return []string{"成果正文为空。"}
		}
		lines := []string{"纯文本预览", preview}
		if v.Truncated {
			lines = append(lines, "仅展示前 600 个字符或前 8 行，以先到者为准。")
		}
		return lines
	default:
		return []string{"助手暂不能预览这项成果。"}
	}
}

func (s Server) assistantArtifactPreview(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantArtifactCardScope(w, r)
	if !ok {
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantArtifactPreviewAction, field: "artifact_slot", component: "artifact", decision: "show_preview",
		revision: card.Revision, visible: func(slot string) bool { return card.HasSlot(slot) },
		saved: func(slot string) bool { _, ok := card.Previews[slot]; return ok },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.AssistantArtifactCard(ctx, s.principal(r), r.PathValue("id"), r.PathValue("run"))
			card = fresh
			return card.Revision, err
		},
		claim: s.Store.BeginAssistantArtifactPreview, finish: s.Store.FinishAssistantArtifactPreview,
		fail: s.Store.FailAssistantArtifactPreview, interpret: s.Settings.AssistantArtifactPreviewDecision,
		render: func() { s.writeAssistantArtifactCard(w, r, rt, card) },
	})
}
