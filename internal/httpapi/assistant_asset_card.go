package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"clusbot/internal/work"
)

const assistantAssetLedgerAction = "query_asset_ledger"

func (s Server) assistantAssetCardScope(w http.ResponseWriter, r *http.Request) (*discoveryA2UIRuntime, work.AssistantAssetCard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	runtime, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !runtime.ready || s.assistantSession(r) == "" {
		fail(w, 503, "a2ui_asset_card_unavailable", "资产台账卡暂不可用。")
		return nil, work.AssistantAssetCard{}, false
	}
	if !validConversationID(r.PathValue("id")) || !validID(r.PathValue("run")) {
		fail(w, 404, "a2ui_asset_card_missing", "当前会话没有这张资产台账卡。")
		return nil, work.AssistantAssetCard{}, false
	}
	if err := s.verifyA2UIAccount(r.Context(), s.assistantSession(r), s.principal(r), false); err != nil {
		writeError(w, err)
		return nil, work.AssistantAssetCard{}, false
	}
	card, err := s.Store.AssistantAssetCard(r.Context(), s.principal(r), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeError(w, err)
		return nil, card, false
	}
	return runtime, card, true
}

func (s Server) assistantAssetCard(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantAssetCardScope(w, r)
	if ok {
		s.writeAssistantAssetCard(w, r, runtime, card)
	}
}

func assetLedgerComponent(runtime *discoveryA2UIRuntime, surface, slot string) string {
	return assistantReadRowComponent(runtime, "asset", surface, slot)
}

func assetLedgerState(state string) string {
	switch state {
	case work.AssetStatePending, work.AssetStateRecheck, work.AssetStatePendingDebug, work.AssetStateDebugging,
		work.AssetStatePendingDeliver, work.AssetStateInService, work.AssetStateMaintenance, work.AssetStateLent, work.AssetStateRetired:
		return state
	default:
		return "未记录"
	}
}

func (s Server) writeAssistantAssetCard(w http.ResponseWriter, r *http.Request, runtime *discoveryA2UIRuntime, card work.AssistantAssetCard) {
	conversation, run := r.PathValue("id"), r.PathValue("run")
	surface, err := runtime.surface(s.assistantSession(r), conversation, assistantAssetLedgerAction, run, card.Revision)
	if err != nil {
		fail(w, 503, "a2ui_asset_card_unavailable", "资产台账卡暂不可用。")
		return
	}
	children := []string{"heading", "scope"}
	nodes := []map[string]any{nil, a2uiText("heading", "我的资产台账"), a2uiText("scope", "当前账号的台账记录，不代表设备实时状态。")}
	for _, row := range card.Rows {
		ref := assistantSurfaceValue(row.Ref)
		if ref == "" {
			ref = "未命名资产"
		}
		kind := assistantEnum(row.Kind, "资产", map[string]string{
			"node": "节点", "host": "主机", "server": "服务器", "switch": "交换机", "gpu": "加速卡",
			"dcu": "加速卡", "pod": "POD", "cluster": "集群", "accelerator": "加速卡",
		})
		id := "row_" + row.Slot
		children = append(children, id)
		nodes = append(nodes, a2uiText(id, assistantSurfaceClip(assistantSurfaceParts(ref, kind, assetLedgerState(row.State)))))
		if status, exists := card.Status[row.Slot]; exists {
			id := "result_" + row.Slot
			children = append(children, id)
			nodes = append(nodes, a2uiText(id, assetLedgerStatusText(status)))
			continue
		}
		button, label := assetLedgerComponent(runtime, surface, row.Slot), "label_"+row.Slot
		children = append(children, button)
		component := a2uiButton(button, label, assistantAssetLedgerAction, map[string]any{"asset_slot": row.Slot, "revision": card.Revision})
		component["variant"] = "borderless"
		nodes = append(nodes, a2uiText(label, assistantSurfaceClip("核对台账状态："+ref)), component)
	}
	if card.Suppressed {
		children = append(children, "unavailable")
		nodes = append(nodes, a2uiText("unavailable", "有资产已变更或不可查看。请重新查询当前台账。"))
	} else if len(card.Rows) == 0 {
		children = append(children, "empty")
		nodes = append(nodes, a2uiText("empty", "当前账号暂无可读的资产台账记录。"))
	}
	if card.More {
		children = append(children, "limit")
		nodes = append(nodes, a2uiText("limit", "台账有更多资产，本卡仅列出前 10 项。"))
	}
	nodes[0] = a2uiColumn(children)
	jsonResponse(w, 200, map[string]any{"conversation_id": conversation, "run_id": run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}

func assetLedgerStatusText(v work.AssistantAssetStatus) string {
	switch v.Decision {
	case "ask_for_details":
		return "请在对话里补充你要核对的内容。资产没有被修改。"
	case "show_ledger":
		health := assistantEnum(v.Health, "未记录", map[string]string{"healthy": "健康", "degraded": "降级", "unhealthy": "异常", "failed": "故障"})
		reach := assistantEnum(v.Reachability, "未记录", map[string]string{"reachable": "可达", "unreachable": "不可达"})
		use := assistantEnum(v.Occupancy, "未记录", map[string]string{"free": "空闲", "occupied": "占用"})
		locked, observed := "", ""
		if v.Frozen {
			locked = " · 已冻结"
		}
		if v.ObservedAt > 0 {
			observed = " · 状态依据时间 " + time.UnixMilli(v.ObservedAt).UTC().Format("2006-01-02 15:04 UTC")
		}
		return fmt.Sprintf("已核对台账（记录 %d）：%s%s · 健康：%s · 可达性：%s · 业务占用：%s%s",
			v.Revision, assetLedgerState(v.State), locked, health, reach, use, observed)
	default:
		return "助手暂不能核对这项资产。未连接设备，资产没有被修改。"
	}
}

func (s Server) assistantAssetStatus(w http.ResponseWriter, r *http.Request) {
	runtime, card, ok := s.assistantAssetCardScope(w, r)
	if !ok {
		return
	}
	hooks := assistantReadSlotHooks{
		action: assistantAssetLedgerAction, field: "asset_slot", component: "asset", decision: "show_ledger",
		revision: card.Revision,
		visible:  func(slot string) bool { _, ok := card.Row(slot); return ok },
		saved:    func(slot string) bool { _, ok := card.Status[slot]; return ok },
		refresh: func(ctx context.Context) (int64, error) {
			fresh, err := s.Store.AssistantAssetCard(ctx, s.principal(r), r.PathValue("id"), r.PathValue("run"))
			card = fresh
			return card.Revision, err
		},
		claim: s.Store.BeginAssistantAssetStatus, finish: s.Store.FinishAssistantAssetStatus,
		fail: s.Store.FailAssistantAssetStatus, interpret: s.Settings.AssistantAssetLedgerDecision,
		render: func() { s.writeAssistantAssetCard(w, r, runtime, card) },
	}
	s.assistantReadSlotAction(w, r, runtime, hooks)
}
