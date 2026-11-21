package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"clusbot/internal/mcpclient"
)

type RunMCPBindings struct {
	RunID        string              `json:"run_id"`
	TaskID       string              `json:"task_id,omitempty"`
	RunStatus    string              `json:"run_status"`
	AutomaticPin *RunMCPAutomaticPin `json:"automatic_pin,omitempty"`
	Bindings     []RunMCPReadBinding `json:"bindings"`
}

type RunMCPAutomaticPin struct {
	MCPReadSelection
	LocalValidity string `json:"local_validity"`
}

type RunMCPReadBinding struct {
	Position           int    `json:"position"`
	ConnectionID       string `json:"connection_id"`
	ConnectionName     string `json:"connection_name"`
	ConnectionRevision int    `json:"connection_revision"`
	ToolName           string `json:"tool_name"`
	ToolVersion        string `json:"tool_version"`
	Selection          string `json:"selection"`
	ReadStatus         string `json:"read_status"`
	Lifecycle          string `json:"lifecycle"`
	LocalValidity      string `json:"local_validity"`
	RemoteObservation  string `json:"remote_observation"`
}

// ReadRunMCPBindings reports actual persisted selections, including retained
// prior reads, without granting permission or reading arguments or results.
// Runs share the same existing authenticated, local session scope as Skills.
func (s *Store) ReadRunMCPBindings(ctx context.Context, actor, runID string) (RunMCPBindings, error) {
	if actor != "user:session" {
		allowed, err := ownsPersonalBusiness(s.db, actor)
		if err != nil {
			return RunMCPBindings{}, err
		}
		if !allowed {
			return RunMCPBindings{}, sql.ErrNoRows
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunMCPBindings{}, err
	}
	defer tx.Rollback()
	out := RunMCPBindings{Bindings: []RunMCPReadBinding{}}
	var control string
	if err := tx.QueryRowContext(ctx, "SELECT id,coalesce(task_id,''),status,control FROM runs WHERE id=?", runID).
		Scan(&out.RunID, &out.TaskID, &out.RunStatus, &control); err != nil {
		return RunMCPBindings{}, err
	}
	var pin RunMCPAutomaticPin
	err = tx.QueryRowContext(ctx, `SELECT connection_id,connection_revision,tool_name,tool_version
FROM run_mcp_selections WHERE run_id=?`, runID).
		Scan(&pin.ConnectionID, &pin.Revision, &pin.ToolName, &pin.ToolVersion)
	if err == nil {
		pin.LocalValidity = "locally_ready"
		if _, err := s.selectedMCPOption(ctx, tx, pin.MCPReadSelection); errors.Is(err, ErrToolPermission) {
			pin.LocalValidity = "unavailable"
		} else if err != nil {
			return RunMCPBindings{}, err
		}
		out.AutomaticPin = &pin
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RunMCPBindings{}, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT r.position,r.connection_id,r.connection_name,r.connection_revision,r.tool_name,r.tool_version,
r.status,r.model_round IS NOT NULL,
c.id,c.revision,c.enabled,c.status,c.config,c.credential_binding,t.definition,t.allowed
FROM run_reads r LEFT JOIN connections c ON c.id=r.connection_id
LEFT JOIN connection_tools t ON t.connection_id=c.id AND t.name=r.tool_name
WHERE r.run_id=? AND coalesce(r.connection_id,'')<>'' ORDER BY r.position`, runID)
	if err != nil {
		return RunMCPBindings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var b RunMCPReadBinding
		var automatic bool
		var id, status, rawConfig, credential, definition sql.NullString
		var revision sql.NullInt64
		var enabled, allowed sql.NullBool
		if err := rows.Scan(&b.Position, &b.ConnectionID, &b.ConnectionName, &b.ConnectionRevision, &b.ToolName, &b.ToolVersion,
			&b.ReadStatus, &automatic, &id, &revision, &enabled, &status, &rawConfig, &credential, &definition, &allowed); err != nil {
			return RunMCPBindings{}, err
		}
		b.Selection, b.Lifecycle, b.RemoteObservation = "explicit", "historical", "not_checked"
		switch {
		case b.ReadStatus == "reused":
			b.Selection = "reused_result"
		case automatic:
			b.Selection = "model_selected"
		}
		if control == "" && (out.RunStatus == "queued" || out.RunStatus == "running") &&
			(b.ReadStatus == "queued" || b.ReadStatus == "checking") {
			b.Lifecycle = "current_run"
		}
		switch {
		case !id.Valid || !enabled.Bool || status.String != "ready":
			b.LocalValidity = "connection_unavailable"
		case !allowed.Bool:
			b.LocalValidity = "tool_not_authorized"
		case revision.Int64 != int64(b.ConnectionRevision):
			b.LocalValidity = "connection_revision_changed"
		default:
			var tool mcpclient.Tool
			var cfg mcpclient.Config
			if json.Unmarshal([]byte(definition.String), &tool) != nil || tool.Name != b.ToolName ||
				tool.Version != b.ToolVersion || mcpclient.ValidateToolSchemas(tool) != nil {
				b.LocalValidity = "tool_definition_changed"
			} else if json.Unmarshal([]byte(rawConfig.String), &cfg) != nil || cfg.Validate() != nil {
				b.LocalValidity = "connection_unavailable"
			} else if current, err := cfg.CredentialBinding(s.connectionKey); err != nil || current != credential.String {
				b.LocalValidity = "credential_binding_changed"
			} else {
				b.LocalValidity = "locally_ready"
			}
		}
		out.Bindings = append(out.Bindings, b)
	}
	return out, rows.Err()
}
