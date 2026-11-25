package work

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestCluskit2RealBinariesE2E(t *testing.T) {
	cluskitDir := "/Users/iskylite/Mine/02-Code/00-Codes/Cluskit2-MCP"
	if _, err := os.Stat(filepath.Join(cluskitDir, "go.mod")); err != nil {
		t.Skipf("Cluskit2-MCP repo not found at %s: %v", cluskitDir, err)
	}

	tmpDir := t.TempDir()
	buildBin := func(pkg, name string) string {
		out := filepath.Join(tmpDir, name)
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", out, pkg)
		cmd.Dir = cluskitDir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s failed: %v\n%s", pkg, err, string(b))
		}
		return out
	}

	clusconfBin := buildBin("./cmd/clusconf", "clusconf")
	clusbenchBin := buildBin("./cmd/clusbench", "clusbench")
	clusopsBin := buildBin("./cmd/clusops", "clusops")

	ctx := context.Background()
	store, err := Open(filepath.Join(tmpDir, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 1. Live stdio E2E against real `clusconf mcp` (runs as non-root).
	confConn, err := store.SaveConnection(ctx, "", Connection{
		Name:    "real-clusconf",
		Enabled: true,
		Config: mcpclient.Config{
			Transport: "stdio",
			Command:   clusconfBin,
			Args:      []string{"mcp"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	probedConf, err := store.TestConnection(ctx, confConn.ID, confConn.Revision)
	if err != nil {
		t.Fatalf("TestConnection(clusconf mcp) failed: %v", err)
	}
	confTools := make(map[string]mcpclient.Tool, len(probedConf.Tools))
	for _, tool := range probedConf.Tools {
		confTools[tool.Name] = tool
	}
	discoverTool, ok := confTools["discover"]
	if !ok {
		t.Fatalf("clusconf mcp missing discover tool")
	}
	if discoverTool.ServerReadOnly == nil || !*discoverTool.ServerReadOnly {
		t.Fatalf("expected real discover readOnlyHint=true, got %+v", discoverTool.ServerReadOnly)
	}
	if err := mcpclient.ValidateArguments(discoverTool, map[string]any{"hosts": "10.0.0.1"}); err == nil {
		t.Fatalf("expected real discover schema to reject 'hosts'")
	}
	if err := mcpclient.ValidateArguments(discoverTool, map[string]any{"nodes": "10.0.0.1", "sync": true}); err != nil {
		t.Fatalf("expected real discover schema to accept nodes+sync: %v", err)
	}
	if runTool, ok := confTools["run"]; ok {
		if runTool.ServerReadOnly == nil || *runTool.ServerReadOnly {
			t.Fatalf("expected real run tool readOnlyHint=false, got %+v", runTool.ServerReadOnly)
		}
		if err := store.SetToolPermissionWithMode(ctx, probedConf.ID, "run", runTool.Version, probedConf.Revision, true, true); err == nil {
			t.Fatalf("expected read_only=true on real clusconf run tool to be rejected")
		}
	}
	taskStatusTool, ok := confTools["task_status"]
	if !ok {
		t.Fatalf("clusconf mcp missing task_status tool")
	}
	if err := store.SetToolPermissionWithMode(ctx, probedConf.ID, "task_status", taskStatusTool.Version, probedConf.Revision, true, true); err != nil {
		t.Fatalf("grant task_status failed: %v", err)
	}
	probedConf, _ = store.Connection(ctx, probedConf.ID)
	// Call real clusconf task_status with unknown idempotency_key -> verify NOT_FOUND contract.
	statusRes, err := store.CallConnectionTool(ctx, probedConf.ID, "task_status", probedConf.Revision, map[string]any{
		"idempotency_key": "nonexistent-e2e-key",
		"tool":            "discover",
	})
	if err != nil || statusRes.IsError {
		t.Fatalf("real clusconf task_status lookup failed: res=%+v err=%v", statusRes, err)
	}
	if _, err := store.UpsertCapabilityBinding(ctx, CapabilityBindingInput{
		CapabilityKey:  "benchmark.run",
		Version:        1,
		Kind:           "action",
		ExecutionMode:  "async",
		ConnectionID:   probedConf.ID,
		MCPToolBinding: MCPToolBinding{StartTool: "task_status", LookupTool: "task_status", GetTool: "task_status"},
	}); err != nil {
		t.Fatalf("UpsertCapabilityBinding failed: %v", err)
	}
	dynBench := store.DynamicBenchmarkProvider(nil)
	foundID, err := dynBench.Lookup(ctx, BenchmarkJobRequest{
		Binding:        BenchmarkBinding{Connection: "real-clusconf", Revision: probedConf.Revision, Tool: "task_status", Validator: benchmarkValidatorV1},
		IdempotencyKey: "nonexistent-e2e-key",
	})
	if err != nil || foundID != "" {
		t.Fatalf("expected NOT_FOUND lookup to return \"\" with nil err, got id=%q err=%v", foundID, err)
	}

	// 2. Real `clusbench mcp` and `clusops mcp` require root at process startup; run live if euid==0,
	// otherwise verify their in-process MCP server tests in Cluskit2-MCP pass.
	if os.Geteuid() == 0 {
		benchConn, err := store.SaveConnection(ctx, "", Connection{
			Name:    "real-clusbench",
			Enabled: true,
			Config:  mcpclient.Config{Transport: "stdio", Command: clusbenchBin, Args: []string{"mcp"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		probedBench, err := store.TestConnection(ctx, benchConn.ID, benchConn.Revision)
		if err != nil {
			t.Fatalf("TestConnection(clusbench mcp) failed: %v", err)
		}
		var benchRunTool mcpclient.Tool
		for _, tl := range probedBench.Tools {
			if tl.Name == "bench_run" {
				benchRunTool = tl
				break
			}
		}
		if benchRunTool.Name == "" {
			t.Fatalf("clusbench mcp missing bench_run")
		}
		if err := mcpclient.ValidateArguments(benchRunTool, map[string]any{"hosts": "node-1"}); err == nil {
			t.Fatalf("expected real bench_run schema to reject 'hosts'")
		}
		if err := mcpclient.ValidateArguments(benchRunTool, map[string]any{"nodes": "node-1", "fanout": 4}); err != nil {
			t.Fatalf("expected real bench_run schema to accept nodes+fanout: %v", err)
		}

		opsConn, err := store.SaveConnection(ctx, "", Connection{
			Name:    "real-clusops",
			Enabled: true,
			Config:  mcpclient.Config{Transport: "stdio", Command: clusopsBin, Args: []string{"mcp"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		probedOps, err := store.TestConnection(ctx, opsConn.ID, opsConn.Revision)
		if err != nil {
			t.Fatalf("TestConnection(clusops mcp) failed: %v", err)
		}
		var faultCheckTool mcpclient.Tool
		for _, tl := range probedOps.Tools {
			if tl.Name == "fault_check" {
				faultCheckTool = tl
				break
			}
		}
		if faultCheckTool.Name == "" {
			t.Fatalf("clusops mcp missing fault_check")
		}
		if err := mcpclient.ValidateArguments(faultCheckTool, map[string]any{"hosts": "node-1"}); err == nil {
			t.Fatalf("expected real fault_check schema to reject 'hosts'")
		}
	} else {
		for _, sub := range []struct{ pkg, run string }{
			{"./cmd/clusbench", "TestMCPRootTools"},
			{"./cmd/clusops", "TestFaultCheck"},
		} {
			cmd := exec.Command("go", "test", "-mod=vendor", sub.pkg, "-run", sub.run, "-count=1")
			cmd.Dir = cluskitDir
			cmd.Env = append(os.Environ(), "GOWORK=off")
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("cluskit2 %s (%s) failed: %v\n%s", sub.pkg, sub.run, err, string(b))
			}
		}
	}
}
