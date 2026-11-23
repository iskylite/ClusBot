package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

var assistantPODMention = regexp.MustCompile(`(?i)\bpod[ \t_-]*[a-z0-9]+(?:[-_][a-z0-9]+)*`)
var assistantSubresourceMention = regexp.MustCompile(`(?i)(?:\b(?:node|host|gpu)[-_][a-z0-9][a-z0-9_-]*|节点[ \t:_-]*[a-z0-9][a-z0-9_-]*|[0-9]+号节点)`)
var assistantOperationalAction = regexp.MustCompile(`(?:^|[，,。;；])\s*(?:(?:请|帮我|立即|直接|先|再|同时)\s*)*(?:重启|隔离|停机|删除|提交工单|创建工单|执行操作|运行命令)`)
var errAssistantEgressDenied = errors.New("assistant inventory model egress denied")
var errAssistantEgressCheck = errors.New("assistant inventory model egress check unavailable")

type assistantOperationScope struct {
	pod, focus          string
	global, subresource bool
}

// Only a bounded, explicit operational recommendation is handled here.
// Everything else retains the ordinary message or explicit command path.
func parseAssistantOperationScope(text string) (assistantOperationScope, bool) {
	var scope assistantOperationScope
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "/") || assistantCredentialValue.MatchString(text) {
		return scope, false
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "建议") && !strings.Contains(lower, "方案") && !strings.Contains(lower, "选项") {
		return scope, false
	}
	requested := false
	for _, term := range []string{"给", "提出", "提供", "生成", "制定", "推荐", "列出", "有哪些", "有什么", "有啥", "怎么", "如何"} {
		requested = requested || strings.Contains(lower, term)
	}
	if !requested || assistantOperationalAction.MatchString(text) {
		return scope, false
	}
	// Do not silently discard a negative condition, another requested action,
	// or a separate question when interpreting a message as an advisory.
	for _, term := range []string{"不要", "不得", "无需", "不需要", "请勿", "不能", "假设", "如果", "并执行", "并立即", "并直接", "然后执行", "然后重启", "顺便", "另外", "天气"} {
		if strings.Contains(lower, term) {
			return scope, false
		}
	}
	matches := assistantPODMention.FindAllString(text, -1)
	unique := map[string]bool{}
	for _, match := range matches {
		unique[assistantNormalizePOD(match)] = true
	}
	if len(unique) == 1 {
		for name := range unique {
			scope.pod = name
		}
	} else if len(unique) > 1 {
		// A recognized but ambiguous request is handled with a clarification,
		// never by choosing one or handing it to another model.
		scope.pod = "ambiguous"
	} else if (strings.Contains(lower, "全局") || strings.Contains(lower, "全部") || strings.Contains(lower, "所有")) &&
		(strings.Contains(lower, "pod") || strings.Contains(lower, "集群") || strings.Contains(lower, "节点")) {
		scope.global = true
	} else {
		return scope, false
	}
	scope.subresource = assistantSubresourceMention.MatchString(text)
	scope.focus = "node_investigation"
	if strings.Contains(lower, "慢训练") {
		scope.focus = "reported_slow_training"
	} else if strings.Contains(lower, "恢复") {
		scope.focus = "recovery"
	}
	return scope, true
}

func assistantNormalizePOD(value string) string {
	return strings.NewReplacer(" ", "", "\t", "", "-", "", "_", "").Replace(strings.ToLower(value))
}

type assistantOperationEvidence struct {
	Scope      string                   `json:"scope"`
	Pod        *assistantOperationPod   `json:"pod,omitempty"`
	TotalNodes int                      `json:"total_nodes"`
	Health     work.ClusterHealthCounts `json:"health"`
	ComputedAt int64                    `json:"computed_at"`
}

type assistantOperationPod struct {
	ObjectRef string `json:"object_ref"`
	Identity  string `json:"identity"`
}

func assistantOperationFailure(tool, message string) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"state": "failed", "tool": tool, "message": message})
	return data
}

func assistantOperationBatch(w http.ResponseWriter, items []json.RawMessage) {
	jsonResponse(w, http.StatusOK, struct {
		State string            `json:"state"`
		Items []json.RawMessage `json:"items"`
	}{"batch_completed", items})
}

func assistantOperationWithSuggestion(items []json.RawMessage, suggestion json.RawMessage) []json.RawMessage {
	return append(append(append([]json.RawMessage{}, items[:1]...), suggestion), items[1:]...)
}

func assistantInventoryPage(r *http.Request, bridge *assistantBridge, query url.Values) (work.ClusterInventoryPage, json.RawMessage, bool) {
	path := "/api/v1/domain/cluster-inventory?" + query.Encode()
	rec, err := bridge.request(r, assistantPreview{method: http.MethodGet, path: path})
	var page work.ClusterInventoryPage
	if err != nil || rec.overflow || rec.status != http.StatusOK || json.Unmarshal(rec.Bytes(), &page) != nil {
		return page, nil, false
	}
	return page, append(json.RawMessage(nil), rec.Bytes()...), true
}

func assistantInventoryReceipt(raw json.RawMessage) json.RawMessage {
	receipt := map[string]any{"state": "completed", "tool": "get_cluster_inventory", "result": raw}
	if surface := assistantResultSurface("get_cluster_inventory", http.MethodGet, raw); len(surface) > 0 {
		receipt["surface"] = surface
	}
	result, _ := json.Marshal(receipt)
	return result
}

func assistantEvidenceReceipt(evidence assistantOperationEvidence) json.RawMessage {
	result, _ := json.Marshal(map[string]any{"state": "completed", "tool": "get_operation_evidence", "result": evidence})
	return result
}

func assistantFindPOD(summary work.ClusterInventorySummary, name string) (work.ClusterInventoryPod, bool) {
	var selected work.ClusterInventoryPod
	found := false
	for _, pod := range summary.Pods {
		if assistantNormalizePOD(pod.ObjectRef) != name && assistantNormalizePOD(pod.Identity) != name {
			continue
		}
		if found && selected.ObjectRef != pod.ObjectRef {
			return work.ClusterInventoryPod{}, false
		}
		selected, found = pod, true
	}
	return selected, found
}

// A current conversation can have a task whose own recorded scope names the
// same POD. Show that task through its existing authorized card; its freeform
// fields and messages are not sent to the recommendation model.
func (s Server) assistantOperationTask(r *http.Request, scope assistantOperationScope, text string) json.RawMessage {
	conversation := r.PathValue("id")
	if scope.global || !validID(conversation) || (!strings.Contains(text, "任务") && !strings.Contains(text, "训练")) {
		return nil
	}
	detail, err := s.Store.Detail(conversation, false)
	if err != nil || detail.Conversation.Archived || detail.Task == nil || detail.Task.ConversationID != conversation {
		return nil
	}
	matches := assistantPODMention.FindAllString(detail.Task.Scope, -1)
	if len(matches) != 1 || assistantNormalizePOD(matches[0]) != scope.pod {
		return nil
	}
	visible, err := s.Store.MessageTaskReferenceOptions(r.Context(), s.principal(r), conversation)
	if err != nil || !visible[detail.Task.ID] {
		return nil
	}
	input, _ := json.Marshal(assistantToolInput{Tool: "get_task", Params: map[string]string{"id": detail.Task.ID}})
	return s.invokeAssistantBatchItem(r, input, false)
}

// Verify that the exact nodes making up the displayed summary have been read
// through the same authenticated inventory endpoint. A changing or oversized
// inventory cannot be safely sent as a supposedly complete summary.
func assistantOperationMembers(r *http.Request, bridge *assistantBridge, podRef string, expected assistantOperationEvidence) ([]string, bool) {
	if expected.TotalNodes < 1 || expected.TotalNodes > work.AssistantInventoryEvidenceLimit {
		return nil, false
	}
	query := url.Values{"limit": {"100"}}
	if podRef != "" {
		query.Set("pod_ref", podRef)
	}
	refs := make([]string, 0, expected.TotalNodes+1)
	pods := map[string]bool{}
	if podRef != "" {
		refs = append(refs, podRef)
		pods[podRef] = true
	}
	seen, cursors := map[string]bool{}, map[string]bool{}
	var counts work.ClusterHealthCounts
	for pageNumber := 0; pageNumber < (work.AssistantInventoryEvidenceLimit+99)/100; pageNumber++ {
		page, _, ok := assistantInventoryPage(r, bridge, query)
		if !ok {
			return nil, false
		}
		for _, node := range page.Items {
			if node.ObjectRef == "" || seen[node.ObjectRef] || (podRef != "" && node.PodRef != podRef) {
				return nil, false
			}
			seen[node.ObjectRef] = true
			refs = append(refs, node.ObjectRef)
			if node.PodRef != "" && !pods[node.PodRef] {
				refs = append(refs, node.PodRef)
				pods[node.PodRef] = true
			}
			switch node.Health {
			case "healthy":
				counts.Healthy++
			case "degraded":
				counts.Degraded++
			case "unhealthy":
				counts.Unhealthy++
			case "unknown":
				counts.Unknown++
			default:
				return nil, false
			}
		}
		if page.NextCursor == "" {
			return refs, len(seen) == expected.TotalNodes && counts == expected.Health
		}
		if cursors[page.NextCursor] {
			return nil, false
		}
		cursors[page.NextCursor] = true
		query.Set("after", page.NextCursor)
	}
	return nil, false
}

func (s Server) assistantOperationChat(w http.ResponseWriter, r *http.Request, text string) bool {
	if r.Header.Get("X-ClusBot-Assistant-Cards") != "1" {
		return false
	}
	return s.assistantOperationAdvice(w, r, text)
}

func (s Server) assistantOperationAdvice(w http.ResponseWriter, r *http.Request, text string) bool {
	scope, matched := parseAssistantOperationScope(text)
	if !matched || readOnlyAssistant(r) {
		return false
	}
	if scope.pod == "ambiguous" {
		fail(w, http.StatusBadRequest, "assistant_scope_ambiguous", "请指定一个 POD，或明确要求全局汇总")
		return true
	}
	if scope.subresource {
		fail(w, http.StatusBadRequest, "assistant_scope_unsupported", "暂不能按单个节点生成建议；请先通过节点详情核对该对象")
		return true
	}
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok || bridge == nil || s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "assistant_unavailable", "本地节点查询暂不可用")
		return true
	}
	page, raw, ok := assistantInventoryPage(r, bridge, url.Values{"limit": {"24"}})
	if !ok {
		fail(w, http.StatusServiceUnavailable, "assistant_inventory_unavailable", "无法读取已授权节点记录")
		return true
	}
	evidence := assistantOperationEvidence{Scope: "global", TotalNodes: page.Summary.TotalNodes, Health: page.Summary.Health, ComputedAt: page.Summary.ComputedAt}
	podRef := ""
	if !scope.global {
		pod, found := assistantFindPOD(page.Summary, scope.pod)
		if !found {
			fail(w, http.StatusNotFound, "assistant_scope_unavailable", "该 POD 不存在、没有可确认的可见节点，或当前无权查看")
			return true
		}
		podRef = pod.ObjectRef
		evidence = assistantOperationEvidence{Scope: "pod", Pod: &assistantOperationPod{pod.ObjectRef, pod.Identity}, TotalNodes: pod.TotalNodes, Health: pod.Health, ComputedAt: page.Summary.ComputedAt}
	}
	lower := strings.ToLower(text)
	items := []json.RawMessage{assistantEvidenceReceipt(evidence)}
	if scope.global || strings.Contains(lower, "集群") || strings.Contains(lower, "全局") {
		items = append(items, assistantInventoryReceipt(raw))
	}
	if strings.Contains(lower, "token") || strings.Contains(lower, "令牌用量") {
		items = append(items, s.invokeAssistantBatchItem(r, json.RawMessage(`{"tool":"get_token_usage_stats"}`), false))
	}
	if strings.Contains(lower, "告警") || strings.Contains(lower, "运维阶段") || strings.Contains(lower, "处置阶段") {
		items = append(items, assistantOperationFailure("get_incident_workflow", "未指定可核验的事件，告警和运维阶段尚未核实；节点健康记录不能代替事件依据。"))
	}
	if task := s.assistantOperationTask(r, scope, text); len(task) != 0 {
		items = append(items, task)
	}
	if evidence.TotalNodes == 0 {
		assistantOperationBatch(w, assistantOperationWithSuggestion(items, assistantOperationFailure(assistantRecommendationTool, "当前范围没有可见节点记录，未生成处置建议。")))
		return true
	}
	refs, complete := assistantOperationMembers(r, bridge, podRef, evidence)
	if !complete {
		assistantOperationBatch(w, assistantOperationWithSuggestion(items, assistantOperationFailure(assistantRecommendationTool, "节点记录已变化、不可完整读取或范围过大，未向模型发送数据；请重新查询。")))
		return true
	}
	modelEvidence := model.AssistantOperationEvidence{Scope: evidence.Scope, Focus: scope.focus, TotalNodes: evidence.TotalNodes, Health: evidence.Health, ComputedAt: evidence.ComputedAt}
	result, err := s.Settings.RecommendAssistantOperation(r.Context(), modelEvidence, func(ctx context.Context) error {
		allowed, err := s.Store.AuthorizeAssistantInventoryExport(ctx, s.principal(r), refs, time.Now())
		if err != nil {
			return errAssistantEgressCheck
		}
		if !allowed {
			return errAssistantEgressDenied
		}
		return nil
	})
	if err != nil {
		message := "模型不可用或当前模型角色不允许处理已授权的只读结果，未生成建议。"
		switch {
		case errors.Is(err, errAssistantEgressDenied):
			message = "当前节点数据没有完整的模型外发授权，未发送数据，也未生成建议。"
		case errors.Is(err, errAssistantEgressCheck):
			message = "无法核对节点数据的模型外发授权，未发送数据，也未生成建议。"
		}
		assistantOperationBatch(w, assistantOperationWithSuggestion(items, assistantOperationFailure(assistantRecommendationTool, message)))
		return true
	}
	draft, valid := decodeAssistantRecommendation(result)
	input, inputValid := decodeAssistantToolInput(result)
	if !valid || !inputValid || assistantInputContainsCredential(input) {
		assistantOperationBatch(w, assistantOperationWithSuggestion(items, assistantOperationFailure(assistantRecommendationTool, "模型未返回有效的文字建议，请稍后重试。")))
		return true
	}
	rec := &assistantResponse{headers: make(http.Header)}
	s.issueAssistantRecommendation(rec, r, draft)
	if rec.overflow || rec.status != http.StatusOK {
		items = assistantOperationWithSuggestion(items, assistantOperationFailure(assistantRecommendationTool, "无法保存本次建议，请稍后重试。"))
	} else {
		items = assistantOperationWithSuggestion(items, append(json.RawMessage(nil), rec.Bytes()...))
	}
	assistantOperationBatch(w, items)
	return true
}
