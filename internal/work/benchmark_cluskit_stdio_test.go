package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHelperPrivateCluskitBenchmarkStdio(t *testing.T) {
	if os.Getenv("CLUSBOT_PRIVATE_CLUSKIT_FIXTURE") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "clusbench"}, nil)
	root := os.Getenv("CLUS_TASK_DIR")
	keyPath := filepath.Join(root, "job-key")
	const jobID = "native-bench-1"
	reply := func(body string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}
	}
	mcp.AddTool(server, &mcp.Tool{Name: "bench_run", Annotations: &mcp.ToolAnnotations{}},
		func(_ context.Context, _ *mcp.CallToolRequest, input struct {
			Benchmark  string `json:"benchmark"`
			Nodes      string `json:"nodes"`
			RequestKey string `json:"idempotency_key"`
		}) (*mcp.CallToolResult, any, error) {
			if input.Nodes != "node-1" || input.Benchmark != "stream" || input.RequestKey == "" {
				return nil, nil, errors.New("invalid benchmark call")
			}
			f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return nil, nil, errors.New("unexpected repeat Start")
			}
			_, err = f.WriteString(input.RequestKey)
			closeErr := f.Close()
			if err = errors.Join(err, closeErr); err != nil {
				return nil, nil, err
			}
			out := reply("simulated loss of original acknowledgement")
			out.IsError = true
			return out, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "task_status", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, input struct {
			TaskID     string `json:"task_id,omitempty"`
			RequestKey string `json:"idempotency_key,omitempty"`
			Tool       string `json:"tool,omitempty"`
			Results    bool   `json:"include_results,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			data, err := os.ReadFile(keyPath)
			if err != nil {
				return nil, nil, err
			}
			if input.RequestKey != "" {
				if input.RequestKey != string(data) || input.Tool != "bench_run" {
					return reply(`{"found":false,"state":"NOT_FOUND"}`), nil, nil
				}
				return reply(`{"found":true,"task_id":"` + jobID + `"}`), nil, nil
			}
			if input.TaskID != jobID || !input.Results {
				return nil, nil, errors.New("missing original job or results")
			}
			return reply(`{"task_id":"` + jobID + `","state":"COMPLETE","results":[{"task_id":"` + jobID + `","event":"result","state":"succeeded","node":"node-1","data":{"benchmark":"stream","metrics":{"Unit":"GB/s","Triad":117}}}]}`), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "task_cancel", Annotations: &mcp.ToolAnnotations{}},
		func(context.Context, *mcp.CallToolRequest, struct {
			TaskID string `json:"task_id"`
		}) (*mcp.CallToolResult, any, error) {
			return reply(`{"task_ids":["` + jobID + `"],"acknowledged":true,"confirmed":["engine:` + jobID + `"]}`), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "discover", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return reply(`{"observations":[]}`), nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestPrivateCluskitBenchmarkOptInLifecycle(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUS_ROOT", dir)
	t.Setenv("CLUS_TASK_DIR", dir)
	t.Setenv("CLUSKIT_LOG_DIR", dir)
	configPath := filepath.Join(dir, "isolated-config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUS_CONFIG_FILE", configPath)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "clusbench")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = mcp ] || exit 7\nprintf x >> \"$CLUS_ROOT/launches\"\nexport CLUSBOT_PRIVATE_CLUSKIT_FIXTURE=1\nexec %s -test.run '^TestHelperPrivateCluskitBenchmarkStdio$'\n", quote(exe))
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	base := mcpclient.Config{Transport: "stdio", Command: command, Args: []string{"mcp"}, Env: []string{"CLUS_ROOT", "CLUS_TASK_DIR", "CLUSKIT_LOG_DIR", "CLUS_CONFIG_FILE"}}
	// Probe only the inactive disposable fixture to obtain exact operator pins.
	session, err := mcpclient.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := mcpclient.Discover(ctx, session)
	_ = session.Close()
	if err != nil {
		t.Fatal(err)
	}
	toolDigests := map[string]string{}
	for _, tool := range catalog.Tools {
		if tool.Name != "discover" {
			toolDigests[tool.Name] = tool.Version
		}
	}
	in := CluskitBenchmarkStdio{Command: command, Connection: "reserved", Revision: 1, ToolDigests: toolDigests}
	runtime, err := PrepareCluskitBenchmarkRuntime(in)
	if err != nil {
		t.Fatal(err)
	}
	p, stop, err := runtime.NewAccountProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop() }()
	launches, err := os.ReadFile(filepath.Join(dir, "launches"))
	if err != nil || string(launches) != "x" {
		t.Fatalf("preparation implicitly launched server: %q %v", launches, err)
	}
	if err := runtime.VerifyEndpoint(ctx); err != nil {
		t.Fatalf("explicit startup probe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup probe called a benchmark tool: %v", err)
	}
	mismatch := in
	mismatch.ToolDigests = make(map[string]string, len(in.ToolDigests))
	for name, value := range in.ToolDigests {
		mismatch.ToolDigests[name] = value
	}
	mismatch.ToolDigests["task_status"] = strings.Repeat("0", 64)
	untrusted, err := PrepareCluskitBenchmarkRuntime(mismatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := untrusted.VerifyEndpoint(ctx); !errors.Is(err, ErrBenchmarkBinding) {
		t.Fatalf("startup probe did not reject changed schema: %v", err)
	}
	untrustedProvider, untrustedStop, err := untrusted.NewAccountProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer untrustedStop()
	untrustedRequest := BenchmarkJobRequest{Binding: BenchmarkBinding{"reserved", 1, "bench_run", benchmarkValidatorV1},
		IdempotencyKey: "must-not-start", Case: "stream", Targets: []string{"node-1"}}
	if _, err := untrustedProvider.Start(ctx, untrustedRequest); !errors.Is(err, ErrBenchmarkBinding) {
		t.Fatalf("unapproved lookup schema passed before Start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unapproved lifecycle invoked bench_run: %v", err)
	}

	instance, err := Open(filepath.Join(dir, "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.ConfigureCluskitBenchmarkIsolation(ctx, true); err != nil {
		t.Fatal(err)
	}
	account, err := instance.BootstrapAccount(ctx, "operator", "Operator", "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(dir, "account.sqlite")
	store, err := OpenAccountBusinessStore(storePath, instance)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	if err := store.BindAccountBusinessStore(ctx, account.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if err := store.InstallCluskitBenchmarkProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	binding := BenchmarkBinding{Connection: "reserved", Revision: 1, Tool: "bench_run", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: binding.Connection, Tool: binding.Tool, Revision: 1, ExpiresAt: now() + 60000}
	if err := store.ImportDomainAssetsForAccount(ctx, account.PrincipalID, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "known", PhysicalSerial: "SERIAL-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	profile := BenchmarkProfile{Key: "stream", Version: 1, Cases: []BenchmarkCase{{Key: "stream", Binding: binding, Targets: []string{"node-1"},
		RequiredMetrics: map[string]string{"Triad": "GB/s"}}}}
	if err := store.PublishBenchmarkProfile(ctx, account.PrincipalID, profile); err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitBenchmarkRun(ctx, account.PrincipalID, BenchmarkRunRequest{RequestKey: "tested-stdio", Profile: "stream", Version: 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	if conns, e := store.Connections(ctx); e != nil || len(conns) != 0 {
		t.Fatalf("runtime transport exposed in connection catalog: %v %v", conns, e)
	}
	if tools, e := store.TaskTools(ctx); e != nil || len(tools) != 0 {
		t.Fatalf("runtime lifecycle leaked as account tools: %v %v", tools, e)
	}
	if _, err := p.caller(ctx, "run", map[string]any{}); !errors.Is(err, ErrBenchmarkBinding) {
		t.Fatalf("non-lifecycle tool was callable: %v", err)
	}
	// An accidental generic registration targeting the same process UID and
	// TasksRoot must not bypass the benchmark gate. Keep unrelated tool calls.
	conn, err := instance.SaveConnection(ctx, "", Connection{Name: "ordinary-tools", Enabled: true, Config: base})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = instance.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bench_run", "task_status", "discover"} {
		var version string
		for _, tool := range conn.Tools {
			if tool.Name == name {
				version = tool.Version
				break
			}
		}
		if err := instance.SetToolPermissionWithMode(ctx, conn.ID, name, version, conn.Revision, true, name != "bench_run"); err != nil {
			t.Fatal(err)
		}
		conn, err = instance.Connection(ctx, conn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, blocked := range []string{"bench_run", "task_status", "task_cancel", "task_artifacts"} {
		if _, err := instance.CallAccountConnectionTool(ctx, account.PrincipalID, conn.ID, blocked, conn.Revision, nil); !errors.Is(err, ErrToolPermission) {
			t.Fatalf("account API path called reserved tool %s: %v", blocked, err)
		}
	}
	if call, err := instance.CallAccountConnectionTool(ctx, account.PrincipalID, conn.ID, "discover", conn.Revision, map[string]any{}); err != nil ||
		call.SourceUnavailable || !strings.Contains(call.Text, "observations") {
		t.Fatalf("unrelated provider call was blocked: %+v %v", call, err)
	}
	auto, err := store.automaticTaskTools(ctx, Run{Generation: 1})
	if err != nil || len(auto) != 1 || auto[0].source.ToolName != "discover" {
		t.Fatalf("model catalog exposed reserved tools or blocked unrelated ones: %+v %v", auto, err)
	}
	modelTx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.selectedMCPOption(ctx, modelTx, MCPReadSelection{
		ConnectionID: conn.ID, Revision: conn.Revision, ToolName: "task_status", ToolVersion: toolDigests["task_status"]})
	_ = modelTx.Rollback()
	if !errors.Is(err, ErrToolPermission) {
		t.Fatalf("old pinned model selection retained reserved tool: %v", err)
	}
	tick := func() {
		t.Helper()
		if _, err := store.runBenchmarkOnce(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	status, err := store.BenchmarkProgress(ctx, account.PrincipalID, task)
	if err != nil || len(status.Cases) != 1 || status.Cases[0].Phase != "submitting" || status.Cases[0].TargetHolds != 1 {
		t.Fatalf("uncertain dispatch not held: %+v %v", status, err)
	}
	stop()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenAccountBusinessStore(storePath, instance)
	if err != nil {
		t.Fatal(err)
	}
	p, stop, err = runtime.NewAccountProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	tick()
	tick()
	status, err = store.BenchmarkProgress(ctx, account.PrincipalID, task)
	if err != nil || status.Cases[0].Phase != "verified" || status.Cases[0].JobID != "native-bench-1" || status.Cases[0].TargetHolds != 0 {
		t.Fatalf("original stdio task not reconciled: %+v %v", status, err)
	}
	if err := os.WriteFile(configPath, []byte("revoked: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if p.Supports(binding) {
		t.Fatal("changed operator config kept original transport active")
	}
	if _, err := p.Lookup(ctx, BenchmarkJobRequest{Binding: binding, IdempotencyKey: "not-allowed"}); !errors.Is(err, ErrBenchmarkBinding) {
		t.Fatalf("call went through revoked transport: %v", err)
	}
	otherTasks := t.TempDir()
	t.Setenv("CLUS_TASK_DIR", otherTasks)
	relocated, err := PrepareCluskitBenchmarkRuntime(in)
	if err != nil || relocated.jobSource == runtime.jobSource || relocated.pin == runtime.pin {
		t.Fatalf("changed TasksRoot retained identity: %v", err)
	}
}

func TestCluskitBenchmarkIsolationSurvivesDisablingDispatch(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	instance, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.ConfigureCluskitBenchmarkIsolation(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	instance, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.ConfigureCluskitBenchmarkIsolation(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !instance.cluskitBenchmarkToolReserved("task_status") || instance.cluskitBenchmarkToolReserved("discover") {
		t.Fatal("disabling dispatch did not preserve only the reserved tool names")
	}
}
