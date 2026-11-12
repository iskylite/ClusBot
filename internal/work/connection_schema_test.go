package work

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectionSchemaDiscoveryPermissionAndLegacyOutput(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "schema-contract", Version: "1"}, nil)
	calls := 0
	for _, name := range []string{"valid", "unsupported"} {
		input := json.RawMessage(`{"type":"object"}`)
		if name == "unsupported" {
			input = json.RawMessage(`{"type":"object","$schema":"http://json-schema.org/draft-04/schema#"}`)
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: input, OutputSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls++
			return &mcp.CallToolResult{StructuredContent: map[string]any{"count": "wrong-type"}}, nil
		})
	}
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	c, err := s.SaveConnection(t.Context(), "", Connection{Name: "schema", Enabled: true, Config: mcpclient.Config{Transport: "http", Endpoint: httpServer.URL}})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.TestConnection(t.Context(), c.ID, c.Revision)
	if err != nil || c.Status != "ready" {
		t.Fatalf("%+v %v", c, err)
	}
	var valid mcpclient.Tool
	for _, tool := range c.Tools {
		if tool.Name == "valid" {
			valid = tool
			continue
		}
		if tool.SchemaError == "" {
			t.Fatal("no incompatibility explanation")
		}
		if err = s.SetToolPermission(t.Context(), c.ID, tool.Name, tool.Version, c.Revision, true); !errors.Is(err, mcpclient.ErrToolSchema) {
			t.Fatalf("grant invalid: %v", err)
		}
		// Simulate a legacy allowed definition without schema_error metadata.
		tool.SchemaError = ""
		raw, _ := json.Marshal(tool)
		if _, err = s.db.Exec("UPDATE connection_tools SET definition=?,allowed=1 WHERE connection_id=? AND name=?", string(raw), c.ID, tool.Name); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SetToolPermission(t.Context(), c.ID, valid.Name, valid.Version, c.Revision, true); err != nil {
		t.Fatal(err)
	}
	c, err = s.Connection(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	options, err := s.TaskTools(t.Context())
	if err != nil || len(options) != 1 || options[0].Tool.Name != "valid" {
		t.Fatalf("invalid tool offered: %+v %v", options, err)
	}
	// Pre-fix rows omitted output_schema, but retained the full tool's version.
	valid.OutputSchema = nil
	raw, _ := json.Marshal(valid)
	if _, err = s.db.Exec("UPDATE connection_tools SET definition=? WHERE connection_id=? AND name=?", string(raw), c.ID, valid.Name); err != nil {
		t.Fatal(err)
	}
	result, err := s.CallConnectionTool(t.Context(), c.ID, valid.Name, c.Revision, map[string]any{})
	if err != nil || !result.IsError || result.ValidationError == "" || result.Reference == nil || calls != 1 {
		t.Fatalf("legacy output bypass: %+v calls=%d %v", result, calls, err)
	}
	var status string
	if err = s.db.QueryRow("SELECT status FROM connection_calls WHERE id=?", result.ID).Scan(&status); err != nil || status != "tool_error" {
		t.Fatalf("wrong call terminal %s %v", status, err)
	}
	if evidence, err := s.ConnectionEvidence(t.Context(), result.ID); err != nil || len(evidence) == 0 {
		t.Fatalf("missing invalid evidence: %v", err)
	}
	run, err := s.SubmitWithDelivery(t.Context(), "schema-task-read", "读取结果生成报告", "", "", true,
		Delivery{Read: &TaskReadRequest{ConnectionID: c.ID, Revision: c.Revision, ToolName: valid.Name, ToolVersion: valid.Version, Arguments: map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	read, err := s.runTaskRead(t.Context(), run.ID)
	if !errors.Is(err, errReadFailed) || read == nil || read.Status != "tool_error" || read.Result.ValidationError == "" || read.Result.Reference == nil {
		t.Fatalf("invalid output became task evidence: %+v %v", read, err)
	}
	if _, err = s.runTaskRead(t.Context(), run.ID); !errors.Is(err, ErrState) || calls != 2 {
		t.Fatalf("schema failure was replayed: calls=%d %v", calls, err)
	}
}
