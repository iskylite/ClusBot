package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"clusbot/internal/mcpclient"
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

const connectionColumns = `c.id,c.name,c.config,c.enabled,c.revision,c.status,c.detail,c.protocol,c.checked_at,c.credential_binding,
(SELECT count(*) FROM connection_tools WHERE connection_id=c.id)`

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
	rows, err := s.db.QueryContext(ctx, "SELECT "+connectionColumns+" FROM connections c ORDER BY c.name,c.id")
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

func connectionTools(ctx context.Context, tx *sql.Tx, key string) ([]mcpclient.Tool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT definition,allowed FROM connection_tools WHERE connection_id=? ORDER BY name", key)
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
	var allowed bool
	err := row.Scan(&raw, &allowed)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &tool)
	}
	tool.Allowed = allowed
	if err == nil {
		if schemaErr := mcpclient.ValidateToolSchemas(tool); schemaErr != nil {
			tool.SchemaError = schemaErr.Error()
		}
	}
	return tool, err
}

func (s *Store) SaveConnection(ctx context.Context, key string, c Connection) (Connection, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || utf8.RuneCountInString(c.Name) > 60 {
		return Connection{}, mcpclient.ErrConfig
	}
	if err := c.Config.Validate(); err != nil {
		return Connection{}, err
	}
	raw, err := json.Marshal(c.Config)
	if err != nil {
		return Connection{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	var total int
	var duplicate bool
	err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(max(name=?),0) FROM connections WHERE id<>?", c.Name, key).Scan(&total, &duplicate)
	if err != nil {
		return Connection{}, err
	}
	if duplicate || total >= 30 {
		return Connection{}, ErrConnectionDuplicate
	}
	if key == "" {
		key = id()
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
	return saved, tx.Commit()
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
		for _, tool := range c.Tools {
			if tool.Allowed && c.credentialBinding == binding && c.Protocol == catalog.Protocol {
				prior[tool.Name] = tool.Version
			}
		}
		for _, tool := range catalog.Tools {
			definition, err := json.Marshal(tool)
			if err != nil {
				return c, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO connection_tools VALUES(?,?,?,?)", key, tool.Name, string(definition), prior[tool.Name] == tool.Version && tool.SchemaError == ""); err != nil {
				return c, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return c, err
	}
	return s.Connection(ctx, key)
}

func (s *Store) SetToolPermission(ctx context.Context, key, name, version string, revision int, allowed bool) error {
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
	tool, err := scanTool(tx.QueryRowContext(ctx, "SELECT definition,allowed FROM connection_tools WHERE connection_id=? AND name=?", key, name))
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
	}
	if _, err = tx.ExecContext(ctx, "UPDATE connection_tools SET allowed=? WHERE connection_id=? AND name=?", allowed, key, name); err != nil {
		return err
	}
	if err = updateConnection(ctx, tx, "UPDATE connections SET revision=revision+1 WHERE id=? AND revision=?", key, revision); err != nil {
		return err
	}
	return tx.Commit()
}

type ConnectionResult struct {
	mcpclient.Result
	ID         string `json:"id"`
	ReceivedAt int64  `json:"received_at"`
}

func (s *Store) CallConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any) (ConnectionResult, error) {
	return s.callConnectionTool(ctx, key, name, revision, args, "", 0)
}

func (s *Store) callConnectionTool(ctx context.Context, key, name string, revision int, args map[string]any, runID string, position int) (ConnectionResult, error) {
	if runID != "" {
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
	deadline, cancel := context.WithTimeout(ctx, mcpclient.Timeout)
	defer cancel()
	session, err := mcpclient.Connect(deadline, c.Config)
	if err != nil {
		return ConnectionResult{}, err
	}
	defer session.Close()
	catalog, err := mcpclient.Discover(deadline, session)
	if err != nil {
		return ConnectionResult{}, err
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
		return ConnectionResult{}, mcpclient.ErrSchemaChanged
	}
	callID := id()
	if err = s.dispatchConnectionCall(deadline, callID, key, granted, revision, runID, position); err != nil {
		return ConnectionResult{}, err
	}
	outcome, evidence, callErr := mcpclient.Call(deadline, session, granted, args, c.Config)
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

func (s *Store) ConnectionEvidence(ctx context.Context, callID string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT evidence FROM connection_calls
WHERE id=? AND status IN ('completed','tool_error') AND evidence IS NOT NULL`, callID).Scan(&content)
	return content, err
}

func (s *Store) dispatchConnectionCall(ctx context.Context, callID, key string, tool mcpclient.Tool, revision int, runID string, position int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if runID != "" {
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO connection_calls(id,connection_id,connection_revision,tool_name,tool_version,status,created_at)
SELECT ?,c.id,c.revision,?,?,'dispatched',? FROM connections c JOIN connection_tools t ON t.connection_id=c.id
WHERE c.id=? AND c.revision=? AND c.enabled=1 AND c.status='ready' AND t.name=? AND t.allowed=1`,
		callID, tool.Name, tool.Version, now(), key, revision, tool.Name)
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
