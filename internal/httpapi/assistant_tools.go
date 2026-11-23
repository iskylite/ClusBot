package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type assistantTool struct {
	Name, Description, Method, Path string
	Confirm                         bool
}

type assistantToolInput struct {
	Tool   string            `json:"tool"`
	Params map[string]string `json:"params,omitempty"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

type assistantPreview struct {
	session      string
	method, path string
	body         json.RawMessage
	tool         string
	expires      time.Time
}

type assistantBridge struct {
	http.Handler
	mu              sync.Mutex
	pending         map[string]assistantPreview
	recommendations map[string]assistantRecommendation
}

type assistantBridgeKey struct{}

func (s Server) registerAssistantTools(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/assistant/tools", s.assistantTools)
	api.HandleFunc("GET /api/v1/assistant/model-routing", s.assistantModelRouting)
	api.HandleFunc("GET /api/v1/assistant/connections", s.assistantConnections)
	api.HandleFunc("GET /api/v1/assistant/models", s.assistantModels)
	api.HandleFunc("GET /api/v1/assistant/composer-options", s.composerOptions)
	api.HandleFunc("GET /api/v1/assistant/token-usage-stats", s.tokenUsageStats)
	api.HandleFunc("GET /api/v1/assistant/runs/{id}/model-receipt", s.assistantRunModelReceipt)
	api.HandleFunc("POST /api/v1/assistant/interpret", s.assistantInterpret)
	api.HandleFunc("POST /api/v1/assistant/tools", s.assistantInvoke)
	api.HandleFunc("POST /api/v1/assistant/confirm", s.assistantConfirm)
	api.HandleFunc("DELETE /api/v1/assistant/confirm/{id}", s.assistantDiscard)
	api.HandleFunc("POST /api/v1/assistant/recommendations/{id}/selection", s.assistantRecommendationSelection)
}

func registeredAssistantTools() []assistantTool {
	return slices.Concat(assistantSettingTools(), assistantTaskTools(), assistantDomainTools())
}

func (s Server) assistantAllowed(r *http.Request, tool assistantTool) bool {
	if readOnlyAssistant(r) && (tool.Method != http.MethodGet || tool.Confirm) {
		return false
	}
	if !s.ServerMode {
		return !strings.HasPrefix(tool.Path, "/api/v1/admin/") && !strings.HasPrefix(tool.Path, "/api/v1/me/")
	}
	account, ok := accountFromContext(r)
	if !ok || account.MustChangePassword {
		return false
	}
	if strings.HasPrefix(tool.Path, "/api/v1/domain/") && tool.Path != "/api/v1/domain/discovery/setup-status" {
		allowed := accountPersonalRoute(tool.Method + " " + tool.Path)
		return allowed && (!strings.HasPrefix(tool.Path, "/api/v1/domain/discovery/") || (account.IsSuperadmin && s.DiscoverySource != nil))
	}
	if strings.HasPrefix(tool.Path, "/api/v1/loans") || tool.Path == "/api/v1/settings/data/storage" {
		return false
	}
	admin := strings.HasPrefix(tool.Path, "/api/v1/admin/") || strings.HasPrefix(tool.Path, "/api/v1/settings") || tool.Path == "/api/v1/assistant/models" ||
		(tool.Method != "GET" && (strings.HasPrefix(tool.Path, "/api/v1/skills") || strings.HasPrefix(tool.Path, "/api/v1/connections")))
	return !admin || account.IsSuperadmin
}

type assistantToolView struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Params      []string `json:"params,omitempty"`
}

var assistantPathParameter = regexp.MustCompile(`\{([a-z_]+)\}`)

func (s Server) assistantToolViews(r *http.Request) []assistantToolView {
	out := []assistantToolView{}
	for _, tool := range registeredAssistantTools() {
		if !s.assistantAllowed(r, tool) {
			continue
		}
		item := assistantToolView{Name: tool.Name, Label: assistantToolLabel(tool.Name), Description: tool.Description, Mode: "read"}
		if tool.Confirm {
			item.Mode = "confirm"
		}
		for _, p := range assistantPathParameter.FindAllStringSubmatch(tool.Path, -1) {
			item.Params = append(item.Params, p[1])
		}
		out = append(out, item)
	}
	for name := range assistantSettingForms() {
		if s.assistantFormAllowed(r, name) {
			out = append(out, assistantToolView{Name: name, Label: assistantToolLabel(name), Description: "打开可信设置表单", Mode: "form"})
		}
	}
	slices.SortFunc(out, func(a, b assistantToolView) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s Server) assistantFormAllowed(r *http.Request, name string) bool {
	if readOnlyAssistant(r) {
		return false
	}
	if !s.ServerMode {
		return name != "change_my_password" && name != "manage_accounts"
	}
	account, ok := accountFromContext(r)
	return ok && (account.IsSuperadmin || name == "change_my_password" || name == "configure_conversation")
}

func (s Server) assistantTools(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, s.assistantToolViews(r))
}

// The recommendation tool is offered to the model only. It does not name or
// grant access to an operation, provider, route, script or target.
func (s Server) assistantModelToolViews(r *http.Request) []assistantToolView {
	return append(s.assistantToolViews(r), assistantToolView{
		Name: assistantRecommendationTool, Label: "提出候选方案", Mode: "proposal",
		Description: "仅根据请求提出 1 至 4 个供用户选择的文字建议，不代表实际能力、授权或执行。参数放在 body：title、可选 summary、options（每项 label、可选 detail 和 recommended）。",
	})
}

func (s Server) assistantInvoke(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if decode(w, r, &raw) {
		if !s.invokeAssistantRequest(w, r, raw, false) {
			fail(w, 400, "invalid_assistant_arguments", "工具参数格式无效，批量调用需要 2 至 6 项")
		}
	}
}

// A batch is a bounded collection of independent existing tool invocations.
// Validate its envelope before invoking anything, then keep each item's
// authorization, confirmation and result separate.
func (s Server) invokeAssistantRequest(w http.ResponseWriter, r *http.Request, raw json.RawMessage, modelProposal bool) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	if calls, batch := fields["calls"]; batch {
		var items []json.RawMessage
		if len(fields) != 1 || json.Unmarshal(calls, &items) != nil || len(items) < 2 || len(items) > 6 {
			return false
		}
		results := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			results = append(results, s.invokeAssistantBatchItem(r, item, modelProposal))
		}
		jsonResponse(w, http.StatusOK, struct {
			State string            `json:"state"`
			Items []json.RawMessage `json:"items"`
		}{"batch_completed", results})
		return true
	}
	input, ok := decodeAssistantToolInput(raw)
	if ok && input.Tool == assistantRecommendationTool && modelProposal && !readOnlyAssistant(r) {
		draft, valid := decodeAssistantRecommendation(raw)
		if !valid || assistantInputContainsCredential(input) {
			return false
		}
		s.issueAssistantRecommendation(w, r, draft)
		return true
	}
	if !ok || (modelProposal && (!assistantKnownTool(input.Tool) || assistantInputContainsCredential(input))) {
		return false
	}
	s.invokeAssistantTool(w, r, input)
	return true
}

func decodeAssistantToolInput(raw json.RawMessage) (assistantToolInput, bool) {
	var input assistantToolInput
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return input, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil {
		return input, false
	}
	return input, true
}

func assistantKnownTool(name string) bool {
	if _, ok := assistantSettingForms()[name]; ok {
		return true
	}
	for _, tool := range registeredAssistantTools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

var assistantCredentialValue = regexp.MustCompile(`(?i)(?:\b(?:sk|rk)-[a-z0-9_-]{10,}|\bbearer\s+[^\s]{8,}|-----begin [a-z ]*private key-----|(?:api[ _-]?key|password|passwd|secret|access[ _-]?token|密码|密钥|凭据)\s*[:=：]\s*[^\s]+)`)
var assistantCredentialMention = regexp.MustCompile(`(?i)(?:password|passwd|secret|credential|api[ _-]?key|(?:private|access|signing)[ _-]?key|(?:access|refresh)[ _-]?token|密码|密钥|凭据)`)

// Credentials belong in the trusted settings forms, not a batch receipt or
// model proposal. Do not echo rejected field names or values in failures.
func assistantInputContainsCredential(in assistantToolInput) bool {
	data, err := json.Marshal(in)
	if err != nil {
		return true
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return true
	}
	pending := []any{value}
	for len(pending) != 0 {
		v := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch v := v.(type) {
		case map[string]any:
			for key, entry := range v {
				normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
				if strings.Contains(normalized, "password") || strings.Contains(normalized, "passwd") ||
					strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential") ||
					strings.Contains(normalized, "token") || strings.Contains(normalized, "apikey") ||
					strings.Contains(normalized, "privatekey") || strings.Contains(normalized, "accesskey") ||
					strings.Contains(normalized, "signingkey") || strings.Contains(normalized, "authorization") ||
					strings.Contains(key, "密码") || strings.Contains(key, "密钥") || strings.Contains(key, "凭据") {
					return true
				}
				pending = append(pending, entry)
			}
		case []any:
			pending = append(pending, v...)
		case string:
			if assistantCredentialValue.MatchString(v) {
				return true
			}
		}
	}
	return false
}

func (s Server) invokeAssistantBatchItem(r *http.Request, raw json.RawMessage, modelProposal bool) json.RawMessage {
	input, ok := decodeAssistantToolInput(raw)
	tool := "unknown"
	if assistantKnownTool(input.Tool) || (modelProposal && input.Tool == assistantRecommendationTool) {
		tool = input.Tool
	}
	failure := func(message string) json.RawMessage {
		data, _ := json.Marshal(map[string]string{"state": "failed", "tool": tool, "message": message})
		return data
	}
	if !ok || input.Tool == "" {
		return failure("工具参数格式无效")
	}
	if assistantInputContainsCredential(input) {
		return failure("请在可信设置表单中处理凭据")
	}
	if input.Tool == assistantRecommendationTool && modelProposal && !readOnlyAssistant(r) {
		draft, valid := decodeAssistantRecommendation(raw)
		if !valid {
			return failure("建议格式无效，请重新提出")
		}
		rec := &assistantResponse{headers: make(http.Header)}
		s.issueAssistantRecommendation(rec, r, draft)
		if rec.status >= 300 || rec.overflow {
			return failure(assistantBatchFailureMessage(rec.status, rec.Bytes()))
		}
		return append(json.RawMessage(nil), rec.Bytes()...)
	}
	if _, isForm := assistantSettingForms()[input.Tool]; isForm && (len(input.Params) != 0 || len(input.Query) != 0 || len(input.Body) != 0) {
		return failure("设置表单不接受对话参数，请打开表单填写")
	}
	rec := &assistantResponse{headers: make(http.Header)}
	s.invokeAssistantTool(rec, r, input)
	if rec.overflow {
		return failure("结果过大，请使用详情入口")
	}
	if rec.status >= 300 {
		return failure(assistantBatchFailureMessage(rec.status, rec.Bytes()))
	}
	var result struct {
		State string `json:"state"`
	}
	if json.Unmarshal(rec.Bytes(), &result) != nil ||
		(result.State != "completed" && result.State != "form_required" && result.State != "confirmation_required") {
		return failure("无法读取工具结果，请重试")
	}
	return append(json.RawMessage(nil), rec.Bytes()...)
}

func assistantBatchFailureMessage(status int, body []byte) string {
	var result struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &result)
	switch result.Code {
	case "assistant_tool_unavailable":
		return "工具不可用或无访问权限"
	case "invalid_assistant_arguments":
		return "请提供有效的对象和参数"
	case "assistant_target_unavailable":
		return "无法读取目标任务"
	case "assistant_preview_busy":
		return "待确认操作过多，请稍后再试"
	case "assistant_result_too_large":
		return "结果过大，请使用详情入口"
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "请提供有效的对象和参数"
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return "工具或目标不可用，或无访问权限"
	case http.StatusConflict:
		return "状态已变化，请重新查询后再试"
	case http.StatusRequestEntityTooLarge:
		return "结果过大，请使用详情入口"
	case http.StatusTooManyRequests:
		return "请求过多，请稍后再试"
	default:
		return "工具暂时不可用，请稍后再试"
	}
}

func (s Server) invokeAssistantTool(w http.ResponseWriter, r *http.Request, in assistantToolInput) {
	if form, ok := assistantSettingForms()[in.Tool]; ok && s.assistantFormAllowed(r, in.Tool) {
		jsonResponse(w, 200, map[string]string{"state": "form_required", "tool": in.Tool, "form": form})
		return
	}
	var selected assistantTool
	for _, tool := range registeredAssistantTools() {
		if tool.Name == in.Tool && s.assistantAllowed(r, tool) {
			selected = tool
			break
		}
	}
	if selected.Name == "" {
		fail(w, 404, "assistant_tool_unavailable", "工具不可用或无访问权限")
		return
	}
	path, ok := assistantToolPath(selected.Path, in.Params, in.Query)
	if !ok || (selected.Method == http.MethodGet && len(in.Body) != 0) {
		fail(w, 400, "invalid_assistant_arguments", "请提供有效的对象和参数")
		return
	}
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok {
		fail(w, 503, "assistant_unavailable", "对话工具暂不可用")
		return
	}
	p := assistantPreview{session: s.assistantSession(r), method: selected.Method, path: path, body: in.Body, tool: in.Tool}
	if selected.Method != http.MethodGet || selected.Confirm {
		if !bytes.HasPrefix(bytes.TrimSpace(in.Body), []byte("{")) || p.session == "" {
			fail(w, 400, "invalid_assistant_arguments", "操作需要有效参数")
			return
		}
		if key := assistantRequestKeyField(selected.Name); key != "" {
			var body map[string]json.RawMessage
			if json.Unmarshal(p.body, &body) != nil || body == nil {
				fail(w, 400, "invalid_assistant_arguments", "操作需要有效参数")
				return
			}
			if _, ok := body[key]; !ok {
				var nonce [16]byte
				if _, err := rand.Read(nonce[:]); err != nil {
					fail(w, 503, "assistant_unavailable", "无法生成操作标识")
					return
				}
				body[key], _ = json.Marshal("assistant-" + hex.EncodeToString(nonce[:]))
				p.body, _ = json.Marshal(body)
				in.Body = p.body
			}
		}
		var target json.RawMessage
		if strings.HasPrefix(selected.Path, "/api/v1/admin/tasks/{owner}/{id}") {
			detail, ok := assistantToolPath("/api/v1/admin/tasks/{owner}/{id}", map[string]string{
				"owner": in.Params["owner"], "id": in.Params["id"],
			}, nil)
			if !ok {
				fail(w, 400, "invalid_assistant_arguments", "请提供任务和所属账号")
				return
			}
			rec, err := bridge.request(r, assistantPreview{method: http.MethodGet, path: detail})
			if err != nil || rec.overflow || rec.status != 200 || !json.Valid(rec.Bytes()) {
				fail(w, 404, "assistant_target_unavailable", "无法读取目标任务")
				return
			}
			target = json.RawMessage(rec.Bytes())
		}
		token, expires, ok := bridge.prepare(p)
		if !ok {
			fail(w, 429, "assistant_preview_busy", "待确认操作过多，请稍后再试")
			return
		}
		jsonResponse(w, 200, struct {
			State   string                 `json:"state"`
			Token   string                 `json:"confirmation_id"`
			Expires int64                  `json:"expires_at_unix_ms"`
			Input   assistantToolInput     `json:"input"`
			Target  json.RawMessage        `json:"target,omitempty"`
			Surface []assistantA2UIMessage `json:"surface,omitempty"`
		}{"confirmation_required", token, expires.UnixMilli(), in, target, assistantPreviewSurface(in.Tool)})
		return
	}
	bridge.execute(w, r, p)
}

func assistantRequestKeyField(tool string) string {
	switch tool {
	case "create_task", "resume_task", "admin_resume_task", "create_workflow", "create_goal",
		"request_task_publication", "query_asset_acceptance", "request_ticket":
		return "client_request_id"
	case "submit_benchmark":
		return "request_key"
	default:
		return ""
	}
}

func assistantToolPath(path string, params, query map[string]string) (string, bool) {
	matches := assistantPathParameter.FindAllStringSubmatch(path, -1)
	if len(matches) != len(params) {
		return "", false
	}
	for _, m := range matches {
		v := params[m[1]]
		if v == "" || len(v) > 256 || v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00\r\n") {
			return "", false
		}
		path = strings.ReplaceAll(path, m[0], url.PathEscape(v))
	}
	values := url.Values{}
	for k, v := range query {
		values.Set(k, v)
	}
	if len(values) != 0 {
		path += "?" + values.Encode()
	}
	return path, true
}

func (s Server) assistantSession(r *http.Request) string {
	if s.ServerMode {
		return s.accountSessionDigest(r)
	}
	if c, err := r.Cookie("clusbot_session"); err == nil {
		return secretDigest(c.Value)
	}
	return ""
}

func (b *assistantBridge) prepare(p assistantPreview) (string, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, old := range b.pending {
		if !now.Before(old.expires) {
			delete(b.pending, k)
		}
	}
	if len(b.pending) >= 128 {
		return "", time.Time{}, false
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", time.Time{}, false
	}
	token := hex.EncodeToString(random[:])
	p.expires = now.Add(5 * time.Minute)
	b.pending[token] = p
	return token, p.expires, true
}

func (s Server) assistantConfirm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"confirmation_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok {
		fail(w, 503, "assistant_unavailable", "对话工具暂不可用")
		return
	}
	p, valid := bridge.take(in.ID, s.assistantSession(r))
	if !valid {
		fail(w, 409, "assistant_confirmation_expired", "确认已失效，请重新预览")
		return
	}
	bridge.execute(w, r, p)
}

func (s Server) assistantDiscard(w http.ResponseWriter, r *http.Request) {
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok {
		fail(w, 503, "assistant_unavailable", "对话工具暂不可用")
		return
	}
	if _, ok := bridge.take(r.PathValue("id"), s.assistantSession(r)); !ok {
		fail(w, 409, "assistant_confirmation_expired", "确认已失效，请重新预览")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *assistantBridge) take(id, session string) (assistantPreview, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, found := b.pending[id]
	if !found || session == "" || p.session != session || !time.Now().Before(p.expires) {
		return assistantPreview{}, false
	}
	delete(b.pending, id)
	return p, true
}

const assistantResultLimit = 512 << 10

type assistantResponse struct {
	headers http.Header
	bytes.Buffer
	status   int
	overflow bool
}

func (w *assistantResponse) Header() http.Header { return w.headers }
func (w *assistantResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *assistantResponse) Write(data []byte) (int, error) {
	w.WriteHeader(200)
	if w.overflow || w.Len()+len(data) > assistantResultLimit {
		w.overflow = true
		return 0, errors.New("assistant result too large")
	}
	return w.Buffer.Write(data)
}

func (b *assistantBridge) request(r *http.Request, p assistantPreview) (*assistantResponse, error) {
	inner, err := http.NewRequestWithContext(context.WithValue(r.Context(), accountContextKey{}, struct{}{}), p.method, p.path, bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}
	inner.Host, inner.RemoteAddr = r.Host, r.RemoteAddr
	inner.Header = r.Header.Clone()
	inner.Header.Set("Content-Type", "application/json")
	rec := &assistantResponse{headers: make(http.Header)}
	b.ServeHTTP(rec, inner)
	return rec, nil
}

func (b *assistantBridge) execute(w http.ResponseWriter, r *http.Request, p assistantPreview) {
	rec, err := b.request(r, p)
	if err != nil {
		fail(w, 400, "invalid_assistant_arguments", "请求参数无效")
		return
	}
	if rec.overflow {
		fail(w, 413, "assistant_result_too_large", "结果过大，请使用详情入口")
		return
	}
	if rec.status == 0 {
		rec.status = 200
	}
	if rec.status >= 300 {
		if json.Valid(rec.Bytes()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.status)
			w.Write(rec.Bytes())
		} else {
			fail(w, rec.status, "assistant_operation_failed", "操作未完成")
		}
		return
	}
	var result any
	if strings.SplitN(p.path, "?", 2)[0] == "/api/v1/domain/snapshot" {
		var assets struct {
			Assets     json.RawMessage `json:"assets"`
			SnapshotAt json.RawMessage `json:"snapshot_at,omitempty"`
		}
		if json.Unmarshal(rec.Bytes(), &assets) != nil {
			fail(w, 502, "invalid_tool_result", "资产查询结果无法读取")
			return
		}
		result = assets
	} else if json.Valid(rec.Bytes()) {
		result = json.RawMessage(rec.Bytes())
	} else if rec.Len() != 0 {
		result = rec.String()
	}
	response := map[string]any{"state": "completed", "tool": p.tool, "result": result}
	if surface := assistantResultSurface(p.tool, p.method, rec.Bytes()); len(surface) != 0 {
		response["surface"] = surface
	}
	jsonResponse(w, 200, response)
}

func assistantReadShortcut(text string) string {
	switch strings.TrimRight(strings.TrimSpace(text), "？?。！!") {
	case "你现在用的哪个模型", "你现在用的什么模型", "查看当前模型", "查看模型绑定", "当前模型是什么":
		return "get_current_model_routing"
	case "查看任务列表", "列出我的任务", "查看我的任务":
		return "list_tasks"
	case "有哪些待处理", "查看待我处理", "查看待处理事项":
		return "list_attention"
	case "查看资产发现", "查看资产发现状态", "查看集群资产发现状态":
		return "get_discovery_setup_status"
	}
	return ""
}

func (s Server) assistantInterpret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Text) > 8000 || strings.TrimSpace(in.Text) == "" {
		fail(w, 400, "invalid_assistant_arguments", "请输入要查询或安排的内容")
		return
	}
	s.interpretAssistantCommand(w, r, in.Text)
}

func readOnlyAssistant(r *http.Request) bool {
	return r.Header.Get("X-ClusBot-Reply-Strategy") == "deterministic_only"
}

func (s Server) interpretAssistantCommand(w http.ResponseWriter, r *http.Request, text string) {
	if name := assistantReadShortcut(text); name != "" {
		s.invokeAssistantTool(w, r, assistantToolInput{Tool: name})
		return
	}
	switch strings.TrimRight(strings.TrimSpace(text), "？?。！!") {
	case "本会话配置", "配置本会话", "会话能力绑定":
		s.invokeAssistantTool(w, r, assistantToolInput{Tool: "configure_conversation"})
		return
	}
	if readOnlyAssistant(r) {
		fail(w, 400, "invalid_read_only_query", "此问法需要模型理解。请通过 /assistant 查看只读工具，或提交明确的工具名称和参数")
		return
	}
	lower := strings.ToLower(text)
	if assistantCredentialValue.MatchString(text) || assistantCredentialMention.MatchString(text) {
		form := "configure_model_provider"
		switch {
		case strings.Contains(lower, "重置密码") || (strings.Contains(lower, "reset") && strings.Contains(lower, "password")):
			form = "manage_accounts"
		case strings.Contains(lower, "密码") || strings.Contains(lower, "password") || strings.Contains(lower, "passwd"):
			form = "change_my_password"
		case strings.Contains(lower, "mcp"):
			form = "configure_mcp_connection"
		}
		s.invokeAssistantTool(w, r, assistantToolInput{Tool: form})
		return
	}
	raw, err := s.Settings.InterpretAssistantTool(r.Context(), text, s.assistantModelToolViews(r))
	if err != nil || !s.invokeAssistantRequest(w, r, raw, true) {
		fail(w, 409, "assistant_intent_unresolved", "无法确定操作，请补充目标或从工具目录选择")
		return
	}
}

func (s Server) assistantConversationCommand(w http.ResponseWriter, r *http.Request, text string) bool {
	if name := assistantReadShortcut(text); name != "" && (readOnlyAssistant(r) || r.Header.Get("X-ClusBot-Assistant-Cards") == "1") {
		s.invokeAssistantTool(w, r, assistantToolInput{Tool: name})
		return true
	}
	command, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	if command != "/助手" && command != "/assistant" {
		return false
	}
	args = strings.TrimSpace(args)
	if args == "" {
		s.assistantTools(w, r)
		return true
	}
	if strings.HasPrefix(args, "{") {
		if !json.Valid([]byte(args)) {
			message := "JSON 格式有误，请检查英文双引号、逗号和括号。草稿已保留。"
			if quote := assistantCurlyJSONQuote(args); quote != "" {
				message = "JSON 中的中文弯引号（" + quote + "）不能用作边界，请将其改为英文双引号（\"）。草稿已保留。"
				if strings.Contains(args, `"calls"`) {
					message += "批量调用需要 2 至 6 项；单次调用请使用 {\"tool\":\"工具名\"}。"
				}
			}
			fail(w, 400, "invalid_assistant_arguments", message)
		} else if !s.invokeAssistantRequest(w, r, json.RawMessage(args), false) {
			message := "工具参数格式有误，请检查 tool、params、query 和 body。草稿已保留。"
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(args), &fields) == nil && fields["calls"] != nil {
				message = "批量调用只接受 calls 列表，列表需要 2 至 6 项；单次调用请使用 {\"tool\":\"工具名\"}。草稿已保留。"
			}
			fail(w, 400, "invalid_assistant_arguments", message)
		}
	} else if !s.assistantOperationAdvice(w, r, args) {
		s.interpretAssistantCommand(w, r, args)
	}
	return true
}

// Detect a likely smart quote used as a JSON delimiter, without changing the
// submitted text or echoing potentially sensitive tool arguments.
func assistantCurlyJSONQuote(raw string) string {
	inString, escaped := false, false
	for i, char := range raw {
		if escaped {
			escaped = false
			continue
		}
		if inString && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			inString = !inString
			continue
		}
		if char != '“' && char != '”' {
			continue
		}
		if !inString {
			return string(char)
		}
		after := strings.TrimLeft(raw[i+len(string(char)):], " \t\r\n")
		if len(after) == 0 || strings.ContainsRune(",}] :", rune(after[0])) {
			return string(char)
		}
	}
	return ""
}
