package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"clusbot/internal/work"
)

const mcpChooseTransport = "choose_mcp_transport"
const mcpSetupStep = "request_mcp_setup_step"

type mcpSetupKey struct{}
type mcpSetupRecord struct {
	fingerprint, decision, proof string
	previewSeal, previewProof    string
	spent                        bool
	expires                      time.Time
}

type mcpSetupRequests struct {
	sync.Mutex
	requests map[string]mcpSetupRecord
}

func mcpSetupIntent(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	explicit := strings.HasPrefix(t, "/assistant ") || strings.HasPrefix(t, "/助手 ")
	if strings.HasPrefix(t, "/assistant ") {
		t = strings.TrimSpace(strings.TrimPrefix(t, "/assistant "))
	}
	if strings.HasPrefix(t, "/助手 ") {
		t = strings.TrimSpace(strings.TrimPrefix(t, "/助手 "))
	}
	if explicit && strings.HasPrefix(t, "{") {
		var call struct {
			Tool string `json:"tool"`
		}
		return a2uiStrictObject([]byte(t), "tool") && json.Unmarshal([]byte(t), &call) == nil && call.Tool == "configure_mcp_connection"
	}
	if t == "/mcp" || t == "/mcp connect" || t == "configure_mcp_connection" {
		return true
	}
	if !strings.Contains(t, "mcp") || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "/") {
		return false
	}
	if setupContainsAny(t, "不要", "不用", "不想", "无需", "不需要", "暂不", "已接入", "已添加", "已注册", "如何", "怎么", "为什么", "what is", "how to", "do not", "don't") {
		return false
	}
	for _, prefix := range []string{"列出", "查看", "查询", "请列出", "请查看", "请查询", "帮我列出", "帮我查看", "帮我查询"} {
		if strings.HasPrefix(t, prefix) {
			return false
		}
	}
	return setupContainsAny(t, "接入", "添加", "导入", "注册", "新建", "新增", "连接一个", "配置连接", "配置mcp", "配置 mcp", "添加连接", "add ", "connect ", "set up ", "configure ")
}

// A settings question with inline configuration must not fall through to the
// general model, even if it was phrased as a question or carried attachments.
func mcpSetupPayload(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "mcp") && (assistantCredentialMention.MatchString(t) || assistantCredentialValue.MatchString(t) || setupContainsAny(t, "https://", "http://", "stdio", "--", "token=", "bearer ", "api_key", "password=", "\n/", " /", "命令：", "命令:", "command\":", "endpoint\":", "密码：", "密钥："))
}

func (s Server) submitMCPSetup(w http.ResponseWriter, r *http.Request, in submission, key string, task bool) bool {
	if !mcpSetupIntent(in.Text) {
		if mcpSetupPayload(in.Text) {
			fail(w, 400, "mcp_settings_private", "请仅发送「接入 MCP」，地址、凭据和命令只在会话内的受信表单填写。")
			return true
		}
		return false
	}
	if task || in.TaskRef != nil || len(in.AttachmentIDs) > 0 || len(in.Reads) > 0 || in.Read != nil || in.Criteria != nil || in.Filename != nil || in.ReplaceMaterials || in.Revision != 0 || in.BenchmarkExecutionTarget != nil || in.BenchmarkRevisionTarget != nil || in.InterpretBenchmarkExecution || readOnlyAssistant(r) {
		fail(w, 400, "mcp_setup_context", "请在普通对话中单独接入 MCP，不附带任务、材料或只读模式。")
		return true
	}
	if !s.assistantFormAllowed(r, "configure_mcp_connection") {
		fail(w, 403, "forbidden", "仅管理员能添加实例 MCP 连接。")
		return true
	}
	run, err := s.Store.OpenMCPOnboarding(r.Context(), s.principal(r), r.PathValue("id"), key)
	if err != nil {
		writeError(w, err)
		return true
	}
	writeSubmission(w, run)
	return true
}

type mcpSetupScope struct {
	actor, session, conversation, run string
	revision                          int64
	signer                            *discoveryA2UIRuntime
	book                              *mcpSetupRequests
}

func (s Server) mcpScope(w http.ResponseWriter, r *http.Request) (mcpSetupScope, bool) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := accountFromContext(r)
	if !ok || !a.IsSuperadmin || a.MustChangePassword {
		fail(w, 403, "forbidden", "仅管理员可接入 MCP。")
		return mcpSetupScope{}, false
	}
	x := mcpSetupScope{actor: a.PrincipalID, session: s.assistantSession(r), conversation: r.PathValue("id"), run: r.PathValue("run")}
	x.signer, _ = r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	x.book, _ = r.Context().Value(mcpSetupKey{}).(*mcpSetupRequests)
	if x.signer == nil || !x.signer.ready || x.book == nil || s.instanceStore == nil || x.session == "" {
		fail(w, 503, "mcp_setup_unavailable", "接入会话暂不可用")
		return x, false
	}
	if !validConversationID(x.conversation) || !validID(x.run) {
		fail(w, 404, "mcp_setup_unavailable", "没有此接入会话")
		return x, false
	}
	rev, err := s.Store.MCPOnboardingRevision(r.Context(), x.actor, x.conversation, x.run)
	if err != nil {
		writeError(w, err)
		return x, false
	}
	x.revision = rev
	return x, true
}
func (s Server) mcpRecheck(ctx context.Context, x mcpSetupScope) error {
	if err := s.verifyDiscoveryA2UISession(ctx, x.session, x.actor); err != nil {
		return err
	}
	rev, err := s.Store.MCPOnboardingRevision(ctx, x.actor, x.conversation, x.run)
	if err != nil {
		return err
	}
	if rev != x.revision {
		return work.ErrConversationRevision
	}
	return nil
}
func (s Server) mcpSetupView(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpScope(w, r)
	if !ok {
		return
	}
	connection, err := s.instanceStore.Connection(r.Context(), work.MCPOnboardingConnectionID(x.actor, x.conversation, x.run))
	if err == nil {
		s.mcpSaved(w, r, x, connection)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	surface, err := x.signer.surface(x.session, x.conversation, mcpChooseTransport, x.run, x.revision)
	if err != nil {
		fail(w, 503, "mcp_setup_unavailable", "接入选择暂不可用")
		return
	}
	nodes := []map[string]any{a2uiColumn([]string{"heading", "stdio", "http"}), a2uiText("heading", "选择连接方式")}
	for _, t := range []string{"stdio", "http"} {
		label := "受控本地程序（stdio）"
		if t == "http" {
			label = "远程服务（Streamable HTTP）"
		}
		nodes = append(nodes, a2uiText("label_"+t, label), a2uiButton(t, "label_"+t, mcpChooseTransport, map[string]any{"transport": t, "revision": x.revision}))
	}
	jsonResponse(w, 200, map[string]any{"state": "choose_transport", "conversation_id": x.conversation, "run_id": x.run, "surface": a2uiCreateSurface(surface, nodes, nil)})
}

func (s Server) mcpSetupAction(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpScope(w, r)
	if !ok {
		return
	}
	req := r.Header.Get("X-ClusBot-Request-Key")
	if readOnlyAssistant(r) || len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(req) {
		fail(w, 400, "mcp_setup_request", "需要有效的模型交互请求标识")
		return
	}
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var msg struct {
		Version string          `json:"version"`
		Action  json.RawMessage `json:"action"`
	}
	var a a2uiDiscoveryAction
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") || json.Unmarshal(msg.Action, &a) != nil ||
		!validA2UIUserMessage(a.UserMessage) || !validA2UIMetadata(a.Metadata) {
		fail(w, 400, "invalid_mcp_setup_action", "仅接受本卡限定的接入选项。")
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, a.Timestamp); err != nil {
		fail(w, 400, "invalid_mcp_setup_action", "交互时间无效")
		return
	}
	choice, target, revision := "", "", int64(0)
	var connection *work.Connection
	switch a.Name {
	case mcpChooseTransport:
		var in struct {
			Transport string `json:"transport"`
			Revision  int64  `json:"revision"`
		}
		if !a2uiStrictObject(a.Context, "transport", "revision") || json.Unmarshal(a.Context, &in) != nil || (in.Transport != "stdio" && in.Transport != "http") {
			fail(w, 400, "invalid_mcp_setup_action", "连接方式无效。")
			return
		}
		if _, err := s.instanceStore.Connection(r.Context(), work.MCPOnboardingConnectionID(x.actor, x.conversation, x.run)); !errors.Is(err, sql.ErrNoRows) {
			if err != nil {
				writeError(w, err)
			} else {
				fail(w, 409, "mcp_surface_stale", "连接已经登记，请读取当前接入状态。")
			}
			return
		}
		choice, target, revision = in.Transport, x.run, in.Revision
	case mcpSetupStep:
		var in struct {
			Step               string `json:"step"`
			Revision           int64  `json:"revision"`
			ConnectionRevision int    `json:"connection_revision"`
		}
		if !a2uiStrictObject(a.Context, "step", "revision", "connection_revision") || json.Unmarshal(a.Context, &in) != nil {
			fail(w, 400, "invalid_mcp_setup_action", "接入步骤无效。")
			return
		}
		c, err := s.instanceStore.Connection(r.Context(), work.MCPOnboardingConnectionID(x.actor, x.conversation, x.run))
		if err != nil || c.Revision != in.ConnectionRevision || !mcpAllowedStep(c, in.Step) {
			fail(w, 409, "mcp_surface_stale", "连接或权限已变化，请重新核对。")
			return
		}
		connection = &c
		choice, target, revision = in.Step, mcpSetupTarget(x, c), in.Revision
	default:
		fail(w, 400, "invalid_mcp_setup_action", "不支持此接入行为。")
		return
	}
	if a.SourceComponentID != choice || revision != x.revision || !x.signer.validSurface(a.SurfaceID, x.session, x.conversation, a.Name, target, revision) {
		fail(w, 409, "mcp_surface_stale", "此选择已过期或不属于本会话，请重新打开。")
		return
	}
	recheck := func(ctx context.Context) error {
		if err := s.mcpRecheck(ctx, x); err != nil {
			return err
		}
		if connection != nil {
			fresh, err := s.instanceStore.Connection(ctx, connection.ID)
			if err != nil {
				return err
			}
			if fresh.Revision != connection.Revision || !mcpAllowedStep(fresh, choice) {
				return work.ErrConnectionRevision
			}
		}
		return nil
	}
	// Request keys are scoped to this actor/session/turn. Different component
	// re-use is rejected; retrying an acknowledged selection never calls the model twice.
	key := x.signer.mac("mcp-request", x.session, x.actor, x.conversation, x.run, req)
	fingerprint := x.signer.mac(a.SurfaceID, choice)
	proof := x.signer.mac("mcp-form", key, fingerprint)
	x.book.Lock()
	for k, v := range x.book.requests {
		if !v.expires.After(time.Now()) {
			delete(x.book.requests, k)
		}
	}
	prior, exists := x.book.requests[key]
	if exists {
		x.book.Unlock()
		if prior.fingerprint != fingerprint || prior.decision == "" {
			fail(w, 409, "mcp_action_pending", "原请求尚未完成或与本次选择不一致")
			return
		}
		s.writeMCPDecision(w, r, x, a.Name, choice, req, prior, connection)
		return
	}
	if len(x.book.requests) >= 256 {
		x.book.Unlock()
		fail(w, 429, "mcp_setup_busy", "接入请求繁忙，请稍后重试")
		return
	}
	record := mcpSetupRecord{fingerprint: fingerprint, proof: proof, expires: time.Now().Add(a2uiSurfaceTTL)}
	x.book.requests[key] = record
	x.book.Unlock()
	abort := func() { x.book.Lock(); delete(x.book.requests, key); x.book.Unlock() }
	select {
	case x.signer.modelGate <- struct{}{}:
		defer func() { <-x.signer.modelGate }()
	default:
		abort()
		fail(w, 429, "mcp_setup_busy", "助手繁忙，请稍后重试")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
	release, err := s.Store.AcquireDiscoveryA2UIModelSlot(wait)
	stop()
	if err != nil {
		abort()
		fail(w, 429, "mcp_setup_busy", "助手繁忙，请稍后重试")
		return
	}
	defer release()
	output, err := s.Settings.MCPOnboardingDecision(r.Context(), a.Name, choice, recheck)
	if err != nil {
		abort()
		fail(w, 503, "mcp_setup_model_unavailable", "助手未能作出接入决定，未创建连接。")
		return
	}
	var d struct {
		Next string `json:"next"`
	}
	if len(output) > 512 || !a2uiStrictObject(output, "next") || json.Unmarshal(output, &d) != nil || (d.Next != "open_trusted_form" && d.Next != "ask_for_details" && d.Next != "cannot_prepare") {
		abort()
		fail(w, 502, "mcp_setup_invalid_model", "助手未返回有效的接入决定；未创建连接。")
		return
	}
	if err := recheck(r.Context()); err != nil {
		abort()
		writeError(w, err)
		return
	}
	record.decision = d.Next
	x.book.Lock()
	x.book.requests[key] = record
	x.book.Unlock()
	s.writeMCPDecision(w, r, x, a.Name, choice, req, record, connection)
}
func (s Server) writeMCPDecision(w http.ResponseWriter, req *http.Request, x mcpSetupScope, action, choice, request string, rec mcpSetupRecord, c *work.Connection) {
	view := map[string]any{"state": rec.decision, "conversation_id": x.conversation, "run_id": x.run}
	if action == mcpChooseTransport {
		view["transport"] = choice
	} else {
		view["step"] = choice
	}
	if rec.decision == "open_trusted_form" {
		view["form_proof"] = rec.proof
		view["request_key"] = request
		view["revision"] = x.revision
		if c != nil {
			view["connection_revision"] = c.Revision
			if choice == "probe" {
				target := c.Config.Endpoint
				if c.Config.Transport == "stdio" {
					target = c.Config.Command
				}
				// Only the trusted admin confirmation sees the exact destination;
				// never put command arguments or credential names/values in this receipt.
				view["probe_target"] = map[string]any{"name": c.Name, "transport": c.Config.Transport, "target": target, "argument_count": len(c.Config.Args)}
			}
			if choice == "permissions" || choice == "binding" {
				view["tools"] = mcpFormTools(*c, choice)
				if choice == "binding" {
					v, err := s.Store.ReadConversationMCPSelection(req.Context(), x.actor, x.conversation)
					if err != nil {
						writeError(w, err)
						return
					}
					view["binding_revision"] = v.BindingRevision
					if v.Selection != nil {
						view["current_tool"] = v.Selection.ToolName
					}
				}
			}
		}
	}
	jsonResponse(w, 200, view)
}
