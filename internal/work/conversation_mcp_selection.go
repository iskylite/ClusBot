package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"clusbot/internal/mcpclient"
)

func migrateConversationMCPSelection(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conversation_mcp_selections (
 conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
 connection_id TEXT NOT NULL, connection_revision INTEGER NOT NULL, tool_name TEXT NOT NULL, tool_version TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS run_mcp_selections (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 connection_id TEXT NOT NULL, connection_revision INTEGER NOT NULL, tool_name TEXT NOT NULL, tool_version TEXT NOT NULL
)`)
	return err
}

type MCPReadSelection struct {
	ConnectionID string `json:"connection_id"`
	Revision     int    `json:"connection_revision"`
	ToolName     string `json:"tool_name"`
	ToolVersion  string `json:"tool_version"`
}

type ConversationMCPInput struct {
	Action string `json:"action"`
	MCPReadSelection
}

type ConversationMCPSelection struct {
	ConversationID string            `json:"conversation_id"`
	Selection      *MCPReadSelection `json:"selection"`
	LocalValidity  string            `json:"local_validity,omitempty"`
}

func (s *Store) selectedMCPOption(ctx context.Context, tx *sql.Tx, sel MCPReadSelection) (TaskToolOption, error) {
	var out TaskToolOption
	var raw, config, credential, state string
	var allowed, enabled bool
	err := tx.QueryRowContext(ctx, `SELECT c.name,c.revision,c.enabled,c.status,c.config,c.credential_binding,t.definition,t.allowed
FROM connections c JOIN connection_tools t ON t.connection_id=c.id WHERE c.id=? AND t.name=?
AND EXISTS(SELECT 1 FROM (
 SELECT c0.id,t0.name FROM connections c0 JOIN connection_tools t0 ON t0.connection_id=c0.id
 WHERE c0.enabled=1 AND c0.status='ready' AND t0.allowed=1 ORDER BY c0.name,t0.name LIMIT 100
) catalog WHERE catalog.id=c.id AND catalog.name=t.name)`, sel.ConnectionID, sel.ToolName).
		Scan(&out.ConnectionName, &out.Revision, &enabled, &state, &config, &credential, &raw, &allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskToolOption{}, ErrToolPermission
	}
	if err != nil {
		return TaskToolOption{}, err
	}
	if !allowed || !enabled || state != "ready" || out.Revision != sel.Revision {
		return TaskToolOption{}, ErrToolPermission
	}
	if json.Unmarshal([]byte(raw), &out.Tool) != nil || out.Tool.Name != sel.ToolName || out.Tool.Version != sel.ToolVersion ||
		mcpclient.ValidateToolSchemas(out.Tool) != nil {
		return TaskToolOption{}, ErrToolPermission
	}
	var cfg mcpclient.Config
	if json.Unmarshal([]byte(config), &cfg) != nil || cfg.Validate() != nil {
		return TaskToolOption{}, ErrToolPermission
	}
	if current, err := cfg.CredentialBinding(s.connectionKey); err != nil || current != credential {
		return TaskToolOption{}, ErrToolPermission
	}
	out.ConnectionID, out.Tool.Allowed = sel.ConnectionID, true
	return out, nil
}

func (s *Store) readConversationMCPSelection(ctx context.Context, tx *sql.Tx, conversation string) (ConversationMCPSelection, error) {
	out := ConversationMCPSelection{ConversationID: conversation}
	var sel MCPReadSelection
	err := tx.QueryRowContext(ctx, `SELECT connection_id,connection_revision,tool_name,tool_version
FROM conversation_mcp_selections WHERE conversation_id=?`, conversation).Scan(&sel.ConnectionID, &sel.Revision, &sel.ToolName, &sel.ToolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return ConversationMCPSelection{}, err
	}
	out.Selection, out.LocalValidity = &sel, "locally_ready"
	if _, err := s.selectedMCPOption(ctx, tx, sel); errors.Is(err, ErrToolPermission) {
		out.LocalValidity = "unavailable"
	} else if err != nil {
		return ConversationMCPSelection{}, err
	}
	return out, nil
}

func (s *Store) ReadConversationMCPSelection(ctx context.Context, actor, conversation string) (ConversationMCPSelection, error) {
	if actor != "user:session" {
		return ConversationMCPSelection{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationMCPSelection{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM conversations WHERE id=?", conversation).Scan(&id); err != nil {
		return ConversationMCPSelection{}, err
	}
	return s.readConversationMCPSelection(ctx, tx, id)
}

func (s *Store) SetConversationMCPSelection(ctx context.Context, actor, conversation string, in ConversationMCPInput) (ConversationMCPSelection, error) {
	if actor != "user:session" {
		return ConversationMCPSelection{}, sql.ErrNoRows
	}
	sel := in.MCPReadSelection
	if in.Action == "clear_mcp_read" {
		if sel != (MCPReadSelection{}) {
			return ConversationMCPSelection{}, ErrReadSelection
		}
	} else if in.Action != "bind_mcp_read" || sel.ConnectionID == "" || sel.Revision < 1 ||
		sel.ToolName == "" || len(sel.ToolName) > 200 || !validBenchmarkSHA(sel.ToolVersion) {
		return ConversationMCPSelection{}, ErrReadSelection
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationMCPSelection{}, err
	}
	defer tx.Rollback()
	if err := checkConversationSubmission(tx, conversation); err != nil {
		return ConversationMCPSelection{}, err
	}
	if in.Action == "clear_mcp_read" {
		_, err = tx.ExecContext(ctx, "DELETE FROM conversation_mcp_selections WHERE conversation_id=?", conversation)
	} else {
		if _, err := s.selectedMCPOption(ctx, tx, sel); err != nil {
			return ConversationMCPSelection{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_mcp_selections VALUES(?,?,?,?,?)
ON CONFLICT(conversation_id) DO UPDATE SET connection_id=excluded.connection_id,connection_revision=excluded.connection_revision,
tool_name=excluded.tool_name,tool_version=excluded.tool_version`, conversation, sel.ConnectionID, sel.Revision, sel.ToolName, sel.ToolVersion)
	}
	if err != nil {
		return ConversationMCPSelection{}, err
	}
	out, err := s.readConversationMCPSelection(ctx, tx, conversation)
	if err != nil {
		return ConversationMCPSelection{}, err
	}
	return out, tx.Commit()
}

// automaticMCPOptions pins the selected tool for an actual task run. An
// invalid pin blocks the tool round, never falls back to the global catalog.
func (s *Store) automaticMCPOptions(ctx context.Context, run Run) ([]TaskToolOption, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if run.Generation == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_mcp_selections
SELECT r.id,b.connection_id,b.connection_revision,b.tool_name,b.tool_version
FROM runs r JOIN conversation_mcp_selections b ON b.conversation_id=r.conversation_id
WHERE r.id=? AND r.task_id IS NOT NULL`, run.ID); err != nil {
			return nil, err
		}
	}
	var sel MCPReadSelection
	err = tx.QueryRowContext(ctx, `SELECT connection_id,connection_revision,tool_name,tool_version FROM run_mcp_selections WHERE run_id=?`, run.ID).
		Scan(&sel.ConnectionID, &sel.Revision, &sel.ToolName, &sel.ToolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return s.TaskTools(ctx)
	}
	if err != nil {
		return nil, err
	}
	option, err := s.selectedMCPOption(ctx, tx, sel)
	if errors.Is(err, ErrToolPermission) {
		// Remember the invalid selection as well; otherwise resuming this
		// same run after clearing it would incorrectly widen to all tools.
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrReadCheckpoint
	}
	if err != nil {
		return nil, err
	}
	return []TaskToolOption{option}, tx.Commit()
}

// Recheck immediately before another model round, not just when a run is
// prepared, so persisted selections cannot retain removed permissions.
func (s *Store) checkAutomaticMCPPin(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sel MCPReadSelection
	err = tx.QueryRowContext(ctx, `SELECT connection_id,connection_revision,tool_name,tool_version
FROM run_mcp_selections WHERE run_id=?`, runID).Scan(&sel.ConnectionID, &sel.Revision, &sel.ToolName, &sel.ToolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.selectedMCPOption(ctx, tx, sel)
	if errors.Is(err, ErrToolPermission) {
		return ErrReadCheckpoint
	}
	return err
}
