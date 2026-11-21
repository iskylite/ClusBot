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
	mu      sync.Mutex
	pending map[string]assistantPreview
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
	return ok && (account.IsSuperadmin || name == "change_my_password")
}

func (s Server) assistantTools(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, s.assistantToolViews(r))
}

func (s Server) assistantInvoke(w http.ResponseWriter, r *http.Request) {
	var input assistantToolInput
	if decode(w, r, &input) {
		s.invokeAssistantTool(w, r, input)
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
	if readOnlyAssistant(r) {
		fail(w, 400, "invalid_read_only_query", "此问法需要模型理解。请通过 /assistant 查看只读工具，或提交明确的工具名称和参数")
		return
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "密码") || strings.Contains(lower, "密钥") ||
		strings.Contains(lower, "api key") || strings.Contains(lower, "api_key") || strings.Contains(lower, "凭据") {
		form := "configure_model_provider"
		switch {
		case strings.Contains(lower, "重置密码"):
			form = "manage_accounts"
		case strings.Contains(lower, "密码"):
			form = "change_my_password"
		case strings.Contains(lower, "mcp"):
			form = "configure_mcp_connection"
		}
		s.invokeAssistantTool(w, r, assistantToolInput{Tool: form})
		return
	}
	raw, err := s.Settings.InterpretAssistantTool(r.Context(), text, s.assistantToolViews(r))
	var in assistantToolInput
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Tool == "" {
		fail(w, 409, "assistant_intent_unresolved", "无法确定操作，请补充目标或从工具目录选择")
		return
	}
	s.invokeAssistantTool(w, r, in)
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
	var input assistantToolInput
	if strings.HasPrefix(args, "{") {
		d := json.NewDecoder(strings.NewReader(args))
		d.DisallowUnknownFields()
		if !json.Valid([]byte(args)) || d.Decode(&input) != nil {
			fail(w, 400, "invalid_assistant_arguments", "工具参数格式无效")
			return true
		}
	} else {
		s.interpretAssistantCommand(w, r, args)
		return true
	}
	s.invokeAssistantTool(w, r, input)
	return true
}
