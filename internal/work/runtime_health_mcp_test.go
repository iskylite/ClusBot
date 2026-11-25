package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const healthMCPTask = "mcp-nhc-001"

func healthMCPRow(task, node, health string) map[string]any {
	status, result, exit := "succeeded", 0, 0
	if health == mcpNHCFault {
		status, result, exit = "failed", 1, 1
	}
	if health == mcpNHCUnknown {
		status, result, exit = "failed", 1, 255
	}
	return map[string]any{"node": node, "state": status, "code": result,
		"data": map[string]any{"health": health, "source": "nhc", "exit_code": exit, "truncated": false, "cli_task_id": task}}
}

func healthMCPPayload(rows ...map[string]any) map[string]any {
	successful := 0
	for _, row := range rows {
		if row["state"] == "succeeded" {
			successful++
		}
	}
	state := "COMPLETE"
	if successful != len(rows) {
		state = "FAILED"
	}
	return map[string]any{"mode": "sync", "terminal": true, "task_id": healthMCPTask, "state": state, "total": len(rows),
		"succeeded": successful, "failed": len(rows) - successful, "canceled": 0,
		"command_results": append(rows, map[string]any{"node": "", "code": 0, "state": "",
			"data": map[string]any{"cli_summary": true, "cli_task_id": healthMCPTask}})}
}

func asHealthMCPResult(structured map[string]any, isError bool) ConnectionResult {
	p, _ := json.Marshal(structured)
	return ConnectionResult{ID: "call-1", Result: mcpclient.Result{Structured: p, IsError: isError}}
}

func TestParseMCPNHCExactTypedCoverage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modify   func(map[string]any, map[string]any)
		health   string
		envelope bool
		want     string
		complete bool
	}{
		{name: "synchronous typed healthy", health: mcpNHCHealthy, want: mcpNHCHealthy, complete: true},
		{name: "nonzero real NHC is a fault not provider success", health: mcpNHCFault, envelope: true, want: mcpNHCFault, complete: true},
		{name: "unreachable tool is unknown not an incident", health: mcpNHCUnknown, envelope: true, want: mcpNHCUnknown},
		{name: "MCP error cannot prove healthy", health: mcpNHCHealthy, envelope: true, want: mcpNHCUnknown},
		{name: "receipt is for another node", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { row["node"] = "node-2" }},
		{name: "old task reuse", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { row["data"].(map[string]any)["cli_task_id"] = "mcp-nhc-older" }},
		{name: "suppressed result words", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { p["command_results"] = []any{}; p["healthy"] = true }},
		{name: "clipped receipt", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { row["data"].(map[string]any)["truncated"] = true }},
		{name: "duplicate actual receipt", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { p["command_results"] = []map[string]any{row, row} }},
		{name: "missing counts", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { delete(p, "total") }},
		{name: "job accepted only", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { p["mode"] = "async"; p["terminal"] = false }},
		{name: "deduplication is not fresh evidence", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { p["deduplicated"] = true }},
		{name: "controller lost durability", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { p["manifest_error"] = "disk full" }},
		{name: "Slurm output is never health evidence", health: mcpNHCHealthy, want: mcpNHCUnknown, modify: func(p, row map[string]any) { row["data"] = map[string]any{"faults": []any{}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := healthMCPRow(healthMCPTask, "node-1", tc.health)
			payload := healthMCPPayload(row)
			if tc.modify != nil {
				tc.modify(payload, row)
			}
			out := parseMCPNHCResult(asHealthMCPResult(payload, tc.envelope), []string{"node-1"})
			if out.Nodes["node-1"] != tc.want || out.Complete != tc.complete {
				t.Fatalf("want %s/complete=%v got %+v", tc.want, tc.complete, out)
			}
		})
	}
	t.Run("retain actual partial fault but no unchecked coverage", func(t *testing.T) {
		fault := healthMCPRow(healthMCPTask, "node-1", mcpNHCFault)
		missing := healthMCPRow(healthMCPTask, "node-2", mcpNHCUnknown)
		payload := healthMCPPayload(fault, missing)
		out := parseMCPNHCResult(asHealthMCPResult(payload, true), []string{"node-1", "node-2"})
		if out.Complete || out.Nodes["node-1"] != mcpNHCFault || out.Nodes["node-2"] != mcpNHCUnknown {
			t.Fatalf("partial report %+v", out)
		}
	})
}

type healthMCPTestInput struct {
	Nodes          string `json:"nodes"`
	Sync           bool   `json:"sync"`
	IncludeStorage bool   `json:"include_storage,omitempty"`
	ExecuteTimeout int    `json:"execute_timeout"`
}

type healMCPTestInput struct {
	Nodes          string `json:"nodes"`
	Sync           bool   `json:"sync"`
	IdempotencyKey string `json:"idempotency_key"`
}

func TestHealthMCPChild(t *testing.T) {
	mode := os.Getenv("CLUSBOT_TEST_HEALTH_MCP")
	if mode == "" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test-health", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "nhc", Description: "Read target health", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in healthMCPTestInput) (*mcp.CallToolResult, any, error) {
			if !in.Sync || in.Nodes == "" || (mode == "read_health" && !in.IncludeStorage) ||
				(in.IncludeStorage && in.ExecuteTimeout != 480) || (!in.IncludeStorage && in.ExecuteTimeout != 300) {
				return nil, nil, errors.New("incorrect NHC client contract")
			}
			if mode == "read_stall" {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
			nodes := strings.Split(in.Nodes, ",")
			rows := make([]map[string]any, 0, len(nodes))
			for _, node := range nodes {
				health := mcpNHCHealthy
				switch mode {
				case "read_unhealthy":
					health = mcpNHCFault
				case "read_partial":
					health = mcpNHCFault
					if node == "node-2" {
						health = mcpNHCUnknown
					}
				}
				r := healthMCPRow(healthMCPTask, node, health)
				if mode == "read_archive" {
					d := r["data"].(map[string]any)
					d["stdout"] = strings.Repeat("x", 11000)
					d["stderr"] = strings.Repeat("y", 11000)
				}
				rows = append(rows, r)
			}
			payload := healthMCPPayload(rows...)
			ret := &mcp.CallToolResult{StructuredContent: payload, IsError: payload["state"] == "FAILED"}
			return ret, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "fault_check", Description: "SLURM only", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in healthMCPTestInput) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"healthy": true, "faults": []any{}, "state": "COMPLETE"}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "repair_test_service", Description: "Inert repair fixture"},
		func(_ context.Context, _ *mcp.CallToolRequest, in healMCPTestInput) (*mcp.CallToolResult, any, error) {
			if !in.Sync || in.Nodes != "node-1" || !strings.HasPrefix(in.IdempotencyKey, "self_heal:") {
				return nil, nil, errors.New("invalid repair fixture request")
			}
			if mode == "write_lost" {
				os.Exit(3)
			}
			const actionID = "mcp-repair-1"
			payload := map[string]any{"mode": "sync", "task_id": actionID, "terminal": true, "state": "COMPLETE",
				"total": 1, "succeeded": 1, "command_results": []map[string]any{{"node": in.Nodes, "state": "succeeded", "code": 0, "data": map[string]any{"cli_task_id": actionID}}}}
			switch mode {
			case "write_async":
				payload["mode"], payload["state"], payload["terminal"] = "async", "RUNNING", false
				delete(payload, "command_results")
			case "write_wrong_target":
				payload["command_results"].([]map[string]any)[0]["node"] = "node-2"
			case "write_empty":
				delete(payload, "command_results")
			}
			return &mcp.CallToolResult{StructuredContent: payload, IsError: mode == "write_error"}, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func setupMCPHealthFixture(t *testing.T, mode string) (*Store, Connection) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Setenv("CLUSBOT_TEST_HEALTH_MCP", mode)
	conn, err := store.SaveConnection(ctx, "", Connection{Name: "health-mock", Enabled: true,
		Config: mcpclient.Config{Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestHealthMCPChild$"}, Env: []string{"CLUSBOT_TEST_HEALTH_MCP"}}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = store.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for i := range conn.Tools {
		tool := conn.Tools[i]
		if err := store.SetToolPermissionWithMode(ctx, conn.ID, tool.Name, tool.Version, conn.Revision, true, tool.Name != "repair_test_service"); err != nil {
			t.Fatal(err)
		}
		conn, err = store.Connection(ctx, conn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, conn
}

func bindMCPHealthFixture(t *testing.T, s *Store, conn Connection, key, name, kind string) {
	t.Helper()
	_, err := s.UpsertCapabilityBinding(context.Background(), CapabilityBindingInput{
		CapabilityKey: key, Kind: kind, ExecutionMode: "sync", ConnectionID: conn.ID, MCPToolBinding: MCPToolBinding{StartTool: name},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertHealthCalls(t *testing.T, s *Store, tool string, expected int) {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM connection_calls WHERE tool_name=?", tool).Scan(&n); err != nil || n != expected {
		t.Fatalf("tool %s ran %d times, want %d: %v", tool, n, expected, err)
	}
}

func TestDynamicEffectCheckerUsesOnlyExplicitNHC(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		mode          string
		verifies      bool
		tool          string
		expectErr     bool
		fallbackCount int
	}{
		{"read_health", true, "nhc", false, 0},
		{"read_archive", true, "nhc", false, 0},
		{"read_unhealthy", false, "nhc", false, 0},
		{"read_partial", false, "nhc", true, 0},
		{"read_health", false, "fault_check", true, 0},
		{"read_health", true, "", false, 1},
	} {
		t.Run(c.mode+"/"+c.tool, func(t *testing.T) {
			s, conn := setupMCPHealthFixture(t, c.mode)
			if c.tool != "" {
				bindMCPHealthFixture(t, s, conn, "ops.effect_check", c.tool, "query")
			}
			fallback := 0
			verify := s.DynamicEffectChecker(func(context.Context, string) (bool, string, error) {
				fallback++
				return true, "configured independent HTTP check", nil
			})
			target := "node-1"
			if c.mode == "read_partial" {
				target = "node-2"
			}
			ok, evidence, err := verify(ctx, target)
			if ok != c.verifies || (err != nil) != c.expectErr || fallback != c.fallbackCount {
				t.Fatalf("verify=%v evidence=%q fallback=%d err=%v", ok, evidence, fallback, err)
			}
			if err == nil && strings.TrimSpace(evidence) == "" {
				t.Fatal("successful verdict has no proof")
			}
			nhcCalls := 0
			if c.tool == "nhc" {
				nhcCalls = 1
			}
			assertHealthCalls(t, s, "nhc", nhcCalls)
			assertHealthCalls(t, s, "fault_check", 0)
		})
	}
}

func TestFaultSyncPersistsOnlyExplicitNHCFailures(t *testing.T) {
	ctx := context.Background()
	s, conn := setupMCPHealthFixture(t, "read_partial")
	bindMCPHealthFixture(t, s, conn, "ops.fault_check", "nhc", "query")
	out, err := s.SyncFaultSegmentsFromMCP(ctx, "node-1,node-2")
	if !errors.Is(err, ErrHealthCoverage) || len(out) != 1 || out[0].ObjectRef != "node-1" || out[0].FaultKey != "nhc" {
		t.Fatalf("partial findings=%+v, err=%v", out, err)
	}
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE object_ref='node-2'`).Scan(&got); err != nil || got != 0 {
		t.Fatalf("unknown node incorrectly became a fault: %d/%v", got, err)
	}
	for _, input := range []string{"", "node-[1-2]", "node-1,node-1", "node-1,", "user@node-1"} {
		if _, err := s.SyncFaultSegmentsFromMCP(ctx, input); !errors.Is(err, ErrHealthCoverage) {
			t.Fatalf("unsafe scope %q not rejected: %v", input, err)
		}
	}
	assertHealthCalls(t, s, "nhc", 1)
}

func TestTrustedDomainMCPDeadlineBoundsAndCancels(t *testing.T) {
	ctx := context.Background()
	s, conn := setupMCPHealthFixture(t, "read_stall")
	// Warm the existing pooled connection; the deadline should exercise the
	// real CallTool path, not simply fail a slow subprocess startup.
	if _, err := s.acquireMCPSession(ctx, conn, conn.credentialBinding); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"nodes": "node-1", "sync": true, "execute_timeout": 300}
	for _, timeout := range []time.Duration{0, mcpclient.MaxDomainCall + time.Second} {
		if _, err := s.callTrustedDomainConnectionTool(ctx, conn.ID, "nhc", conn.Revision, args, timeout); !errors.Is(err, mcpclient.ErrConfig) {
			t.Fatalf("unbounded timeout %s admitted: %v", timeout, err)
		}
	}
	start := time.Now()
	if _, err := s.callTrustedDomainConnectionTool(ctx, conn.ID, "nhc", conn.Revision, args, 250*time.Millisecond); !errors.Is(err, mcpclient.ErrCall) {
		t.Fatalf("stalled call did not return unknown: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("canceled call blocked for %s", elapsed)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM connection_calls WHERE tool_name='nhc'`).Scan(&status); err != nil || status != "unknown" {
		t.Fatalf("canceled call falsely confirmed: %s %v", status, err)
	}
}
