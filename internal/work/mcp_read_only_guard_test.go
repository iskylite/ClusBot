package work

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestMCPWriteToolsBlockedFromAutomaticTaskTools(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mcp-ro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	conn, err := s.SaveConnection(ctx, "", Connection{
		Name: "cluskit2", Enabled: true,
		Config: mcpclient.Config{Transport: "stdio", Command: "/bin/echo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE connections SET status='ready', credential_binding='bind-1', protocol='stdio' WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}

	insertTool := func(name string, allowed, readOnly bool) {
		t.Helper()
		tool := mcpclient.Tool{
			Name:        name,
			Description: "tool " + name,
			Version:     "v1",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		}
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only) VALUES(?,?,?,?,?)`,
			conn.ID, name, string(raw), allowed, readOnly); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate Cluskit2 write tools even if allowed=1 (and even if someone forced read_only=1 in SQL).
	for _, writeTool := range []string{"run", "tree_run", "star_run", "copy", "setssh", "setpwd", "bench_run", "bench_install", "bench_save", "task_cancel"} {
		insertTool(writeTool, true, false)
	}
	// Simulate an allowed read-only tool.
	insertTool("task_status", true, true)
	// Simulate an allowed tool without explicit read_only flag (fail-closed).
	insertTool("unknown_tool", true, false)

	// 1. TaskTools must expose ONLY task_status.
	options, err := s.TaskTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].Tool.Name != "task_status" || !options[0].Tool.ReadOnly {
		t.Fatalf("TaskTools leaked non-readonly tools: %+v", options)
	}

	// 2. automaticTaskTools for an ordinary run must expose ONLY workspace tools + task_status.
	run, err := s.Submit(ctx, "ro-guard-run", "check node status", "cluster", "", true)
	if err != nil {
		t.Fatal(err)
	}
	autoTools, err := s.automaticTaskTools(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	var mcpNames []string
	for _, tl := range autoTools {
		if tl.source.request.ConnectionID != "" {
			mcpNames = append(mcpNames, tl.source.ToolName)
		}
	}
	if len(mcpNames) != 1 || mcpNames[0] != "task_status" {
		t.Fatalf("automaticTaskTools leaked write/unverified MCP tools: %v", mcpNames)
	}

	// 3. SetToolPermissionWithMode must reject setting read_only=true on known write tools.
	for _, writeTool := range []string{"run", "setpwd", "bench_run"} {
		err := s.SetToolPermissionWithMode(ctx, conn.ID, writeTool, "v1", conn.Revision, true, true)
		if !errors.Is(err, ErrToolPermission) {
			t.Fatalf("SetToolPermissionWithMode(%s, readOnly=true) succeeded or wrong error: %v", writeTool, err)
		}
	}
}
