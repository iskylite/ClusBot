package httpapi

import (
	"encoding/json"
	"net/http"

	"clusbot/internal/mcpclient"
	"clusbot/internal/work"
)

func (s Server) taskTools(w http.ResponseWriter, r *http.Request) {
	options, err := s.Store.TaskTools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, options)
}

func (s Server) connections(w http.ResponseWriter, r *http.Request) {
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
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.SetToolPermission(r.Context(), r.PathValue("id"), in.Name, in.Version, in.Revision, in.Allowed); err != nil {
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
	result, err := s.Store.CallConnectionTool(r.Context(), r.PathValue("id"), in.Name, in.Revision, args)
	if err != nil {
		writeError(w, err)
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
	content, err := s.Store.ConnectionEvidence(r.Context(), key)
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
