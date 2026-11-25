package work

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
)

func TestBenchmarkMCPArgumentsFailClosed(t *testing.T) {
	binding := BenchmarkBinding{"cluster", 1, "bench_run", benchmarkValidatorV1}
	calls := 0
	p, err := NewMCPBenchmarkProvider("cluster", 1, MCPToolBinding{StartTool: "bench_run"}, func(_ context.Context, _ string, got map[string]any) (mcpclient.Result, error) {
		calls++
		want := map[string]any{"benchmark": "hplmxp", "mode": "mp", "device": "dcu", "idempotency_key": "one",
			"nodes": "n1,n2", "percent": 0.01, "numa": "0-1", "dcu": "0,2", "fanout": 4,
			"args": []string{"N:128"}, "group_size": 2, "enable_nhc": false}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("MCP arguments changed: got %#v, want %#v", got, want)
		}
		return mcpclient.Result{Text: `{"task_id":"job-1"}`}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req := BenchmarkJobRequest{Binding: binding, IdempotencyKey: "one", Case: "logical-case", Targets: []string{"n1", "n2"},
		Parameters: map[string]string{"benchmark": "hplmxp", "mode": "mp", "device": "dcu", "percent": "0.01",
			"numa": "0-1", "dcu": "0,2", "concurrency": "4", "args": `["N:128"]`, "group_size": "2", "enable_nhc": "false"}}
	if job, err := p.Start(t.Context(), req); err != nil || job != "job-1" {
		t.Fatalf("start %q %v", job, err)
	}
	for _, fields := range []map[string]string{
		{"benchmark": "hpl", "keep_alive": "true"}, {"benchmark": "hpl", "fanout": "oops"},
		{"benchmark": "hpl", "percent": "NaN"}, {"benchmark": "hpl", "percent": "101"}, {"benchmark": "imaginary-benchmark"},
		{"benchmark": "hpl", "numa": "0;echo"}, {"benchmark": "hpl", "hostfile": "/etc/hosts"},
		{"benchmark": "hpl", "nodes": "n3"}, {"benchmark": "rccl:imaginary_collective"}, {"benchmark": "hpl", "args": `["invalid-value"]`},
		{"benchmark": "hpl", "mode": "single", "group_size": "2"}, {"benchmark": "hpl", "mystery": "foo"},
	} {
		req.Parameters = fields
		if _, err := p.Start(t.Context(), req); !errors.Is(err, ErrBenchmarkInput) {
			t.Errorf("accepted unsupported parameters %v: %v", fields, err)
		}
	}
	req.Targets = []string{"n[1-3]"}
	req.Parameters = map[string]string{"benchmark": "hpl"}
	if _, err := p.Start(t.Context(), req); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("nodeset expanded frozen identity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("invalid arguments crossed the provider boundary: %d calls", calls)
	}
}

func TestBenchmarkMCPDoesNotInventEvidence(t *testing.T) {
	binding := BenchmarkBinding{"cluster", 1, "bench_run", benchmarkValidatorV1}
	status := `{"task_id":"job-1","state":"COMPLETE","summary":{"passed":2,"bw_gbps":1000}}`
	p, err := NewMCPBenchmarkProvider("cluster", 1, MCPToolBinding{StartTool: "bench_run", GetTool: "task_status"}, func(_ context.Context, _ string, _ map[string]any) (mcpclient.Result, error) {
		return mcpclient.Result{Text: status}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req := BenchmarkJobRequest{Binding: binding, Case: "hpl", Targets: []string{"n1", "n2"}}
	if result, err := p.Get(t.Context(), req, "job-1"); err != nil || result.State != "completed" || len(result.Measurements) != 0 {
		t.Fatalf("aggregate summary treated as per-node proof: %+v, %v", result, err)
	}
	status = `{"task_id":"unrelated","state":"COMPLETE"}`
	if _, err := p.Get(t.Context(), req, "job-1"); !errors.Is(err, ErrBenchmarkBinding) {
		t.Fatalf("unrelated remote job accepted: %v", err)
	}
	status = `{"task_id":"job-1","state":"COMPLETE","results":[
		{"task_id":"other","node":"n1","event":"result","state":"succeeded","data":{"benchmark":"hpl","metrics":{"Rmax":1,"Unit":"GFLOPS"}}},
		{"task_id":"job-1","node":"n1","event":"result","state":"succeeded","data":{"benchmark":"hpl","nodes":["n1","n2"],"metrics":{"Rmax":2,"Unit":"GFLOPS"}}},
		{"task_id":"job-1","node":"else","event":"result","state":"succeeded","data":{"benchmark":"hpl","metrics":{"Rmax":3,"Unit":"GFLOPS"}}},
		{"task_id":"child","main_id":"job-1","node":"n1","event":"result","state":"succeeded","code":0,"data":{"benchmark":"hpl","metrics":{"Rmax":195.4,"Unit":"GFLOPS"},"row":{"node":"n1","benchmark":"hpl","status":"succeeded","metrics":{"unit":"GFLOPS","fields":{"Rmax":195.4,"N":12}}}}},
		{"task_id":"job-1","node":"n2","event":"result","state":"failed","code":1,"data":{"benchmark":"hpl","metrics":{"Rmax":4,"Unit":"GFLOPS"}}}
	]}`
	result, err := p.Get(t.Context(), req, "job-1")
	if err != nil || len(result.Measurements) != 2 {
		t.Fatalf("nested result attribution failed: %+v %v", result, err)
	}
	got := result.Measurements[0]
	if got.Target != "n1" || got.Outcome != "measured" || got.Metrics["Rmax"] != (BenchmarkMeasuredMetric{195.4, "GFLOPS"}) ||
		got.Metrics["N"] != (BenchmarkMeasuredMetric{12, "count"}) || got.Raw != nil || len(got.ActualConfig) != 0 {
		t.Fatalf("metrics/raw evidence were not faithfully decoded: %+v", got)
	}
	if result.Measurements[1].Target != "n2" || result.Measurements[1].Outcome != "failed" {
		t.Fatalf("failed node reported as measured: %+v", result.Measurements[1])
	}
}

func TestBenchmarkDynamicProviderUsesCallerTransactionEvenWithoutBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	s, err := Open(filepath.Join(t.TempDir(), "dynamic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	binding := BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1}
	seedBenchmarkMCPFixture(t, ctx, s, actor, binding)
	p := s.DynamicBenchmarkProvider(&benchFixtureProvider{})
	if ready, err := s.PreflightBenchmark(ctx, actor, BenchmarkProfileRef{"plan", 1}, p); err != nil || !ready.CanQueue {
		t.Fatalf("preflight borrowed a second writer or rejected fallback: %+v %v", ready, err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"once", "plan", 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BenchmarkProgressWithProvider(ctx, actor, task, p); err != nil {
		t.Fatal(err)
	}
	if d, ok, err := s.claimBenchmark(ctx, p, now()); err != nil || !ok || d.action != "start" {
		t.Fatalf("dynamic claim = %+v, %v, %v", d, ok, err)
	}
}

func TestBenchmarkDynamicProviderPinsLifecycleTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	s, err := Open(filepath.Join(t.TempDir(), "pinned.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.SaveConnection(ctx, "", Connection{Name: "cluster", Enabled: true, Config: mcpclient.Config{Transport: "stdio", Command: "/bin/echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE connections SET status='ready', credential_binding='bound-identity', protocol='stdio' WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bench_run", "task_status", "task_cancel", "other_status"} {
		readOnly := name != "bench_run" && name != "task_cancel"
		raw, _ := json.Marshal(mcpclient.Tool{Name: name, Version: "1", ServerReadOnly: &readOnly, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`)})
		if _, err := s.db.ExecContext(ctx, `INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only) VALUES(?,?,?,?,?)`,
			conn.ID, name, string(raw), true, readOnly); err != nil {
			t.Fatal(err)
		}
	}
	setTools := func(get string) {
		t.Helper()
		_, err := s.UpsertCapabilityBinding(ctx, CapabilityBindingInput{CapabilityKey: "cluster.benchmark.run", Version: 1,
			Kind: "action", TargetKind: "node", ExecutionMode: "async", ConnectionID: conn.ID, Idempotent: "supported",
			VerifiedScope: "test", MCPToolBinding: MCPToolBinding{StartTool: "bench_run", LookupTool: "task_status", GetTool: get, CancelTool: "task_cancel"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	setTools("task_status")
	const actor = "user:session"
	binding := BenchmarkBinding{conn.Name, conn.Revision, "bench_run", benchmarkValidatorV1}
	seedBenchmarkMCPFixture(t, ctx, s, actor, binding)
	p := s.DynamicBenchmarkProvider(nil)
	if ready, err := s.PreflightBenchmark(ctx, actor, BenchmarkProfileRef{"plan", 1}, p); err != nil || !ready.CanQueue {
		t.Fatalf("cannot queue pinned MCP operation: %+v %v", ready, err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"once", "plan", 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	d, ok, err := s.claimBenchmark(ctx, p, stamp)
	if err != nil || !ok || d.action != "start" || d.provider == nil {
		t.Fatalf("original operation not pinned: %+v %v %v", d, ok, err)
	}
	var pinned, installedConnection, capability string
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT params_digest,connection_id,capability_key,capability_version FROM operations WHERE operation_id=?`, d.operation).
		Scan(&pinned, &installedConnection, &capability, &version); err != nil || pinned == digest(toJSON(d.request)) ||
		installedConnection != conn.ID || capability != "cluster.benchmark.run" || version != 1 {
		t.Fatalf("expected resolved capability and tool pin in operation: %q %q %q %d %v", pinned, installedConnection, capability, version, err)
	}
	setTools("other_status")
	progress, err := s.BenchmarkProgressWithProvider(ctx, actor, task, p)
	if err != nil || len(progress.Cases) != 1 || progress.Cases[0].TargetHolds != 1 || progress.Cases[0].Phase != "submitting" {
		t.Fatalf("drift released or restarted operation: %+v %v", progress, err)
	}
	changed, ok, err := s.claimBenchmark(ctx, p, stamp+benchmarkClaimMS+1)
	if err != nil || !ok || changed.action != "" || changed.operation != d.operation {
		t.Fatalf("drift allowed a lookup or reexecution: %+v %v %v", changed, ok, err)
	}
	setTools("task_cancel")
	if p.Supports(binding) {
		t.Fatal("write-enabled cancel tool unexpectedly accepted as query")
	}
	setTools("task_status")
	recovered, ok, err := s.claimBenchmark(ctx, p, stamp+benchmarkClaimMS+30002)
	if err != nil || !ok || recovered.action != "lookup" || recovered.operation != d.operation || recovered.request.IdempotencyKey != d.request.IdempotencyKey {
		t.Fatalf("restoring the exact binding must only resume original operation: %+v %v %v", recovered, ok, err)
	}
}

func seedBenchmarkMCPFixture(t *testing.T, ctx context.Context, s *Store, actor string, binding BenchmarkBinding) {
	t.Helper()
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "serial-1",
		Benchmark: []DomainBenchmarkGrant{{Connection: binding.Connection, Revision: binding.Revision, Tool: binding.Tool, ExpiresAt: now() + 600000}}}}); err != nil {
		t.Fatal(err)
	}
	c := BenchmarkCase{Key: "hpl", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"Rmax": "GFLOPS"}}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "plan", Version: 1, Cases: []BenchmarkCase{c}}); err != nil {
		t.Fatal(err)
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("test requires the production single-writer pool")
	}

}

func TestBenchmarkMCPCancelAcknowledgementDoesNotReleaseHold(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "native-cancel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	binding := BenchmarkBinding{"cluster", 1, "bench_run", benchmarkValidatorV1}
	seedBenchmarkMCPFixture(t, ctx, s, actor, binding)
	terminal, cancels := false, 0
	provider, err := NewMCPBenchmarkProvider("cluster", 1, MCPToolBinding{
		StartTool: "bench_run", LookupTool: "task_status", GetTool: "task_status", CancelTool: "task_cancel",
	}, func(_ context.Context, name string, args map[string]any) (mcpclient.Result, error) {
		switch name {
		case "bench_run":
			return mcpclient.Result{Text: `{"task_id":"job-1"}`}, nil
		case "task_status":
			if args["task_id"] != "job-1" || args["include_results"] != true {
				t.Fatalf("query not scoped to original task: %v", args)
			}
			if terminal {
				return mcpclient.Result{Text: `{"task_id":"job-1","state":"CANCELED","canceled":1}`}, nil
			}
			return mcpclient.Result{Text: `{"task_id":"job-1","state":"RUNNING"}`}, nil
		case "task_cancel":
			if !reflect.DeepEqual(args, map[string]any{"task_id": "job-1"}) {
				t.Fatalf("cancel not scoped to original task: %v", args)
			}
			cancels++
			return mcpclient.Result{Text: `{"task_ids":["job-1"],"acknowledged":true,"confirmed":["engine:job-1"],"unconfirmed":[],"remote_exit_verified":false}`}, nil
		}
		t.Fatalf("unexpected tool %s", name)
		return mcpclient.Result{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"once", "plan", 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		ok, err := s.runBenchmarkOnce(ctx, provider)
		if err != nil || !ok {
			t.Fatalf("run failed: %v %v", ok, err)
		}
	}
	run()
	if err := s.Control(ctx, task, "stop", 1); err != nil {
		t.Fatal(err)
	}
	run() // native acknowledgment
	run() // native RUNNING
	got, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || cancels != 1 || got.State != "finalizing" || got.Cases[0].TargetHolds != 1 || got.Cases[0].Wait.State != "open" {
		t.Fatalf("cancel acknowledgment falsely released original task: %+v (%d), %v", got, cancels, err)
	}
	terminal = true
	if _, err := s.db.ExecContext(ctx, "UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	run()
	got, err = s.BenchmarkProgress(ctx, actor, task)
	if err != nil || cancels != 1 || got.State != "cancelled" || got.Cases[0].TargetHolds != 0 || got.Cases[0].Wait.State != "closed" {
		t.Fatalf("terminal proof did not settle original task: %+v (%d), %v", got, cancels, err)
	}
}
