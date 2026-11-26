package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"clusbot/internal/work"
)

const assistantDiscoveryScheduleAction = "preview_discovery_schedule"

func (s Server) submitAssistantDiscoverySchedule(w http.ResponseWriter, r *http.Request, key, text string) {
	if err := s.verifyA2UIAccount(r.Context(), s.assistantSession(r), s.principal(r), true); err != nil {
		writeError(w, err)
		return
	}
	run, err := s.Store.OpenAssistantDiscoverySchedule(r.Context(), s.principal(r), r.PathValue("id"), key, text, s.DiscoverySource)
	if err != nil {
		writeError(w, err)
		return
	}
	writeSubmission(w, run)
}

func (s Server) assistantDiscoverySchedule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rt, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	session, actor, conversation, run := s.assistantSession(r), s.principal(r), r.PathValue("id"), r.PathValue("run")
	if rt == nil || !rt.ready || session == "" {
		fail(w, 503, "a2ui_schedule_unavailable", "定时发现状态暂不可读取。")
		return
	}
	if !validConversationID(conversation) || !validID(run) {
		fail(w, 404, "a2ui_schedule_missing", "当前会话没有这份定时发现核对。")
		return
	}
	if err := s.verifyA2UIAccount(r.Context(), session, actor, true); err != nil {
		writeError(w, err)
		return
	}
	card, err := s.Store.ReadAssistantDiscoverySchedule(r.Context(), actor, conversation, run, s.DiscoverySource)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.writeAssistantDiscoverySchedule(w, r, rt, card, "")
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantDiscoveryScheduleAction, field: "schedule_slot", component: "scheduleview", decision: "show_schedule", revision: card.Revision,
		visible: func(slot string) bool { return slot == "1" && card.Proof != "" }, proof: func(_ string) string { return card.Proof },
		refresh: func(ctx context.Context) (int64, error) {
			// The shared action helper revalidates the session. This second
			// capability check preserves Discovery's administrator boundary.
			if err := s.verifyA2UIAccount(ctx, session, actor, true); err != nil {
				return 0, err
			}
			fresh, err := s.Store.ReadAssistantDiscoverySchedule(ctx, actor, conversation, run, s.DiscoverySource)
			card = fresh
			return card.Revision, err
		},
		interpret: s.Settings.AssistantDiscoveryScheduleDecision,
		present: func(_ context.Context, _ work.AssistantReadCardAction, decision string) error {
			s.writeAssistantDiscoverySchedule(w, r, rt, card, decision)
			return nil
		},
	})
}

func scheduleUTC(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02 15:04:05 UTC")
}

func (s Server) writeAssistantDiscoverySchedule(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime,
	card work.AssistantDiscoveryScheduleCard, decision string) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := rt.surface(s.assistantSession(r), conversation, assistantDiscoveryScheduleAction, run, card.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	var children []string
	nodes := []map[string]any{nil}
	text := func(id, value string) {
		children = append(children, id)
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(value)))
	}
	text("heading", "定时发现状态")
	text("boundary", "只核对一项已启用的发现计划。不会更改计划、授权或发起探测。")
	plan := card.Current.Plan
	switch {
	case card.Changed:
		text("unavailable", "原计划已变化或不可读取。请重新查询；没有切换到其他计划。")
	case plan == nil:
		if card.Current.State == "source_unavailable" {
			text("unavailable", "当前没有可核对的发现来源。没有探测设备。")
		} else {
			text("unavailable", "目前没有可核对的已启用发现计划。此处不是全部计划清单。")
		}
	case decision == "show_schedule":
		label := assistantSurfaceValue(plan.ID)
		if assistantCredentialValue.MatchString(label) || label == "" {
			label = "名称已省略"
		}
		text("plan", fmt.Sprintf("计划 %s · 第 %d 版 · 每 %d 秒", label, plan.Revision, plan.PeriodSeconds))
		text("next", "记录中的下一计划时点："+scheduleUTC(plan.NextAt)+"。到点仍需由运行时检查授权和来源。")
		last := card.Current.LastRun
		if last == nil {
			text("receipt", "当前计划版本还没有触发记录。")
			break
		}
		text("scheduled", "最近一次记录的计划时点："+scheduleUTC(last.ScheduledAt)+"。")
		switch last.Status {
		case "queued":
			text("receipt", "这次触发已登记，尚未取得结果。")
		case "running":
			text("receipt", "本地运行时记录为处理中，尚未确认结果。")
		case "failed":
			text("receipt", "这次发现未完成，没有可核实的结果。")
		case "skipped":
			text("receipt", "这次时点未执行。现有记录未区分具体跳过原因。")
		case "done":
			if last.ObservationCount != nil {
				text("receipt", fmt.Sprintf("这次发现已记录 %d 条观测。观测数不是纳管数、健康数或总覆盖。", *last.ObservationCount))
			}
			if last.Coverage != nil && last.Coverage.Outcome == "partial" {
				text("coverage", "这次只获得部分覆盖，未确认的目标不计为通过。")
			}
		}
	default:
		if decision == "ask_for_details" {
			text("notice", "可在原对话补充要核对的内容。")
		} else if decision != "" {
			text("notice", "暂未展开这个计划。")
		}
		id := assistantReadRowComponent(rt, "scheduleview", surface, "1", card.Proof)
		children = append(children, id)
		button := a2uiButton(id, "review_label", assistantDiscoveryScheduleAction, map[string]any{"schedule_slot": "1", "revision": card.Revision})
		button["variant"] = "borderless"
		nodes = append(nodes, a2uiText("review_label", "展开计划和最近回执"), button)
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}
