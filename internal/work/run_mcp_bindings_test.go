package work

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestRunMCPBindingsDoNotConnectOrExposePayload(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "run-mcp-view.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, envName, endpoint = "user:session", "MCP_TEST_PRIVATE_ENV", "https://never-connect.invalid/private-path"
	t.Setenv(envName, "private-initial-key")
	cfg := mcpclient.Config{Transport: "http", Endpoint: endpoint, TokenEnv: envName}
	conn, err := s.SaveConnection(ctx, "", Connection{Name: "Fixture", Enabled: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cfg.CredentialBinding(s.connectionKey)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE connections SET status='ready',credential_binding=? WHERE id=?", credential, conn.ID)
	tool := mcpclient.Tool{Name: "fixture-read", Version: "v1", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}
	definition, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only) VALUES(?,?,?,1,1)", conn.ID, tool.Name, string(definition))
	unbound, err := s.Submit(ctx, "unbound", "summarize", "reports", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// A local read has no connection ID; it is not an MCP binding.
	exec("INSERT INTO run_reads(run_id,position,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments) VALUES(?,1,0,'local','read','read','1','{}')", unbound.ID)
	if got, err := s.ReadRunMCPBindings(ctx, actor, unbound.ID); err != nil || got.Bindings == nil || len(got.Bindings) != 0 {
		t.Fatalf("unselected catalog or local tool projected: %+v %v", got, err)
	}

	run, err := s.SubmitWithDelivery(ctx, "bound", "summarize", "reports", "", true, Delivery{Read: &TaskReadRequest{
		ConnectionID: conn.ID, Revision: conn.Revision, ToolName: tool.Name, ToolVersion: tool.Version,
		Arguments: map[string]any{"query": "private-arguments"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(wantValidity, wantLifecycle string) RunMCPReadBinding {
		t.Helper()
		got, err := s.ReadRunMCPBindings(ctx, actor, run.ID)
		if err != nil || got.RunID != run.ID || len(got.Bindings) != 1 {
			t.Fatalf("invalid binding: %+v %v", got, err)
		}
		item := got.Bindings[0]
		if item.LocalValidity != wantValidity || item.Lifecycle != wantLifecycle || item.RemoteObservation != "not_checked" ||
			item.ConnectionID != conn.ID || item.ToolName != tool.Name || item.ToolVersion != tool.Version {
			t.Fatalf("wrong projected binding: %+v", item)
		}
		body, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{endpoint, envName, credential, "private-initial-key", "private-arguments", "private-response"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("leaked binding internals: %s", body)
			}
		}
		return item
	}
	if got := read("locally_ready", "current_run"); got.Selection != "explicit" || got.ReadStatus != "queued" {
		t.Fatalf("ready binding not selected: %+v", got)
	}
	if _, err := s.ReadRunMCPBindings(ctx, "foreign", run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign binding not masked: %v", err)
	}
	t.Setenv(envName, "changed-key")
	read("credential_binding_changed", "current_run")
	exec("UPDATE connections SET revision=revision+1 WHERE id=?", conn.ID)
	read("connection_revision_changed", "current_run")
	exec("UPDATE connection_tools SET allowed=0 WHERE connection_id=?", conn.ID)
	read("tool_not_authorized", "current_run")
	exec("UPDATE run_reads SET status='reused',result=? WHERE run_id=?", `{"text":"private-response"}`, run.ID)
	if got := read("tool_not_authorized", "historical"); got.Selection != "reused_result" {
		t.Fatalf("reused result presented as new selection: %+v", got)
	}
	exec("UPDATE connections SET enabled=0 WHERE id=?", conn.ID)
	read("connection_unavailable", "historical")
}
