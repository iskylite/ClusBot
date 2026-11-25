package work

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHelperCluskit2MCPProcess runs a minimal stdio MCP server when invoked as a subprocess.
func TestHelperCluskit2MCPProcess(t *testing.T) {
	if os.Getenv("CLUSBOT_TEST_MCP_HELPER") != "enabled" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "cluskit2-mock", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "discover",
		Description: "Discover cluster nodes",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		Nodes string `json:"nodes,omitempty"`
		Sync  bool   `json:"sync,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		payload, _ := json.Marshal(map[string]any{
			"pid": os.Getpid(),
			"summary": map[string]any{
				"observations": []map[string]any{
					{"host": "10.0.0.11", "serial": "SN-1011"},
				},
			},
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run",
		Description: "Execute remote SSH command",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"ok":true}`}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func boolPtr(v bool) *bool { return &v }

func TestAnnotationGuardrailAndStdioSessionPooling(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/clusbot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.SaveConnection(ctx, "", Connection{
		Name:    "cluskit2",
		Enabled: true,
		Config: mcpclient.Config{
			Transport: "stdio",
			Command:   os.Args[0],
			Args:      []string{"-test.run=^TestHelperCluskit2MCPProcess$"},
			Env:       []string{"CLUSBOT_TEST_MCP_HELPER"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUSBOT_TEST_MCP_HELPER", "enabled")

	probed, err := store.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}

	var discoverTool, runTool mcpclient.Tool
	for _, tool := range probed.Tools {
		switch tool.Name {
		case "discover":
			discoverTool = tool
		case "run":
			runTool = tool
		}
	}
	if discoverTool.Name == "" || runTool.Name == "" {
		t.Fatalf("expected discover and run tools, got %+v", probed.Tools)
	}
	if !runTool.Destructive || runTool.ServerReadOnly == nil || *runTool.ServerReadOnly {
		t.Fatalf("expected run tool to carry destructive/non-readonly annotations, got %+v", runTool)
	}

	// Guardrail: `run` cannot be marked read_only even if requested explicitly.
	if err := store.SetToolPermissionWithMode(ctx, probed.ID, "run", runTool.Version, probed.Revision, true, true); err == nil {
		t.Fatalf("expected SetToolPermissionWithMode to reject read_only=true on destructive tool run")
	}
	if err := store.SetToolPermissionWithMode(ctx, probed.ID, "discover", discoverTool.Version, probed.Revision, true, true); err != nil {
		t.Fatalf("expected discover read_only permission to succeed: %v", err)
	}
	updated, err := store.Connection(ctx, probed.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Session pooling: two consecutive stdio calls should reuse the same child PID.
	res1, err := store.CallConnectionTool(ctx, updated.ID, "discover", updated.Revision, map[string]any{"nodes": "10.0.0.11", "sync": true})
	if err != nil {
		t.Fatalf("first discover call failed: %v", err)
	}
	res2, err := store.CallConnectionTool(ctx, updated.ID, "discover", updated.Revision, map[string]any{"nodes": "10.0.0.11", "sync": true})
	if err != nil {
		t.Fatalf("second discover call failed: %v", err)
	}
	var p1, p2 struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(res1.Text), &p1); err != nil || p1.PID == 0 {
		t.Fatalf("failed to parse pid from res1 %q: %v", res1.Text, err)
	}
	if err := json.Unmarshal([]byte(res2.Text), &p2); err != nil || p2.PID == 0 {
		t.Fatalf("failed to parse pid from res2 %q: %v", res2.Text, err)
	}
	if p1.PID != p2.PID {
		t.Fatalf("expected pooled stdio PID reuse (%d == %d)", p1.PID, p2.PID)
	}

	// DynamicDiscoverySource parses Cluskit2 summary.observations directly.
	src := store.DynamicDiscoverySource(nil)
	if !src.AllowsDiscovery("cluskit2", "10.0.0.11") {
		t.Fatalf("expected DynamicDiscoverySource to allow cluskit2")
	}
	obs, err := src.ScanDiscovery(ctx, "cluskit2", "10.0.0.11")
	if err != nil {
		t.Fatalf("ScanDiscovery failed: %v", err)
	}
	if len(obs) != 1 || obs[0].Address != "10.0.0.11" || obs[0].Serial != "SN-1011" {
		t.Fatalf("unexpected discovery observations: %+v", obs)
	}
}

func TestCluskit2BenchmarkProviderAdaptation(t *testing.T) {
	var calls atomic.Int32
	var seenSuite, seenNodes string
	var seenFanout int
	var seenForbiddenHosts bool
	lookupNotFound := false
	caller := func(ctx context.Context, toolName string, args map[string]any) (mcpclient.Result, error) {
		calls.Add(1)
		switch toolName {
		case "bench_run":
			if _, bad := args["hosts"]; bad {
				seenForbiddenHosts = true
			}
			seenSuite, _ = args["suite"].(string)
			seenNodes, _ = args["nodes"].(string)
			seenFanout, _ = args["fanout"].(int)
			return mcpclient.Result{Text: `{"task_id":"task-99"}`}, nil
		case "task_status":
			if _, ok := args["idempotency_key"]; ok {
				if lookupNotFound {
					return mcpclient.Result{Text: `{"found":false,"state":"NOT_FOUND"}`}, nil
				}
				return mcpclient.Result{Text: `{"found":true,"task_id":"task-99","state":"RUNNING"}`}, nil
			}
			return mcpclient.Result{Text: `{"task_id":"task-99","state":"COMPLETE","summary":{"passed":2,"bw_gbps":1000},"results":[
			  {"task_id":"task-99","node":"node-1","event":"result","state":"succeeded","code":0,"data":{"benchmark":"hpl","metrics":{"Rmax":195.4,"Unit":"GFLOPS"},"row":{"node":"node-1","benchmark":"hpl","status":"succeeded","metrics":{"unit":"GFLOPS","fields":{"Rmax":195.4}}}}},
			  {"task_id":"task-99","node":"node-2","event":"result","state":"succeeded","code":0,"data":{"benchmark":"hpl","metrics":{"Rmax":170.2,"Unit":"GFLOPS"},"row":{"node":"node-2","benchmark":"hpl","status":"succeeded","metrics":{"unit":"GFLOPS","fields":{"Rmax":170.2}}}}}
			]}`}, nil
		case "task_cancel":
			return mcpclient.Result{Text: `{"task_ids":["task-99"],"acknowledged":true,"confirmed":["engine:task-99"],"unconfirmed":[],"remote_exit_verified":false}`}, nil
		default:
			return mcpclient.Result{IsError: true, Text: "unknown tool"}, nil
		}
	}

	prov, err := NewMCPBenchmarkProvider("cluskit2", 1, MCPToolBinding{
		StartTool:  "bench_run",
		LookupTool: "task_status",
		GetTool:    "task_status",
		CancelTool: "task_cancel",
	}, caller)
	if err != nil {
		t.Fatal(err)
	}
	req := BenchmarkJobRequest{
		Binding:        BenchmarkBinding{Connection: "cluskit2", Revision: 1, Tool: "bench_run", Validator: benchmarkValidatorV1},
		IdempotencyKey: "idem-bench-1",
		Case:           "gpu",
		Targets:        []string{"node-1", "node-2"},
		Parameters:     map[string]string{"suite": "gpu", "fanout": "16"},
	}
	jobID, err := prov.Start(context.Background(), req)
	if err != nil || jobID != "task-99" {
		t.Fatalf("Start failed: jobID=%q err=%v", jobID, err)
	}
	if seenForbiddenHosts {
		t.Fatalf("bench_run must not send unknown 'hosts' parameter")
	}
	if seenSuite != "gpu" || !strings.Contains(seenNodes, "node-1,node-2") || seenFanout != 16 {
		t.Fatalf("expected Cluskit2 suite/nodes/fanout mapping, got suite=%q nodes=%q fanout=%d", seenSuite, seenNodes, seenFanout)
	}
	lookupNotFound = true
	missingID, err := prov.Lookup(context.Background(), req)
	if err != nil || missingID != "" {
		t.Fatalf("expected NOT_FOUND lookup to return empty id with nil err, got id=%q err=%v", missingID, err)
	}
	lookupNotFound = false
	lookedUp, err := prov.Lookup(context.Background(), req)
	if err != nil || lookedUp != "task-99" {
		t.Fatalf("Lookup failed: %q err=%v", lookedUp, err)
	}
	res, err := prov.Get(context.Background(), req, jobID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if res.State != "completed" || len(res.Measurements) != 2 || res.Measurements[0].Metrics["Rmax"] != (BenchmarkMeasuredMetric{Value: 195.4, Unit: "GFLOPS"}) || res.Measurements[1].Metrics["Rmax"].Value != 170.2 || res.Measurements[0].Outcome != "measured" || res.Measurements[0].Raw != nil {
		t.Fatalf("unexpected BenchmarkJobResult: %+v", res)
	}
	if err := prov.Cancel(context.Background(), req, jobID); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("expected 5 MCP calls, got %d", calls.Load())
	}
}
