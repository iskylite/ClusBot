package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"clusbot/internal/mcpclient"
	"clusbot/internal/work"
)

func sharedConnectionRoute(pattern string) bool {
	switch pattern {
	case "GET /api/v1/task-tools",
		"GET /api/v1/connections", "POST /api/v1/connections",
		"GET /api/v1/connections/{id}", "PATCH /api/v1/connections/{id}",
		"POST /api/v1/connections/{id}/test", "POST /api/v1/connections/{id}/permission",
		"GET /api/v1/connections/{id}/calls", "POST /api/v1/connections/{id}/call",
		"GET /api/v1/tool-results/{id}/content":
		return true
	default:
		return false
	}
}

func (s Server) serveSharedConnection(w http.ResponseWriter, r *http.Request, account work.Account, next http.Handler) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Pattern {
	case "POST /api/v1/connections", "PATCH /api/v1/connections/{id}",
		"POST /api/v1/connections/{id}/test", "POST /api/v1/connections/{id}/permission":
		if !account.IsSuperadmin {
			accountError(w, work.ErrAccountDenied)
			return
		}
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountContextKey{}, account)))
}

type sharedConnectionView struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Enabled   bool             `json:"enabled"`
	Revision  int              `json:"revision"`
	Status    string           `json:"status"`
	ToolCount int              `json:"tool_count"`
	Tools     []mcpclient.Tool `json:"tools,omitempty"`
}

func sharedConnection(c work.Connection) sharedConnectionView {
	return sharedConnectionView{ID: c.ID, Name: c.Name, Enabled: c.Enabled, Revision: c.Revision, Status: c.Status, ToolCount: c.ToolCount}
}

func (s Server) taskTools(w http.ResponseWriter, r *http.Request) {
	options, err := s.Store.TaskTools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, options)
}

func (s Server) connections(w http.ResponseWriter, r *http.Request) {
	if account, ok := accountFromContext(r); ok && !account.IsSuperadmin {
		result, err := s.Store.SharedConnections(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		views := make([]sharedConnectionView, 0, len(result))
		for _, c := range result {
			views = append(views, sharedConnection(c))
		}
		jsonResponse(w, 200, views)
		return
	}
	result, err := s.Store.Connections(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) connection(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.Connection(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if account, ok := accountFromContext(r); ok && !account.IsSuperadmin {
		view := sharedConnection(result)
		for _, tool := range result.Tools {
			if tool.Allowed && tool.SchemaError == "" {
				view.Tools = append(view.Tools, tool)
			}
		}
		view.ToolCount = len(view.Tools)
		if !view.Enabled || view.Status != "ready" || view.ToolCount == 0 {
			fail(w, 404, "not_found", "连接不存在")
			return
		}
		jsonResponse(w, 200, view)
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) saveConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string           `json:"name"`
		Enabled  bool             `json:"enabled"`
		Config   mcpclient.Config `json:"config"`
		Revision int              `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	key := r.PathValue("id")
	result, err := s.Store.SaveConnection(r.Context(), key, work.Connection{Name: in.Name, Enabled: in.Enabled, Config: in.Config, Revision: in.Revision})
	if err != nil {
		writeError(w, err)
		return
	}
	status := 200
	if key == "" {
		status = 201
	}
	jsonResponse(w, status, result)
}

func (s Server) testConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.TestConnection(r.Context(), r.PathValue("id"), in.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) toolPermission(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Revision int    `json:"revision"`
		Allowed  bool   `json:"allowed"`
		ReadOnly *bool  `json:"read_only,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	readOnly := false
	if in.ReadOnly != nil {
		readOnly = *in.ReadOnly
	}
	if err := s.Store.SetToolPermissionWithMode(r.Context(), r.PathValue("id"), in.Name, in.Version, in.Revision, in.Allowed, readOnly); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) callConnectionTool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string          `json:"name"`
		Revision  int             `json:"revision"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if !decode(w, r, &in) {
		return
	}
	var args map[string]any
	if len(in.Arguments) > 24*1024 || json.Unmarshal(in.Arguments, &args) != nil || args == nil {
		writeError(w, mcpclient.ErrArguments)
		return
	}
	var result work.ConnectionResult
	var err error
	if account, ok := accountFromContext(r); ok {
		result, err = s.Store.CallAccountConnectionTool(r.Context(), account.PrincipalID, r.PathValue("id"), in.Name, in.Revision, args)
	} else {
		result, err = s.Store.CallConnectionTool(r.Context(), r.PathValue("id"), in.Name, in.Revision, args)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if result.SourceUnavailable {
		// Do not emit Result's zero-valued success/error flags for a private result.
		jsonResponse(w, http.StatusOK, map[string]any{"id": result.ID, "received_at": result.ReceivedAt, "source_unavailable": true})
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) connectionEvidence(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	if !validID(key) {
		fail(w, 404, "not_found", "未找到读取结果")
		return
	}
	var content []byte
	var err error
	if account, ok := accountFromContext(r); ok {
		store := s.Store
		if s.instanceStore != nil {
			store = s.instanceStore
		}
		content, err = store.AccountConnectionEvidence(r.Context(), account.PrincipalID, key)
		if s.instanceStore != nil && errors.Is(err, sql.ErrNoRows) {
			content, err = s.Store.ConnectionEvidence(r.Context(), key)
		}
	} else {
		content, err = s.Store.ConnectionEvidence(r.Context(), key)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="tool-result.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

func (s Server) runMCPBindings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := s.Store.ReadRunMCPBindings(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) readConversationMCPSelection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := s.Store.ReadConversationMCPSelection(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) setConversationMCPSelection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in work.ConversationMCPInput
	if !decode(w, r, &in) {
		return
	}
	if !validID(r.PathValue("id")) || (in.ConnectionID != "" && !validID(in.ConnectionID)) {
		writeError(w, work.ErrReadSelection)
		return
	}
	out, err := s.Store.SetConversationMCPSelection(r.Context(), s.principal(r), r.PathValue("id"), in)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
