package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	ErrConnectionRevision    = errors.New("连接已更新，请刷新详情后重试")
	ErrConnectionUnavailable = errors.New("请启用连接并成功测试后再授权或调用工具")
	ErrConnectionDuplicate   = errors.New("连接名称已存在，或已达到 30 个连接的上限")
	ErrToolPermission        = errors.New("当前工具未获得读取授权")
)

type Connection struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	Config            mcpclient.Config `json:"config"`
	Enabled           bool             `json:"enabled"`
	Revision          int              `json:"revision"`
	Status            string           `json:"status"`
	Detail            string           `json:"detail,omitempty"`
	Protocol          string           `json:"protocol,omitempty"`
	CheckedAt         int64            `json:"checked_at,omitempty"`
	ToolCount         int              `json:"tool_count"`
	Tools             []mcpclient.Tool `json:"tools,omitempty"`
	credentialBinding string
}

const connectionFields = `c.id,c.name,c.config,c.enabled,c.revision,c.status,c.detail,c.protocol,c.checked_at,c.credential_binding`
const connectionColumns = connectionFields + `,(SELECT count(*) FROM connection_tools WHERE connection_id=c.id)`

func scanConnection(row scanner) (Connection, error) {
	var c Connection
	var raw string
	err := row.Scan(&c.ID, &c.Name, &raw, &c.Enabled, &c.Revision, &c.Status, &c.Detail, &c.Protocol, &c.CheckedAt, &c.credentialBinding, &c.ToolCount)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c.Config)
	}
	return c, err
}

func (s *Store) Connections(ctx context.Context) ([]Connection, error) {
	return s.connections(ctx, "SELECT "+connectionColumns+" FROM connections c ORDER BY c.name,c.id")
}

func (s *Store) SharedConnections(ctx context.Context) ([]Connection, error) {
	return s.connections(ctx, `SELECT `+connectionFields+`,count(*) FROM connections c
JOIN connection_tools t ON t.connection_id=c.id AND t.allowed=1
WHERE c.enabled=1 AND c.status='ready' GROUP BY c.id ORDER BY c.name,c.id`)
}

func (s *Store) connections(ctx context.Context, query string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (s *Store) Connection(ctx context.Context, key string) (Connection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	c, err := scanConnection(tx.QueryRowContext(ctx, "SELECT "+connectionColumns+" FROM connections c WHERE c.id=?", key))
	if err != nil {
		return c, err
	}
	c.Tools, err = connectionTools(ctx, tx, key)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func isKnownUnsafeWriteTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "run", "tree_run", "star_run", "copy", "setssh", "setpwd",
		"power", "boot", "install", "relocate",
		"save", "save_bmc", "save_redis", "save_gv",
		"setbase", "setntp", "setnfs", "setall",
		"star_cancel", "star_artifact", "tree_cancel", "tree_copy",
		"bench_run", "bench_install", "bench_save", "local_probe",
		"offline", "online", "ac", "update", "fault", "task_cancel":
		return true
	}
	return strings.HasPrefix(lower, "update_") || (strings.HasPrefix(lower, "set") && lower != "settings")
}

// MCPReadOnlyEligible reports whether an administrator may designate a catalog entry as read-only.
func MCPReadOnlyEligible(tool mcpclient.Tool) bool {
	return tool.SchemaError == "" && !isToolDisallowedForReadOnly(tool)
}

func isToolDisallowedForReadOnly(tool mcpclient.Tool) bool {
	if isKnownUnsafeWriteTool(tool.Name) || tool.Destructive {
		return true
	}
	if tool.ServerReadOnly != nil && !*tool.ServerReadOnly {
		return true
	}
	return false
}

func connectionTools(ctx context.Context, tx *sql.Tx, key string) ([]mcpclient.Tool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT definition,allowed,read_only FROM connection_tools WHERE connection_id=? ORDER BY name", key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []mcpclient.Tool
	for rows.Next() {
		tool, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, tool)
	}
	return items, rows.Err()
}

func scanTool(row scanner) (mcpclient.Tool, error) {
	var tool mcpclient.Tool
	var raw string
	var allowed, readOnly bool
	err := row.Scan(&raw, &allowed, &readOnly)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &tool)
	}
	tool.Allowed = allowed
	tool.ReadOnly = readOnly && !isToolDisallowedForReadOnly(tool)
	if err == nil {
		if schemaErr := mcpclient.ValidateToolSchemas(tool); schemaErr != nil {
			tool.SchemaError = schemaErr.Error()
		}
	}
	return tool, err
}

func (s *Store) SaveConnection(ctx context.Context, key string, c Connection) (Connection, error) {
	return s.saveConnection(ctx, key, c, "")
}

// MCPConnectionDefinition normalizes and validates exactly what saving will
// store. This does not inspect the environment, connect, or start a program.
func MCPConnectionDefinition(c Connection) (Connection, []byte, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || utf8.RuneCountInString(c.Name) > 60 {
		return Connection{}, nil, mcpclient.ErrConfig
	}
	if err := c.Config.Validate(); err != nil {
		return Connection{}, nil, err
	}
	raw, err := json.Marshal(c.Config)
	return c, raw, err
}

func (s *Store) saveConnection(ctx context.Context, key string, c Connection, createID string) (Connection, error) {
	c, raw, err := MCPConnectionDefinition(c)
	if err != nil {
		return Connection{}, err
	}
	s.connectionGrantMu.Lock()
	defer s.connectionGrantMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	if createID != "" {
		saved, findErr := scanConnection(tx.QueryRowContext(ctx, "SELECT "+connectionColumns+" FROM connections c WHERE c.id=?", createID))
		if findErr == nil {
			cfg, _ := json.Marshal(saved.Config)
			if saved.Name != c.Name || saved.Enabled != c.Enabled || string(cfg) != string(raw) {
				return Connection{}, ErrConnectionRevision
			}
			return saved, nil
		}
		if !errors.Is(findErr, sql.ErrNoRows) {
			return Connection{}, findErr
		}
	}
	if err = checkMCPConnectionCapacity(ctx, tx, c.Name, key); err != nil {
		return Connection{}, err
	}
	if key == "" {
		key = createID
		if key == "" {
			key = id()
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO connections(id,name,config,enabled) VALUES(?,?,?,?)", key, c.Name, string(raw), c.Enabled)
	} else {
		err = updateConnection(ctx, tx, `UPDATE connections SET name=?,config=?,enabled=?,revision=revision+1,
status='untested',detail='',protocol='',checked_at=0,credential_binding='' WHERE id=? AND revision=?`, c.Name, string(raw), c.Enabled, key, c.Revision)
		if err == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM connection_tools WHERE connection_id=?", key)
		}
	}
	if err != nil {
		return Connection{}, err
	}
	saved, err := scanConnection(tx.QueryRowContext(ctx, "SELECT "+connectionColumns+" FROM connections c WHERE c.id=?", key))
	if err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Connection{}, err
	}
	s.invalidateMCPSession(key, saved.Revision)
	return saved, nil
}

// CheckNewMCPConnectionName is local and read-only. Registration checks this
// again inside its transaction because another editor may reserve the name.
func (s *Store) CheckNewMCPConnectionName(ctx context.Context, name string) error {
	return checkMCPConnectionCapacity(ctx, s.db, name, "")
}

func checkMCPConnectionCapacity(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name, skip string) error {
	var total int
	var duplicate bool
	if err := q.QueryRowContext(ctx, "SELECT count(*),coalesce(max(name=?),0) FROM connections WHERE id<>?", name, skip).Scan(&total, &duplicate); err != nil {
		return err
	}
	if duplicate || total >= 30 {
		return ErrConnectionDuplicate
	}
	return nil
}

func updateConnection(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed != 1 {
		return ErrConnectionRevision
	}
	return err
}

func (s *Store) TestConnection(ctx context.Context, key string, revision int) (Connection, error) {
	c, err := s.Connection(ctx, key)
	if err != nil {
		return c, err
	}
	if c.Revision != revision {
		return c, ErrConnectionRevision
	}
	if !c.Enabled {
		return c, ErrConnectionUnavailable
	}
	probe, cancel := context.WithTimeout(ctx, mcpclient.Timeout)
	defer cancel()
	var catalog mcpclient.Catalog
	binding, bindingErr := c.Config.CredentialBinding(s.connectionKey)
	if bindingErr != nil {
		return c, bindingErr
	}
	session, probeErr := mcpclient.Connect(probe, c.Config)
	if probeErr == nil {
		catalog, probeErr = mcpclient.Discover(probe, session)
		_ = session.Close()
	}
	s.connectionGrantMu.Lock()
	defer s.connectionGrantMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	status, detail := "ready", ""
	if probeErr != nil {
		status, detail = "failed", probeErr.Error()
	}
	err = updateConnection(ctx, tx, `UPDATE connections SET status=?,detail=?,protocol=?,checked_at=?,credential_binding=?,revision=revision+1
WHERE id=? AND revision=? AND enabled=1`, status, detail, catalog.Protocol, now(), binding, key, revision)
	if err != nil {
		return c, err
	}
	if probeErr == nil {
		if _, err = tx.ExecContext(ctx, "DELETE FROM connection_tools WHERE connection_id=?", key); err != nil {
			return c, err
		}
		prior := map[string]string{}
		priorReadOnly := map[string]bool{}
		for _, tool := range c.Tools {
			if tool.Allowed && c.credentialBinding == binding && c.Protocol == catalog.Protocol {
				prior[tool.Name] = tool.Version
				priorReadOnly[tool.Name] = tool.ReadOnly
			}
		}
		for _, tool := range catalog.Tools {
			definition, err := json.Marshal(tool)
			if err != nil {
				return c, err
			}
			keepAllowed := prior[tool.Name] == tool.Version && tool.SchemaError == ""
			keepReadOnly := keepAllowed && priorReadOnly[tool.Name] && !isToolDisallowedForReadOnly(tool)
			if _, err = tx.ExecContext(ctx, "INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only) VALUES(?,?,?,?,?)",
				key, tool.Name, string(definition), keepAllowed, keepReadOnly); err != nil {
				return c, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return c, err
	}
	s.invalidateMCPSession(key, revision+1)
	return s.Connection(ctx, key)
}

func (s *Store) SetToolPermission(ctx context.Context, key, name, version string, revision int, allowed bool) error {
	return s.SetToolPermissionWithMode(ctx, key, name, version, revision, allowed, allowed && !isKnownUnsafeWriteTool(name))
}

func (s *Store) SetToolPermissionWithMode(ctx context.Context, key, name, version string, revision int, allowed, readOnly bool) error {
	if readOnly && isKnownUnsafeWriteTool(name) {
		return fmt.Errorf("%w：写工具 %s 禁止加入只读自动工具集，请走受控能力绑定与 Operation 通道", ErrToolPermission, name)
	}
	if !allowed {
		readOnly = false
	}
	s.connectionGrantMu.Lock()
	defer s.connectionGrantMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, rawConfig, savedBinding string
	var enabled bool
	var current int
	if err = tx.QueryRowContext(ctx, "SELECT status,enabled,revision,config,credential_binding FROM connections WHERE id=?", key).
		Scan(&status, &enabled, &current, &rawConfig, &savedBinding); err != nil {
		return err
	}
	if current != revision {
		return ErrConnectionRevision
	}
	if allowed && (!enabled || status != "ready") {
		return ErrConnectionUnavailable
	}
	if allowed {
		var cfg mcpclient.Config
		if err = json.Unmarshal([]byte(rawConfig), &cfg); err != nil {
			return err
		}
		binding, err := cfg.CredentialBinding(s.connectionKey)
		if err != nil {
			return err
		}
		if binding != savedBinding {
			return mcpclient.ErrSchemaChanged
		}
	}
	tool, err := scanTool(tx.QueryRowContext(ctx, "SELECT definition,allowed,read_only FROM connection_tools WHERE connection_id=? AND name=?", key, name))
	if err != nil {
		return err
	}
	if tool.Version != version {
		return mcpclient.ErrSchemaChanged
	}
	if allowed {
		if err = mcpclient.ValidateToolSchemas(tool); err != nil {
			return err
		}
		if readOnly && isToolDisallowedForReadOnly(tool) {
			return ErrToolPermission
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE connection_tools SET allowed=?,read_only=? WHERE connection_id=? AND name=?", allowed, readOnly, key, name); err != nil {
		return err
	}
	if err = updateConnection(ctx, tx, "UPDATE connections SET revision=revision+1 WHERE id=? AND revision=?", key, revision); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateMCPSession(key, revision+1)
	return nil
}

type ConnectionResult struct {
	mcpclient.Result
	ID                string `json:"id"`
	ReceivedAt        int64  `json:"received_at"`
	SourceUnavailable bool   `json:"source_unavailable,omitempty"`
}

func (s *Store) CallConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any) (ConnectionResult, error) {
	return s.callDirectConnectionTool(ctx, key, name, revision, args, "", mcpclient.Timeout)
}

func (s *Store) CallAccountConnectionTool(ctx context.Context, principal, key, name string, revision int, args map[string]any) (ConnectionResult, error) {
	if principal == "" {
		return ConnectionResult{}, ErrAccountDenied
	}
	return s.callDirectConnectionTool(ctx, key, name, revision, args, principal, mcpclient.Timeout)
}

// Only application-managed domain logic can set this timeout. Normal account
// and model-initiated calls do not expose a deadline argument.
func (s *Store) callTrustedDomainConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any, timeout time.Duration) (ConnectionResult, error) {
	if timeout <= 0 || timeout > mcpclient.MaxDomainCall {
		return ConnectionResult{}, mcpclient.ErrConfig
	}
	return s.callDirectConnectionTool(ctx, key, name, revision, args, "", timeout)
}

func (s *Store) callDirectConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any, principal string, timeout time.Duration) (ConnectionResult, error) {
	result, err := s.executeConnectionTool(ctx, key, name, revision, args, "", 0, principal, timeout)
	if err != nil {
		return result, err
	}
	// The already-issued call may settle after local permission was revoked.
	// Keep its durable outcome but do not send the now-private inline bytes.
	var authorized bool
	query := `SELECT ` + exactMCPAuthority("connection_calls") + ` FROM connection_calls WHERE id=?`
	params := []any{result.ID}
	if principal != "" {
		query += accountConnectionCallScope
		params = append(params, principal)
	}
	err = s.db.QueryRowContext(ctx, query, params...).Scan(&authorized)
	if err != nil {
		return ConnectionResult{}, err
	}
	if !authorized {
		return ConnectionResult{ID: result.ID, ReceivedAt: result.ReceivedAt, SourceUnavailable: true}, nil
	}
	return result, nil
}

func (s *Store) callConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any, runID string, position int) (ConnectionResult, error) {
	return s.executeConnectionTool(ctx, key, name, revision, args, runID, position, "", mcpclient.Timeout)
}

func (s *Store) executeConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any, runID string, position int, principal string, timeout time.Duration) (ConnectionResult, error) {
	if s.cluskitBenchmarkToolReserved(name) {
		return ConnectionResult{}, fmt.Errorf("%w：该工具由个人助手基准运行时专用，请按原任务查询或停止", ErrToolPermission)
	}
	c, err := s.Connection(ctx, key)
	if err != nil {
		return ConnectionResult{}, err
	}
	if c.Revision != revision {
		return ConnectionResult{}, ErrConnectionRevision
	}
	if !c.Enabled || c.Status != "ready" {
		return ConnectionResult{}, ErrConnectionUnavailable
	}
	binding, err := c.Config.CredentialBinding(s.connectionKey)
	if err != nil {
		return ConnectionResult{}, err
	}
	if binding != c.credentialBinding {
		return ConnectionResult{}, mcpclient.ErrSchemaChanged
	}
	var granted mcpclient.Tool
	for _, tool := range c.Tools {
		if tool.Name == name && tool.Allowed {
			granted = tool
			break
		}
	}
	if granted.Name == "" {
		return ConnectionResult{}, ErrToolPermission
	}
	if err = mcpclient.ValidateArguments(granted, args); err != nil {
		return ConnectionResult{}, err
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		session *mcp.ClientSession
		catalog mcpclient.Catalog
		pooled  *pooledMCPSession
	)
	if c.Config.Transport == "stdio" {
		pooled, err = s.acquireMCPSession(deadline, c, binding)
		if err != nil {
			return ConnectionResult{}, err
		}
		var release func()
		deadline, release, err = pooled.beginCall(deadline)
		if err != nil {
			return ConnectionResult{}, err
		}
		defer release()
		session = pooled.session
		catalog = pooled.catalog
	} else {
		handshake, stopHandshake := context.WithTimeout(deadline, mcpclient.Timeout)
		defer stopHandshake()
		if timeout == mcpclient.Timeout {
			session, err = mcpclient.ConnectWithLifetime(handshake, deadline, c.Config)
		} else {
			session, err = mcpclient.ConnectForTrustedDomain(handshake, deadline, c.Config, timeout)
		}
		if err != nil {
			return ConnectionResult{}, err
		}
		defer session.Close()
		catalog, err = mcpclient.Discover(handshake, session)
		stopHandshake()
		if err != nil {
			return ConnectionResult{}, err
		}
	}
	valid := false
	for _, tool := range catalog.Tools {
		if tool.Name == granted.Name && tool.Version == granted.Version {
			if err = mcpclient.ValidateToolSchemas(tool); err != nil {
				return ConnectionResult{}, err
			}
			// Earlier persisted definitions omitted output_schema even though the
			// identity hash included it. Use the freshly verified full definition.
			granted = tool
			valid = true
			break
		}
	}
	if !valid || catalog.Protocol != c.Protocol {
		if pooled != nil {
			s.mcpPool.retire(key, pooled, mcpclient.ErrSchemaChanged)
		}
		return ConnectionResult{}, mcpclient.ErrSchemaChanged
	}
	callID := id()
	if err = s.dispatchConnectionCallForPrincipal(deadline, callID, key, granted, revision, runID, position, principal); err != nil {
		return ConnectionResult{}, err
	}
	var stopAbort func() bool
	if pooled != nil {
		// A timed-out in-flight RPC may be blocked writing to a stalled
		// child. Retire its transport rather than waiting for the write.
		stopAbort = context.AfterFunc(deadline, func() {
			s.mcpPool.retire(key, pooled, mcpclient.ErrCall)
		})
	}
	outcome, evidence, callErr := mcpclient.Call(deadline, session, granted, args, c.Config)
	if stopAbort != nil {
		stopAbort()
	}
	if callErr != nil && pooled != nil {
		s.mcpPool.retire(key, pooled, mcpclient.ErrCall)
	}
	status := "completed"
	if callErr != nil {
		status = "unknown"
	} else if outcome.IsError {
		status = "tool_error"
	}
	receipt := ConnectionResult{Result: outcome, ID: callID, ReceivedAt: now()}
	if _, err = s.db.Exec("UPDATE connection_calls SET status=?,evidence=? WHERE id=?", status, evidence, callID); err != nil {
		return receipt, err
	}
	if len(evidence) != 0 {
		receipt.Reference = &mcpclient.ResultReference{ID: callID, Bytes: len(evidence), SHA256: digest(string(evidence))}
	}
	return receipt, callErr
}

// exactMCPAuthority requires the grant and the source versions that produced the
// read. Regranting increments the connection revision, so it does not revive old evidence.
// row is an internal constant SQL table/alias, never request data.
func exactMCPAuthority(row string) string {
	return `EXISTS(SELECT 1 FROM connections mc JOIN connection_tools mt ON mt.connection_id=mc.id
WHERE mc.id=` + row + `.connection_id AND mc.revision=` + row + `.connection_revision
AND mc.enabled=1 AND mc.status='ready' AND mt.allowed=1
AND mt.name=` + row + `.tool_name AND json_extract(mt.definition,'$.version')=` + row + `.tool_version)`
}

func (s *Store) ConnectionEvidence(ctx context.Context, callID string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT evidence FROM connection_calls
WHERE id=? AND status IN ('completed','tool_error') AND evidence IS NOT NULL AND `+exactMCPAuthority("connection_calls"), callID).Scan(&content)
	return content, err
}

func (s *Store) dispatchConnectionCall(ctx context.Context, callID, key string, tool mcpclient.Tool, revision int, runID string, position int) error {
	return s.dispatchConnectionCallForPrincipal(ctx, callID, key, tool, revision, runID, position, "")
}

func (s *Store) dispatchConnectionCallForPrincipal(ctx context.Context, callID, key string, tool mcpclient.Tool, revision int, runID string, position int, principal string) error {
	s.connectionGrantMu.Lock()
	defer s.connectionGrantMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO connection_calls(id,connection_id,connection_revision,tool_name,tool_version,status,created_at)
SELECT ?,c.id,c.revision,?,?,'dispatched',? FROM connections c JOIN connection_tools t ON t.connection_id=c.id
WHERE c.id=? AND c.revision=? AND c.enabled=1 AND c.status='ready' AND t.name=? AND t.allowed=1 AND json_extract(t.definition,'$.version')=?`,
		callID, tool.Name, tool.Version, now(), key, revision, tool.Name, tool.Version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConnectionRevision
	}
	if principal != "" {
		owned, err := tx.ExecContext(ctx, `INSERT INTO account_connection_calls(call_id,principal_id)
SELECT ?,principal_id FROM accounts WHERE principal_id=? AND status='active'`, callID, principal)
		if err != nil {
			return err
		}
		count, err := owned.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrAccountDenied
		}
	}
	if runID != "" {
		result, err = tx.ExecContext(ctx, `UPDATE run_reads SET status='dispatched',call_id=? WHERE run_id=? AND position=? AND status='checking'
AND connection_id=? AND connection_revision=? AND tool_name=? AND tool_version=?
AND EXISTS(SELECT 1 FROM runs r WHERE r.id=run_reads.run_id AND r.status='running' AND r.control='' AND r.execution_generation=?
AND (r.model_requests=r.admission_requests OR (r.model_requests=r.admission_requests+1 AND run_reads.position=1 AND EXISTS(SELECT 1 FROM run_read_model_calls m WHERE m.run_id=r.id))
OR (run_reads.model_order>0 AND run_reads.model_call_id IS NOT NULL AND EXISTS(SELECT 1 FROM run_read_checkpoints c WHERE c.run_id=r.id AND c.version=?))))`,
			callID, runID, position, key, revision, tool.Name, tool.Version, executionGeneration(ctx), readCheckpointVersion)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrState
		}
		if err = addEvent(tx, runID, "tool.read_changed", map[string]any{"status": "dispatched", "position": position}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
