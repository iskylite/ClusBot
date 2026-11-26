package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"clusbot/internal/mcpclient"
)

var ErrCapabilityBinding = errors.New("能力绑定参数无效或目标 MCP 工具未启用")

// MCPToolBinding maps an abstract capability/benchmark action onto concrete MCP tools.
type MCPToolBinding struct {
	StartTool  string `json:"start_tool"`
	LookupTool string `json:"lookup_tool,omitempty"`
	GetTool    string `json:"get_tool,omitempty"`
	CancelTool string `json:"cancel_tool,omitempty"`
}

type CapabilityBindingInput struct {
	CapabilityKey string `json:"capability_key"`
	Version       int    `json:"version"`
	Kind          string `json:"kind"` // query / action
	TargetKind    string `json:"target_kind,omitempty"`
	ExecutionMode string `json:"execution_mode,omitempty"` // sync / async
	ConnectionID  string `json:"connection_id"`
	MCPToolBinding
	Idempotent     string `json:"idempotent,omitempty"`
	SupportsGet    string `json:"supports_get,omitempty"`
	SupportsCancel string `json:"supports_cancel,omitempty"`
	VerifiedScope  string `json:"verified_scope,omitempty"`
	ExposedToModel bool   `json:"exposed_to_model,omitempty"`
}

type CapabilityBinding struct {
	CapabilityID       string `json:"capability_id"`
	CapabilityKey      string `json:"capability_key"`
	Version            int    `json:"version"`
	Kind               string `json:"kind"`
	TargetKind         string `json:"target_kind,omitempty"`
	ExecutionMode      string `json:"execution_mode"`
	ConnectionID       string `json:"connection_id"`
	ConnectionName     string `json:"connection_name"`
	ConnectionRevision int    `json:"connection_revision"`
	MCPToolBinding
	Idempotent     string `json:"idempotent"`
	SupportsGet    string `json:"supports_get"`
	SupportsCancel string `json:"supports_cancel"`
	VerifiedScope  string `json:"verified_scope,omitempty"`
	ExposedToModel bool   `json:"exposed_to_model"`
	UpdatedAt      int64  `json:"updated_at"`
}

func normalizeSupportFlag(v string) string {
	switch strings.TrimSpace(v) {
	case "supported", "unsupported", "unknown":
		return strings.TrimSpace(v)
	default:
		return "unknown"
	}
}

// UpsertCapabilityBinding persists a capability-to-MCP-tool binding into
// capabilities, capability_providers, and capability_connections.
// Action capabilities are always forced to exposed_to_model=false.
func (s *Store) UpsertCapabilityBinding(ctx context.Context, in CapabilityBindingInput) (CapabilityBinding, error) {
	in.CapabilityKey = strings.TrimSpace(in.CapabilityKey)
	in.Kind = strings.TrimSpace(in.Kind)
	in.ConnectionID = strings.TrimSpace(in.ConnectionID)
	in.StartTool = strings.TrimSpace(in.StartTool)
	in.LookupTool = strings.TrimSpace(in.LookupTool)
	in.GetTool = strings.TrimSpace(in.GetTool)
	in.CancelTool = strings.TrimSpace(in.CancelTool)
	if in.Version < 1 {
		in.Version = 1
	}
	if in.ExecutionMode == "" {
		in.ExecutionMode = "async"
	}
	if in.CapabilityKey == "" || (in.Kind != "query" && in.Kind != "action") ||
		in.ConnectionID == "" || in.StartTool == "" ||
		(in.ExecutionMode != "sync" && in.ExecutionMode != "async" && in.ExecutionMode != "deterministic") {
		return CapabilityBinding{}, ErrCapabilityBinding
	}
	if in.Kind == "action" {
		// Write/action capabilities never enter the generic model automatic toolset.
		in.ExposedToModel = false
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CapabilityBinding{}, err
	}
	defer tx.Rollback()

	var connName, status string
	var connRev int
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT name,revision,status,enabled FROM connections WHERE id=?`, in.ConnectionID).
		Scan(&connName, &connRev, &status, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return CapabilityBinding{}, fmt.Errorf("%w：连接不存在", ErrCapabilityBinding)
	}
	if err != nil {
		return CapabilityBinding{}, err
	}
	if !enabled || status != "ready" {
		return CapabilityBinding{}, fmt.Errorf("%w：连接未就绪或已停用", ErrCapabilityBinding)
	}

	for _, toolName := range []string{in.StartTool, in.LookupTool, in.GetTool, in.CancelTool} {
		if toolName == "" {
			continue
		}
		var allowed bool
		err := tx.QueryRowContext(ctx, `SELECT allowed FROM connection_tools WHERE connection_id=? AND name=?`, in.ConnectionID, toolName).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !allowed) {
			return CapabilityBinding{}, fmt.Errorf("%w：工具 %s 未登记或未授权", ErrCapabilityBinding, toolName)
		}
		if err != nil {
			return CapabilityBinding{}, err
		}
	}

	idempotent := normalizeSupportFlag(in.Idempotent)
	supportsGet := normalizeSupportFlag(in.SupportsGet)
	if supportsGet == "unknown" && in.GetTool != "" {
		supportsGet = "supported"
	}
	supportsCancel := normalizeSupportFlag(in.SupportsCancel)
	if supportsCancel == "unknown" && in.CancelTool != "" {
		supportsCancel = "supported"
	}

	stamp := now()
	var capID string
	err = tx.QueryRowContext(ctx, `SELECT capability_id FROM capabilities WHERE capability_key=? AND version=?`, in.CapabilityKey, in.Version).Scan(&capID)
	if errors.Is(err, sql.ErrNoRows) {
		capID = id()
		_, err = tx.ExecContext(ctx, `INSERT INTO capabilities(
capability_id,capability_key,version,kind,target_kind,execution_mode,
supports_get,supports_cancel,supports_events,supports_materials,idempotent,fencing,
verified_scope,exposed_to_model,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,'unsupported','unsupported',?,'unknown',?,?,?,?)`,
			capID, in.CapabilityKey, in.Version, in.Kind, in.TargetKind, in.ExecutionMode,
			supportsGet, supportsCancel, idempotent, in.VerifiedScope, boolToInt(in.ExposedToModel), stamp, stamp)
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE capabilities SET
kind=?,target_kind=?,execution_mode=?,supports_get=?,supports_cancel=?,idempotent=?,verified_scope=?,exposed_to_model=?,updated_at=?
WHERE capability_id=?`,
			in.Kind, in.TargetKind, in.ExecutionMode, supportsGet, supportsCancel, idempotent, in.VerifiedScope, boolToInt(in.ExposedToModel), stamp, capID)
	}
	if err != nil {
		return CapabilityBinding{}, err
	}

	bindingJSON, err := json.Marshal(struct {
		ConnectionID string `json:"connection_id"`
		MCPToolBinding
	}{in.ConnectionID, in.MCPToolBinding})
	if err != nil {
		return CapabilityBinding{}, err
	}
	capRefJSON, err := json.Marshal([]string{fmt.Sprintf("%s:%d", in.CapabilityKey, in.Version)})
	if err != nil {
		return CapabilityBinding{}, err
	}
	providerKey := fmt.Sprintf("mcp:%s:%s:%d", in.ConnectionID, in.CapabilityKey, in.Version)
	var providerID string
	err = tx.QueryRowContext(ctx, `SELECT provider_id FROM capability_providers WHERE provider_key=? AND version=?`, providerKey, connRev).Scan(&providerID)
	if errors.Is(err, sql.ErrNoRows) {
		providerID = id()
		if _, err = tx.ExecContext(ctx, `INSERT INTO capability_providers(provider_id,provider_key,version,kind,config_ref,capabilities,created_at)
VALUES(?,?,?,'mcp',?,?,?)`, providerID, providerKey, connRev, string(bindingJSON), string(capRefJSON), stamp); err != nil {
			return CapabilityBinding{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO capability_connections(connection_id,provider_id,connection_name,connection_version,credential_ref,enabled,created_at)
VALUES(?,?,?,?,?,1,?)`, id(), providerID, connName, connRev, in.ConnectionID, stamp); err != nil {
			return CapabilityBinding{}, err
		}
	} else if err == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE capability_providers SET config_ref=?,capabilities=? WHERE provider_id=?`,
			string(bindingJSON), string(capRefJSON), providerID); err != nil {
			return CapabilityBinding{}, err
		}
	} else {
		return CapabilityBinding{}, err
	}

	if err = tx.Commit(); err != nil {
		return CapabilityBinding{}, err
	}
	return CapabilityBinding{
		CapabilityID:       capID,
		CapabilityKey:      in.CapabilityKey,
		Version:            in.Version,
		Kind:               in.Kind,
		TargetKind:         in.TargetKind,
		ExecutionMode:      in.ExecutionMode,
		ConnectionID:       in.ConnectionID,
		ConnectionName:     connName,
		ConnectionRevision: connRev,
		MCPToolBinding:     in.MCPToolBinding,
		Idempotent:         idempotent,
		SupportsGet:        supportsGet,
		SupportsCancel:     supportsCancel,
		VerifiedScope:      in.VerifiedScope,
		ExposedToModel:     in.ExposedToModel,
		UpdatedAt:          stamp,
	}, nil
}

func (s *Store) ListCapabilityBindings(ctx context.Context) ([]CapabilityBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.capability_id,c.capability_key,c.version,c.kind,c.target_kind,c.execution_mode,
c.supports_get,c.supports_cancel,c.idempotent,c.verified_scope,c.exposed_to_model,c.updated_at,
p.config_ref,cc.connection_name,cc.connection_version
FROM capabilities c
JOIN capability_providers p ON p.kind='mcp' AND p.capabilities=json_array(c.capability_key||':'||c.version)
JOIN capability_connections cc ON cc.provider_id=p.provider_id AND cc.enabled=1
ORDER BY c.capability_key,c.version DESC,p.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CapabilityBinding{}
	seen := make(map[string]bool)
	for rows.Next() {
		var b CapabilityBinding
		var exposed int
		var configRef string
		if err := rows.Scan(&b.CapabilityID, &b.CapabilityKey, &b.Version, &b.Kind, &b.TargetKind, &b.ExecutionMode,
			&b.SupportsGet, &b.SupportsCancel, &b.Idempotent, &b.VerifiedScope, &exposed, &b.UpdatedAt,
			&configRef, &b.ConnectionName, &b.ConnectionRevision); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%d", b.CapabilityKey, b.Version)
		if seen[key] {
			continue
		}
		seen[key] = true
		b.ExposedToModel = exposed != 0
		var cfg struct {
			ConnectionID string `json:"connection_id"`
			MCPToolBinding
		}
		if err := json.Unmarshal([]byte(configRef), &cfg); err == nil {
			b.ConnectionID = cfg.ConnectionID
			b.MCPToolBinding = cfg.MCPToolBinding
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MCPToolCaller abstracts calling a bound MCP tool so unit tests can inject
// deterministic Cluskit2 MCP stubs while production uses mcpclient.Call.
type MCPToolCaller func(ctx context.Context, toolName string, args map[string]any) (mcpclient.Result, error)

// MCPBenchmarkProvider adapts an MCP server (such as Cluskit2-MCP) to the
// BenchmarkProvider + benchmarkCanceller Operation contract.
type MCPBenchmarkProvider struct {
	connection      string
	revision        int
	binding         MCPToolBinding
	caller          MCPToolCaller
	pin             string
	jobSource       string
	runtimeReady    func() bool
	startDefinition *mcpclient.Tool
}

func NewMCPBenchmarkProvider(connection string, revision int, binding MCPToolBinding, caller MCPToolCaller) (*MCPBenchmarkProvider, error) {
	if !validBenchKey(connection) || revision < 1 || strings.TrimSpace(binding.StartTool) == "" || caller == nil {
		return nil, ErrBenchmarkBinding
	}
	return &MCPBenchmarkProvider{
		connection: connection,
		revision:   revision,
		binding:    binding,
		caller:     caller,
	}, nil
}

func (p *MCPBenchmarkProvider) Supports(b BenchmarkBinding) bool {
	return p != nil && b.Connection == p.connection && b.Revision == p.revision &&
		b.Validator == benchmarkValidatorV1 && b.Tool == p.binding.StartTool &&
		(p.runtimeReady == nil || p.runtimeReady())
}

func (p *MCPBenchmarkProvider) benchmarkJobSource() string { return p.jobSource }

func (p *MCPBenchmarkProvider) Start(ctx context.Context, req BenchmarkJobRequest) (string, error) {
	if !p.Supports(req.Binding) || req.IdempotencyKey == "" {
		return "", ErrBenchmarkBinding
	}
	var args map[string]any
	if p.binding.StartTool == "bench_run" {
		var err error
		args, err = cluskitBenchmarkArguments(req)
		if err != nil {
			return "", err
		}
	} else {
		args = map[string]any{
			"idempotency_key": req.IdempotencyKey,
			"case":            req.Case,
			"targets":         req.Targets,
			"parameters":      req.Parameters,
		}
	}
	res, err := p.caller(ctx, p.binding.StartTool, args)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("mcp start error: %s", res.Text)
	}
	var out struct {
		JobID  string `json:"job_id"`
		TaskID string `json:"task_id"`
	}
	if err := decodeMCPResultJSON(res, &out); err != nil {
		return "", err
	}
	if out.JobID != "" && out.TaskID != "" && out.JobID != out.TaskID {
		return "", ErrBenchmarkBinding
	}
	if out.JobID != "" {
		return out.JobID, nil
	}
	if out.TaskID != "" {
		return out.TaskID, nil
	}
	return "", ErrBenchmarkBinding
}

func (p *MCPBenchmarkProvider) Lookup(ctx context.Context, req BenchmarkJobRequest) (string, error) {
	if !p.Supports(req.Binding) || req.IdempotencyKey == "" || strings.TrimSpace(p.binding.LookupTool) == "" {
		return "", ErrBenchmarkBinding
	}
	args := map[string]any{
		"idempotency_key": req.IdempotencyKey,
	}
	if p.binding.LookupTool == "task_status" {
		args["tool"] = p.binding.StartTool
	} else {
		args["case"] = req.Case
	}
	res, err := p.caller(ctx, p.binding.LookupTool, args)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("mcp lookup error: %s", res.Text)
	}
	var out struct {
		Found  *bool  `json:"found"`
		JobID  string `json:"job_id"`
		TaskID string `json:"task_id"`
		State  string `json:"state"`
		Status string `json:"status"`
	}
	if err := decodeMCPResultJSON(res, &out); err != nil {
		return "", err
	}
	if (out.Found != nil && !*out.Found) ||
		strings.EqualFold(out.State, "NOT_FOUND") ||
		strings.EqualFold(out.Status, "not_found") {
		return "", nil
	}
	if out.JobID != "" && out.TaskID != "" && out.JobID != out.TaskID {
		return "", ErrBenchmarkBinding
	}
	if out.JobID != "" {
		return out.JobID, nil
	}
	return out.TaskID, nil
}

func (p *MCPBenchmarkProvider) Get(ctx context.Context, req BenchmarkJobRequest, jobID string) (BenchmarkJobResult, error) {
	if !p.Supports(req.Binding) || strings.TrimSpace(jobID) == "" || strings.TrimSpace(p.binding.GetTool) == "" {
		return BenchmarkJobResult{}, ErrBenchmarkBinding
	}
	args := map[string]any{
		"task_id": jobID,
	}
	if p.binding.GetTool == "task_status" {
		args["include_results"] = true
	} else {
		args["job_id"] = jobID
		args["case"] = req.Case
	}
	res, err := p.caller(ctx, p.binding.GetTool, args)
	if err != nil {
		return BenchmarkJobResult{}, err
	}
	if res.IsError {
		return BenchmarkJobResult{}, fmt.Errorf("mcp get error: %s", res.Text)
	}
	var out BenchmarkJobResult
	if err := decodeMCPResultJSON(res, &out); err != nil {
		return BenchmarkJobResult{}, err
	}
	var env struct {
		TaskID               string                         `json:"task_id"`
		State                string                         `json:"state"`
		Status               string                         `json:"status"`
		RemoteExecutionState string                         `json:"remote_execution_state"`
		RemoteExitVerified   *bool                          `json:"remote_exit_verified"`
		Results              []cluskitBenchmarkResultRecord `json:"results"`
	}
	if err := decodeMCPResultJSON(res, &env); err != nil {
		return BenchmarkJobResult{}, err
	}
	out.JobID = firstNonEmpty(out.JobID, env.TaskID)
	if out.JobID != jobID || env.TaskID != "" && env.TaskID != jobID {
		return BenchmarkJobResult{}, fmt.Errorf("%w：没有核对到原基准作业的身份", ErrBenchmarkBinding)
	}
	rawState := firstNonEmpty(out.State, env.State, env.Status)
	if rawState != "" {
		switch strings.ToLower(strings.TrimSpace(rawState)) {
		case "queued", "running", "active", "rrunning", "pending":
			out.State = "running"
		case "succeeded", "completed", "complete":
			out.State = "completed"
		case "failed":
			out.State = "failed"
		case "canceled", "cancelled":
			out.State = "cancelled"
		}
	}
	if p.binding.GetTool == "task_status" {
		out.Measurements = nil
		if out.State == "completed" || out.State == "failed" || out.State == "cancelled" {
			// task_status may report a local transport terminal state while
			// explicitly saying that the remote process has not been checked.
			// Never let that statement settle the original operation or release
			// its physical-asset hold (not even if a cancel was acknowledged).
			remoteState := strings.ToLower(strings.TrimSpace(env.RemoteExecutionState))
			needsExitProof := p.runtimeReady != nil && (out.State == "cancelled" || out.State == "failed")
			if needsExitProof && env.RemoteExitVerified == nil ||
				env.RemoteExitVerified != nil && !*env.RemoteExitVerified ||
				remoteState != "" && remoteState != "exited" && remoteState != "stopped" {
				return BenchmarkJobResult{}, fmt.Errorf("%w：原作业的远端进程尚未核实退出", ErrBenchmarkBinding)
			}
			out.Measurements = cluskitBenchmarkMeasurements(req, jobID, env.Results)
		}
	}
	return out, nil
}

func (p *MCPBenchmarkProvider) Cancel(ctx context.Context, req BenchmarkJobRequest, jobID string) error {
	if !p.Supports(req.Binding) || strings.TrimSpace(jobID) == "" || strings.TrimSpace(p.binding.CancelTool) == "" {
		return ErrBenchmarkBinding
	}
	args := map[string]any{
		"task_id": jobID,
	}
	if p.binding.CancelTool != "task_cancel" {
		args["job_id"] = jobID
	}
	res, err := p.caller(ctx, p.binding.CancelTool, args)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("mcp cancel error: %s", res.Text)
	}
	var out struct {
		Acknowledged bool            `json:"acknowledged"`
		Confirmed    json.RawMessage `json:"confirmed"`
		Unconfirmed  []string        `json:"unconfirmed"`
		TaskIDs      []string        `json:"task_ids"`
		Status       string          `json:"status"`
	}
	if err := decodeMCPResultJSON(res, &out); err != nil {
		return err
	}
	status := strings.ToLower(strings.TrimSpace(out.Status))
	if len(out.TaskIDs) != 0 && (len(out.TaskIDs) != 1 || out.TaskIDs[0] != jobID) || len(out.Unconfirmed) != 0 {
		return ErrState
	}
	var addresses []string
	if p.binding.CancelTool == "task_cancel" {
		if len(out.TaskIDs) != 1 || !out.Acknowledged || json.Unmarshal(out.Confirmed, &addresses) != nil || len(addresses) == 0 {
			return ErrState
		}
		// This ack only proves acceptance. Get must still confirm the job's exit.
		return nil
	}
	var legacyAck bool
	if len(out.Confirmed) != 0 && json.Unmarshal(out.Confirmed, &addresses) != nil && json.Unmarshal(out.Confirmed, &legacyAck) != nil {
		return ErrState
	}
	// Confirmed entries (engine:..., local:..., etc.) acknowledge cancel
	// delivery only. The runtime must still query the original job's exit.
	if !out.Acknowledged && !legacyAck && len(addresses) == 0 && status != "confirmed" && status != "canceled" && status != "canceling" {
		return ErrState
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func decodeMCPResultJSON(res mcpclient.Result, target any) error {
	if len(res.Structured) > 0 {
		return json.Unmarshal(res.Structured, target)
	}
	if strings.TrimSpace(res.Text) != "" {
		return json.Unmarshal([]byte(res.Text), target)
	}
	return ErrBenchmarkBinding
}

func toolSchemaProperties(tool mcpclient.Tool) map[string]any {
	if len(tool.InputSchema) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(tool.InputSchema, &m) != nil {
		return nil
	}
	props, _ := m["properties"].(map[string]any)
	return props
}

func buildTargetArgs(tool mcpclient.Tool, target string, extra map[string]any) map[string]any {
	props := toolSchemaProperties(tool)
	args := map[string]any{}
	target = strings.TrimSpace(target)
	if target != "" {
		if props == nil {
			args["nodes"] = target
		} else if _, ok := props["nodes"]; ok {
			args["nodes"] = target
		} else if _, ok := props["target"]; ok {
			args["target"] = target
		}
	}
	for k, v := range extra {
		if props == nil {
			args[k] = v
			continue
		}
		if _, ok := props[k]; ok {
			args[k] = v
		}
	}
	return args
}

// CallBoundMCPTool invokes an enabled, ready MCP tool resolved via exact capability_key
// (or an explicitly allowed read-only tool when capabilityKey == "").
func (s *Store) CallBoundMCPTool(ctx context.Context, capabilityKey, preferredTool string, args map[string]any) (ConnectionResult, CapabilityBinding, bool, error) {
	return s.callBoundMCPToolInternal(ctx, capabilityKey, preferredTool, "", args, false)
}

// CallBoundMCPTargetTool resolves a bound MCP tool and adapts target/extra arguments to its real JSON Schema.
func (s *Store) CallBoundMCPTargetTool(ctx context.Context, capabilityKey, preferredTool, target string, extra map[string]any) (ConnectionResult, CapabilityBinding, bool, error) {
	return s.callBoundMCPToolInternal(ctx, capabilityKey, preferredTool, target, extra, true)
}

func (s *Store) callBoundMCPToolInternal(ctx context.Context, capabilityKey, preferredTool, target string, rawArgs map[string]any, adaptTarget bool) (ConnectionResult, CapabilityBinding, bool, error) {
	bindings, err := s.ListCapabilityBindings(ctx)
	if err == nil {
		for _, b := range bindings {
			matchKey := capabilityKey != "" && b.CapabilityKey == capabilityKey
			matchTool := preferredTool != "" && (b.StartTool == preferredTool || b.LookupTool == preferredTool || b.GetTool == preferredTool)
			if !matchKey && !matchTool {
				continue
			}
			conn, connErr := s.Connection(ctx, b.ConnectionID)
			if connErr != nil || !conn.Enabled || conn.Status != "ready" {
				continue
			}
			toolName := b.StartTool
			if preferredTool != "" && (b.StartTool == preferredTool || b.LookupTool == preferredTool || b.GetTool == preferredTool) {
				toolName = preferredTool
			}
			args := rawArgs
			if adaptTarget {
				var matchedTool mcpclient.Tool
				for _, t := range conn.Tools {
					if t.Name == toolName {
						matchedTool = t
						break
					}
				}
				args = buildTargetArgs(matchedTool, target, rawArgs)
			}
			res, callErr := s.CallConnectionTool(ctx, conn.ID, toolName, conn.Revision, args)
			return res, b, true, callErr
		}
	}
	if capabilityKey == "" && preferredTool != "" {
		conns, listErr := s.Connections(ctx)
		if listErr == nil {
			for _, item := range conns {
				if !item.Enabled || item.Status != "ready" {
					continue
				}
				conn, connErr := s.Connection(ctx, item.ID)
				if connErr != nil {
					continue
				}
				for _, t := range conn.Tools {
					if t.Name == preferredTool && t.Allowed && !isToolDisallowedForReadOnly(t) {
						args := rawArgs
						if adaptTarget {
							args = buildTargetArgs(t, target, rawArgs)
						}
						res, callErr := s.CallConnectionTool(ctx, conn.ID, preferredTool, conn.Revision, args)
						return res, CapabilityBinding{
							ConnectionID:       conn.ID,
							ConnectionName:     conn.Name,
							ConnectionRevision: conn.Revision,
							MCPToolBinding:     MCPToolBinding{StartTool: preferredTool},
						}, true, callErr
					}
				}
			}
		}
	}
	return ConnectionResult{}, CapabilityBinding{}, false, nil
}

type dynamicBenchmarkProvider struct {
	store    *Store
	fallback BenchmarkProvider
}

// DynamicBenchmarkProvider resolves capability_bindings for MCP benchmark tools at runtime,
// falling back to an optional HTTP BenchmarkProvider when no MCP binding matches.
func (s *Store) DynamicBenchmarkProvider(fallback BenchmarkProvider) BenchmarkProvider {
	return &dynamicBenchmarkProvider{store: s, fallback: fallback}
}

func (p *dynamicBenchmarkProvider) Supports(b BenchmarkBinding) bool {
	if p == nil || p.store == nil {
		return false
	}
	_, err := p.resolve(context.Background(), p.store.db, b)
	return err == nil
}

func (p *dynamicBenchmarkProvider) Start(ctx context.Context, req BenchmarkJobRequest) (string, error) {
	pinned, err := p.resolve(ctx, p.store.db, req.Binding)
	if err != nil {
		return "", err
	}
	return pinned.provider.Start(ctx, req)
}

func (p *dynamicBenchmarkProvider) Lookup(ctx context.Context, req BenchmarkJobRequest) (string, error) {
	pinned, err := p.resolve(ctx, p.store.db, req.Binding)
	if err != nil {
		return "", err
	}
	return pinned.provider.Lookup(ctx, req)
}

func (p *dynamicBenchmarkProvider) Get(ctx context.Context, req BenchmarkJobRequest, jobID string) (BenchmarkJobResult, error) {
	pinned, err := p.resolve(ctx, p.store.db, req.Binding)
	if err != nil {
		return BenchmarkJobResult{}, err
	}
	return pinned.provider.Get(ctx, req, jobID)
}

func (p *dynamicBenchmarkProvider) Cancel(ctx context.Context, req BenchmarkJobRequest, jobID string) error {
	pinned, err := p.resolve(ctx, p.store.db, req.Binding)
	if err != nil {
		return err
	}
	if !pinned.canStop {
		return ErrBenchmarkBinding
	}
	return pinned.provider.(benchmarkCanceller).Cancel(ctx, req, jobID)
}

// DynamicEffectChecker verifies recovery only from a concrete, independent
// device-health check. Generic Slurm fault listings and command success cannot
// prove a healthy device. An explicitly configured HTTP checker is a fallback
// only when no MCP checker was configured, not when the active source fails.
func (s *Store) DynamicEffectChecker(fallback EffectChecker) EffectChecker {
	return func(ctx context.Context, objectRef string) (bool, string, error) {
		if strings.TrimSpace(objectRef) == "" {
			return false, "", fmt.Errorf("%w：复检需要明确目标", ErrHealthCoverage)
		}
		res, _, bound, err := s.callBoundNHC(ctx, "ops.effect_check", []string{objectRef}, true)
		if err != nil {
			return false, "", err
		}
		if !bound {
			if fallback != nil {
				return fallback(ctx, objectRef)
			}
			return false, "", fmt.Errorf("%w：未配置独立设备健康验证器或 ops.effect_check → nhc", ErrCapabilityBinding)
		}
		report := parseMCPNHCResult(res, []string{objectRef})
		switch report.Nodes[objectRef] {
		case mcpNHCHealthy:
			if report.Complete && report.Status == "COMPLETE" && !res.IsError {
				return true, mcpNHCEvidence(res, report, objectRef, mcpNHCHealthy), nil
			}
		case mcpNHCFault:
			return false, mcpNHCEvidence(res, report, objectRef, mcpNHCFault), nil
		}
		return false, "", fmt.Errorf("%w：没有节点 %s 本次完整的 NHC 结果", ErrHealthCoverage, objectRef)
	}
}
