package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"clusbot/internal/work"
)

const assistantAttentionReasonAction = "query_attention_reason"
const assistantApprovalPreviewAction = "preview_pending_approval"

func (s Server) assistantAttentionCardScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantAttentionListCard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	rt, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !rt.ready || s.assistantSession(r) == "" {
		fail(w, 503, "a2ui_attention_unavailable", "待处理事项暂不可读取。")
		return nil, work.AssistantAttentionListCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "a2ui_attention_missing", "当前对话没有这张待处理清单。")
		return nil, work.AssistantAttentionListCard{}, false
	}
	if err := s.verifyA2UIAccount(r.Context(), s.assistantSession(r), s.principal(r), false); err != nil {
		writeError(w, err)
		return nil, work.AssistantAttentionListCard{}, false
	}
	card, err := s.Store.AssistantAttentionCard(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return rt, card, true
}

func (s Server) assistantAttentionCard(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantAttentionCardScope(w, r)
	if ok {
		s.writeAssistantAttentionCard(w, r, rt, card)
	}
}

func attentionStateLabel(s string) string {
	return assistantEnum(s, "状态待核实", map[string]string{
		"decision_required": "等待你的决定", "expired": "审批已过期", "approved": "审批已同意", "rejected": "审批已拒绝",
		"result_pending": "远端结果待核对", "replaced": "等待记录已更新", "result_recorded": "已记录作业结果",
		"wait_closed": "等待已结束", "stop_requested": "已请求停止", "no_longer_pending": "事项已不在等待中",
	})
}

func (s Server) writeAssistantAttentionCard(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime, card work.AssistantAttentionListCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := rt.surface(s.assistantSession(r), conversation, assistantAttentionReasonAction, run, card.Revision)
	if err != nil {
		fail(w, 503, "a2ui_attention_unavailable", "待处理事项暂不可读取。")
		return
	}
	children := []string{"heading"}
	nodes := []map[string]any{nil, a2uiText("heading", "待我处理")}
	for _, row := range card.Rows {
		item := row.Item
		label := "基准作业 · " + item.TaskID
		if item.Kind == "approval" {
			label = "审批 · " + approvalPreviewValue(item.ObjectRef)
		}
		group, id := "group_"+row.Slot, "row_"+row.Slot
		rowChildren := []string{id}
		line := assistantSurfaceParts(label, attentionStateLabel(item.State))
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(line)))
		if saved, exists := card.Reasons[row.Slot]; exists {
			id := "receipt_" + row.Slot
			rowChildren = append(rowChildren, id)
			nodes = append(nodes, a2uiText(id, attentionReasonText(saved)))
		} else {
			btn := assistantReadRowComponent(rt, "attention", surface, row.Slot)
			labelID := "label_" + row.Slot
			rowChildren = append(rowChildren, btn)
			button := a2uiButton(btn, labelID, assistantAttentionReasonAction, map[string]any{"attention_slot": row.Slot, "revision": card.Revision})
			button["variant"] = "borderless"
			nodes = append(nodes, a2uiText(labelID, assistantSurfaceClip("核对原因："+assistantSurfaceValue(label)+"（"+row.Slot+"）")), button)
		}
		if review, ok := card.Reviews[row.Slot]; ok {
			if lines := approvalPreviewLines(review); len(lines) != 0 {
				for index, value := range lines {
					id := fmt.Sprintf("preview_%s_%d", row.Slot, index)
					rowChildren = append(rowChildren, id)
					nodes = append(nodes, a2uiText(id, assistantSurfaceClip(value)))
				}
			} else {
				btn := assistantReadRowComponent(rt, "approvalview", surface, row.Slot, review.Fingerprint)
				labelID := "preview_label_" + row.Slot
				rowChildren = append(rowChildren, btn)
				button := a2uiButton(btn, labelID, assistantApprovalPreviewAction, map[string]any{"attention_slot": row.Slot, "revision": card.Revision})
				button["variant"] = "borderless"
				nodes = append(nodes, a2uiText(labelID, assistantSurfaceClip("查看审批预览："+approvalPreviewValue(review.Object)+"（"+row.Slot+"）")), button)
			}
		}
		children = append(children, group)
		nodes = append(nodes, map[string]any{"id": group, "component": "Column", "children": rowChildren})
	}
	if card.Suppressed {
		children = append(children, "unavailable")
		nodes = append(nodes, a2uiText("unavailable", "有事项已不可查看。请重新查询待处理事项。"))
	} else if len(card.Rows) == 0 {
		children = append(children, "empty")
		nodes = append(nodes, a2uiText("empty", "目前没有待处理事项。"))
	}
	if card.More {
		children = append(children, "limit")
		nodes = append(nodes, a2uiText("limit", "本卡仅列出最近 10 项待处理事项。"))
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}

func attentionReasonText(reason work.AssistantAttentionReason) string {
	switch reason.Decision {
	case "show_reason":
		basis := "来源未核实"
		if reason.Kind == "approval" {
			basis = "本人审批记录"
		} else if reason.Kind == "verified_timer_receipt" {
			basis = "系统的等待核对记录，不是基准验收结论"
		}
		asOf := ""
		if reason.CheckedAt > 0 {
			asOf = "（" + time.UnixMilli(reason.CheckedAt).UTC().Format("2006-01-02 15:04 UTC") + "）"
		}
		return "本次核对" + asOf + "：" + attentionStateLabel(reason.State) + "。依据：" + basis + "。未提交决定或重新执行任务。"
	case "ask_for_details":
		return "请在对话里补充要核对的内容。没有提交决定。"
	default:
		return "助手暂不能核对本项。没有提交决定。"
	}
}

func (s Server) assistantAttentionReason(w http.ResponseWriter, r *http.Request) {
	rt, card, ok := s.assistantAttentionCardScope(w, r)
	if !ok {
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantAttentionReasonAction, field: "attention_slot", component: "attention", decision: "show_reason",
		revision: card.Revision,
		visible:  func(slot string) bool { _, ok := card.Row(slot); return ok },
		saved:    func(slot string) bool { _, ok := card.Reasons[slot]; return ok },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.AssistantAttentionCard(ctx, s.principal(r), r.PathValue("id"), r.PathValue("run"))
			card = fresh
			return card.Revision, err
		},
		claim: s.Store.BeginAssistantAttentionReason, finish: s.Store.FinishAssistantAttentionReason,
		fail: s.Store.FailAssistantAttentionReason, interpret: s.Settings.AssistantAttentionReasonDecision,
		render: func() { s.writeAssistantAttentionCard(w, r, rt, card) },
	})
}
