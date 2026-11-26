package httpapi

import (
	"crypto/hmac"
	"net/http"
	"strconv"
	"time"

	"clusbot/internal/work"
)

func mcpSetupTarget(x mcpSetupScope, c work.Connection) string {
	return x.run + ":" + strconv.Itoa(c.Revision)
}

type mcpFormTool struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Allowed  bool   `json:"allowed"`
	ReadOnly bool   `json:"read_only"`
	CanRead  bool   `json:"can_read"`
}

func mcpFormTools(c work.Connection, step string) []mcpFormTool {
	out := make([]mcpFormTool, 0)
	for _, t := range c.Tools {
		if t.SchemaError != "" || step == "binding" && (!t.Allowed || !t.ReadOnly) {
			continue
		}
		out = append(out, mcpFormTool{t.Name, t.Version, t.Allowed, t.ReadOnly, work.MCPReadOnlyEligible(t)})
	}
	return out
}
func mcpAllowedStep(c work.Connection, step string) bool {
	if !c.Enabled {
		return false
	}
	switch step {
	case "probe":
		return true
	case "permissions", "binding":
		return c.Status == "ready" && len(mcpFormTools(c, step)) > 0
	}
	return false
}
func (s Server) mcpSaved(w http.ResponseWriter, r *http.Request, x mcpSetupScope, c work.Connection) {
	// SaveConnection returns a summary. Load the recorded catalog, never probing
	// a provider when presenting or restoring a card.
	c, err := s.instanceStore.Connection(r.Context(), c.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	sid, err := x.signer.surface(x.session, x.conversation, mcpSetupStep, mcpSetupTarget(x, c), x.revision)
	if err != nil {
		fail(w, 503, "mcp_setup_unavailable", "无法生成后续接入选择")
		return
	}
	children := []string{"heading"}
	nodes := []map[string]any{nil, a2uiText("heading", "MCP 连接后续操作")}
	for _, v := range []struct{ key, label string }{{"probe", "核对握手与工具目录"}, {"permissions", "审阅实例工具授权"}, {"binding", "选择本会话的只读工具"}} {
		if !mcpAllowedStep(c, v.key) {
			continue
		}
		children = append(children, v.key)
		nodes = append(nodes, a2uiText("label_"+v.key, v.label), a2uiButton(v.key, "label_"+v.key, mcpSetupStep, map[string]any{"step": v.key, "revision": x.revision, "connection_revision": c.Revision}))
	}
	nodes[0] = a2uiColumn(children)
	view := map[string]any{"state": "saved", "conversation_id": x.conversation, "run_id": x.run, "connection_id": c.ID, "revision": c.Revision, "status": c.Status, "surface": a2uiCreateSurface(sid, nodes, nil)}
	bound, err := s.Store.ReadConversationMCPSelection(r.Context(), x.actor, x.conversation)
	if err != nil {
		writeError(w, err)
		return
	}
	if bound.Selection != nil && bound.Selection.ConnectionID == c.ID {
		view["binding_status"] = bound.LocalValidity
		view["bound_tool"] = bound.Selection.ToolName
	}
	jsonResponse(w, 200, view)
}
func (s Server) mcpSetupOperation(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpScope(w, r)
	if !ok {
		return
	}
	var in struct {
		Surface            string `json:"surface_id"`
		Revision           int64  `json:"revision"`
		Proof              string `json:"form_proof"`
		Request            string `json:"request_key"`
		Step               string `json:"step"`
		ConnectionRevision int    `json:"connection_revision"`
		Tool               string `json:"tool"`
		Version            string `json:"tool_version"`
		Mode               string `json:"mode"`
		BindingRevision    *int   `json:"binding_revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.instanceStore.Connection(r.Context(), work.MCPOnboardingConnectionID(x.actor, x.conversation, x.run))
	if err != nil {
		writeError(w, err)
		return
	}
	if readOnlyAssistant(r) || !a2uiBrowserKey.MatchString(in.Request) || in.Revision != x.revision || in.ConnectionRevision != c.Revision || !mcpAllowedStep(c, in.Step) ||
		!x.signer.validSurface(in.Surface, x.session, x.conversation, mcpSetupStep, mcpSetupTarget(x, c), x.revision) {
		fail(w, 409, "mcp_form_stale", "连接状态或会话已变化，请重新打开表单")
		return
	}
	var chosen *mcpFormTool
	if in.Step != "probe" {
		for _, v := range mcpFormTools(c, in.Step) {
			if v.Name == in.Tool && v.Version == in.Version {
				chosen = &v
				break
			}
		}
	}
	if (in.Step == "probe" && (in.Tool != "" || in.Version != "" || in.Mode != "" || in.BindingRevision != nil)) ||
		(in.Step == "permissions" && (chosen == nil || in.BindingRevision != nil || (in.Mode != "revoked" && in.Mode != "controlled" && in.Mode != "read_only") || (in.Mode == "read_only" && !chosen.CanRead))) ||
		(in.Step == "binding" && (chosen == nil || in.BindingRevision == nil || *in.BindingRevision < 0 || in.Mode != "")) {
		fail(w, 400, "mcp_setup_option_invalid", "请选择当前表单允许的工具、权限和绑定版本。")
		return
	}
	if err := s.mcpRecheck(r.Context(), x); err != nil {
		writeError(w, err)
		return
	}
	key := x.signer.mac("mcp-request", x.session, x.actor, x.conversation, x.run, in.Request)
	x.book.Lock()
	rec, ok := x.book.requests[key]
	if !ok || rec.spent || !rec.expires.After(time.Now()) || rec.decision != "open_trusted_form" || !hmac.Equal([]byte(rec.proof), []byte(in.Proof)) || rec.fingerprint != x.signer.mac(in.Surface, in.Step) {
		x.book.Unlock()
		fail(w, 409, "mcp_form_stale", "此表单未签发或已提交，请核对实际结果。")
		return
	}
	rec.spent = true
	x.book.requests[key] = rec
	x.book.Unlock()
	switch in.Step {
	case "probe":
		_, err = s.instanceStore.TestConnection(r.Context(), c.ID, c.Revision)
	case "permissions":
		err = s.instanceStore.SetToolPermissionWithMode(r.Context(), c.ID, in.Tool, in.Version, c.Revision, in.Mode != "revoked", in.Mode == "read_only")
	case "binding":
		_, err = s.Store.SetConversationMCPSelection(r.Context(), x.actor, x.conversation, work.ConversationMCPInput{Action: "bind_mcp_read", ExpectedBindingRevision: in.BindingRevision,
			MCPReadSelection: work.MCPReadSelection{ConnectionID: c.ID, Revision: c.Revision, ToolName: in.Tool, ToolVersion: in.Version}})
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.mcpSaved(w, r, x, c)
}
