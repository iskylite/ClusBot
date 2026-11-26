package httpapi

import (
	"crypto/hmac"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"clusbot/internal/mcpclient"
	"clusbot/internal/work"
)

type mcpSetupDraftForm struct {
	Surface  string           `json:"surface_id"`
	Revision int64            `json:"revision"`
	Proof    string           `json:"form_proof"`
	Request  string           `json:"request_key"`
	Preview  string           `json:"preview_id,omitempty"`
	Name     string           `json:"name"`
	Config   mcpclient.Config `json:"config"`
}

func isMCPSetupDraftRecord(rec mcpSetupRecord, exists bool, x mcpSetupScope, in mcpSetupDraftForm) bool {
	return exists && rec.expires.After(time.Now()) && rec.decision == "open_trusted_form" &&
		rec.fingerprint == x.signer.mac(in.Surface, in.Config.Transport) && len(in.Proof) == 32 &&
		hmac.Equal([]byte(in.Proof), []byte(rec.proof))
}

func (s Server) mcpSetupReadDraft(w http.ResponseWriter, r *http.Request, x mcpSetupScope, in mcpSetupDraftForm) (work.Connection, string, string, bool) {
	// Nothing in a generic A2UI action can turn into a draft. Only the
	// locally opened, session-scoped trusted form arrives here.
	if readOnlyAssistant(r) || !a2uiBrowserKey.MatchString(in.Request) || in.Revision != x.revision ||
		!x.signer.validSurface(in.Surface, x.session, x.conversation, mcpChooseTransport, x.run, x.revision) {
		fail(w, 409, "mcp_form_stale", "设置表单已失效或会话已变化，请重新选择。")
		return work.Connection{}, "", "", false
	}
	key := x.signer.mac("mcp-request", x.session, x.actor, x.conversation, x.run, in.Request)
	x.book.Lock()
	rec, exists := x.book.requests[key]
	x.book.Unlock()
	if !isMCPSetupDraftRecord(rec, exists, x, in) {
		fail(w, 409, "mcp_form_stale", "没有当前连接方式的有效受信表单，请重新选择。")
		return work.Connection{}, "", "", false
	}
	if err := s.mcpRecheck(r.Context(), x); err != nil {
		writeError(w, err)
		return work.Connection{}, "", "", false
	}
	draft, raw, err := work.MCPConnectionDefinition(work.Connection{Name: in.Name, Config: in.Config, Enabled: true})
	if err != nil {
		writeError(w, err)
		return work.Connection{}, "", "", false
	}
	// Bind exact normalized data, including arguments and environment
	// references, without retaining a second copy or exposing it in history.
	seal := x.signer.mac("mcp-connection-draft", key, draft.Name, string(raw))
	return draft, key, seal, true
}

func (s Server) mcpSetupPreflight(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpScope(w, r)
	if !ok {
		return
	}
	var in mcpSetupDraftForm
	if !decode(w, r, &in) {
		return
	}
	if in.Preview != "" {
		fail(w, 400, "mcp_preview_input", "预检只接收待核对的连接配置。")
		return
	}
	draft, key, seal, ok := s.mcpSetupReadDraft(w, r, x, in)
	if !ok {
		return
	}
	if _, err := s.instanceStore.Connection(r.Context(), work.MCPOnboardingConnectionID(x.actor, x.conversation, x.run)); !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			writeError(w, err)
		} else {
			fail(w, 409, "mcp_connection_already_saved", "连接已经登记，请重新读取接入记录。")
		}
		return
	}
	if err := s.instanceStore.CheckNewMCPConnectionName(r.Context(), draft.Name); err != nil {
		writeError(w, err)
		return
	}
	proof := x.signer.mac("mcp-confirm-reviewed-connection", key, seal)
	x.book.Lock()
	rec, exists := x.book.requests[key]
	if !isMCPSetupDraftRecord(rec, exists, x, in) || rec.spent {
		x.book.Unlock()
		fail(w, 409, "mcp_form_stale", "表单已提交或失效，请重新读取接入记录。")
		return
	}
	rec.previewSeal, rec.previewProof = seal, proof
	x.book.requests[key] = rec
	x.book.Unlock()

	target := draft.Config.Endpoint
	refs := 0
	if draft.Config.Transport == "stdio" {
		target = draft.Config.Command
		refs = len(draft.Config.Env)
	} else if draft.Config.TokenEnv != "" {
		refs = 1
	}
	jsonResponse(w, 200, map[string]any{
		"state": "configuration_reviewed", "conversation_id": x.conversation, "run_id": x.run,
		"request_key": in.Request, "revision": x.revision, "preview_id": proof,
		"name": draft.Name, "transport": draft.Config.Transport, "target": target,
		"argument_count": len(draft.Config.Args), "credential_reference_count": refs,
	})
}

func (s Server) mcpSetupSave(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpScope(w, r)
	if !ok {
		return
	}
	var in mcpSetupDraftForm
	if !decode(w, r, &in) {
		return
	}
	draft, key, seal, ok := s.mcpSetupReadDraft(w, r, x, in)
	if !ok {
		return
	}
	x.book.Lock()
	rec, exists := x.book.requests[key]
	if !isMCPSetupDraftRecord(rec, exists, x, in) || len(in.Preview) != 32 || rec.previewSeal != seal ||
		!hmac.Equal([]byte(rec.previewProof), []byte(in.Preview)) {
		x.book.Unlock()
		fail(w, 409, "mcp_preview_required", "配置尚未核对或已更改。请重新预检后再确认登记。")
		return
	}
	// Block replacement of this preview once a registration was attempted.
	// A same-draft retry is safe: the instance uses an idempotent ID qualified
	// by actor, conversation and originating Run, and compares saved data.
	rec.spent = true
	x.book.requests[key] = rec
	x.book.Unlock()
	saved, err := s.instanceStore.CreateMCPOnboardingConnection(r.Context(), x.actor, x.conversation, x.run, draft)
	if err != nil {
		writeError(w, err)
		return
	}
	s.mcpSaved(w, r, x, saved)
}
