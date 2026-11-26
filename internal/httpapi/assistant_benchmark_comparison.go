package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"clusbot/internal/work"
)

const assistantBenchmarkCompareAction = "preview_benchmark_comparison"

func (s Server) submitAssistantBenchmarkComparison(w http.ResponseWriter, r *http.Request, text, key string) bool {
	words := strings.Fields(text)
	if len(words) == 0 || words[0] != "比较基准报告" && words[0] != "对比基准报告" {
		return false
	}
	if len(words) != 4 || strings.ContainsAny(strings.TrimSpace(text), "\r\n") || !validID(words[1]) || !validID(words[2]) || words[1] == words[2] {
		fail(w, 400, "benchmark_comparison_input", "请按“比较基准报告 <基线任务ID> <本次任务ID> <测项>”输入；两个任务使用不同的完整编号。")
		return true
	}
	run, err := s.Store.OpenAssistantBenchmarkComparison(r.Context(), s.principal(r), r.PathValue("id"), key, text,
		work.AssistantBenchmarkComparisonRequest{Baseline: words[1], Current: words[2], Case: words[3]})
	if err != nil {
		benchmarkError(w, err)
		return true
	}
	writeSubmission(w, run)
	return true
}

func (s Server) assistantBenchmarkComparison(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rt, _ := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	session, actor, conversation, run := s.assistantSession(r), s.principal(r), r.PathValue("id"), r.PathValue("run")
	if rt == nil || !rt.ready || session == "" {
		fail(w, 503, "a2ui_comparison_unavailable", "基准报告比较暂不可用。")
		return
	}
	if !validConversationID(conversation) || !validID(run) {
		fail(w, 404, "a2ui_comparison_missing", "当前对话没有这份报告比较。")
		return
	}
	if err := s.verifyA2UIAccount(r.Context(), session, actor, false); err != nil {
		writeError(w, err)
		return
	}
	card, err := s.Store.ReadAssistantBenchmarkComparison(r.Context(), actor, conversation, run)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.writeAssistantBenchmarkComparison(w, r, rt, card, "")
		return
	}
	s.assistantReadSlotAction(w, r, rt, assistantReadSlotHooks{
		action: assistantBenchmarkCompareAction, field: "report_slot", component: "reportcompare", decision: "show_comparison", revision: card.Revision,
		visible: func(slot string) bool { return slot == "1" && card.Proof != "" },
		proof:   func(_ string) string { return card.Proof },
		refresh: func(ctx context.Context) (int64, error) {
			next, err := s.Store.ReadAssistantBenchmarkComparison(ctx, actor, conversation, run)
			card = next
			return card.Revision, err
		},
		interpret: s.Settings.AssistantBenchmarkComparisonDecision,
		present: func(_ context.Context, _ work.AssistantReadCardAction, decision string) error {
			s.writeAssistantBenchmarkComparison(w, r, rt, card, decision)
			return nil
		},
	})
}

func comparisonReadLabel(value string) string {
	switch value {
	case "verified":
		return "原有测量已核实"
	case "missing_required_evidence":
		return "缺必需原始证据"
	case "unit_or_value_unverified":
		return "指标、单位或数值未核实"
	default:
		return "没有完整可核实的结果"
	}
}

func comparisonSafeLabel(value string) string {
	if assistantCredentialValue.MatchString(value) || assistantSurfaceValue(value) == "" {
		return "名称已省略"
	}
	return assistantSurfaceValue(value)
}

func comparisonMetric(m *work.BenchmarkMeasuredMetric) string {
	if m == nil {
		return "无可核实记录"
	}
	return strconv.FormatFloat(m.Value, 'g', 8, 64) + " " + comparisonSafeLabel(m.Unit)
}

func (s Server) writeAssistantBenchmarkComparison(w http.ResponseWriter, r *http.Request, rt *discoveryA2UIRuntime,
	card work.AssistantBenchmarkComparison, decision string) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := rt.surface(s.assistantSession(r), conversation, assistantBenchmarkCompareAction, run, card.Revision)
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
	text("heading", "基准报告对比")
	text("scope", "测项："+comparisonSafeLabel(card.Case))
	text("boundary", "只读取原报告。不会启动测试、生成文件或发布报告。")
	switch decision {
	case "show_comparison":
		text("basis", "基线："+comparisonReadLabel(card.BaselineStatus)+"；本次："+comparisonReadLabel(card.CurrentStatus)+"。")
		var reason string
		switch card.Reason {
		case "comparable":
			reason = "已核验的声明条件相同，可显示本次减基线的差值。未记录的环境不能核实，差值不代表原因。"
		case "target_scope":
			reason = "资产范围或冻结版本不同，只并列，不算差值。"
		case "metric_or_unit":
			reason = "指标或单位不同，或差值超出可表示范围。只并列，不算差值。"
		case "method_unverified":
			reason = "方法、方案版本或声明条件不同，或缺少核对依据。只并列，不算差值。"
		default:
			reason = "至少一份报告的证据不足。只显示已核实的记录，不算差值。"
		}
		text("reason", reason)
		for i, row := range card.Rows {
			prefix := assistantSurfaceClip(comparisonSafeLabel(row.Target) + " · " + comparisonSafeLabel(row.Metric))
			// Keep independently labelled short values after the title, even
			// when target names are long or the units are different.
			text("metric_"+strconv.Itoa(i), prefix)
			line := "基线 " + comparisonMetric(row.Baseline) + "；本次 " + comparisonMetric(row.Current)
			if row.Delta != nil {
				line += "；差值 " + strconv.FormatFloat(*row.Delta, 'g', 8, 64) + " " + comparisonSafeLabel(row.Current.Unit)
			}
			text("value_"+strconv.Itoa(i), line)
		}
		if card.More {
			text("limit", "这里只预览前 8 行。")
		}
	default:
		if decision == "ask_for_details" {
			text("notice", "需要补充核对目标，请在原对话说明。")
		} else if decision != "" {
			text("notice", "助手这次无法展开报告。")
		}
		btn := assistantReadRowComponent(rt, "reportcompare", surface, "1", card.Proof)
		children = append(children, btn)
		button := a2uiButton(btn, "view_label", assistantBenchmarkCompareAction, map[string]any{"report_slot": "1", "revision": card.Revision})
		button["variant"] = "borderless"
		nodes = append(nodes, a2uiText("view_label", "核对并展开报告对比"), button)
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}
