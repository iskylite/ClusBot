package work

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestCapabilityBindingAndMCPBenchmarkProvider(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mcp-op.db"))
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
	for _, name := range []string{"bench_run", "task_lookup", "task_status", "task_cancel"} {
		tool := mcpclient.Tool{
			Name: name, Version: "v1",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		}
		raw, _ := json.Marshal(tool)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only) VALUES(?,?,?,?,0)`,
			conn.ID, name, string(raw), true); err != nil {
			t.Fatal(err)
		}
	}

	binding, err := s.UpsertCapabilityBinding(ctx, CapabilityBindingInput{
		CapabilityKey: "cluster.benchmark.run",
		Version:       1,
		Kind:          "action",
		TargetKind:    "node",
		ExecutionMode: "async",
		ConnectionID:  conn.ID,
		MCPToolBinding: MCPToolBinding{
			StartTool:  "bench_run",
			LookupTool: "task_lookup",
			GetTool:    "task_status",
			CancelTool: "task_cancel",
		},
		Idempotent:    "supported",
		VerifiedScope: "pod7",
		// Attempt to expose action to model; UpsertCapabilityBinding must force false.
		ExposedToModel: true,
	})
	if err != nil {
		t.Fatalf("UpsertCapabilityBinding: %v", err)
	}
	if binding.ExposedToModel {
		t.Fatalf("action capability was exposed to model: %+v", binding)
	}
	listed, err := s.ListCapabilityBindings(ctx)
	if err != nil || len(listed) != 1 || listed[0].StartTool != "bench_run" || listed[0].CancelTool != "task_cancel" {
		t.Fatalf("ListCapabilityBindings: %+v, %v", listed, err)
	}

	// Test MCPBenchmarkProvider Start -> Lookup -> Get -> Cancel (confirmed & unconfirmed).
	cancelConfirmed := false
	provider, err := NewMCPBenchmarkProvider(conn.ID, conn.Revision, binding.MCPToolBinding,
		func(_ context.Context, toolName string, args map[string]any) (mcpclient.Result, error) {
			switch toolName {
			case "bench_run":
				return mcpclient.Result{Structured: json.RawMessage(`{"job_id":"J-101"}`)}, nil
			case "task_lookup":
				if args["idempotency_key"] == "op-key-1" {
					return mcpclient.Result{Structured: json.RawMessage(`{"job_id":"J-101"}`)}, nil
				}
				return mcpclient.Result{Structured: json.RawMessage(`{}`)}, nil
			case "task_status":
				return mcpclient.Result{Structured: json.RawMessage(`{"job_id":"J-101","state":"completed","measurements":[{"target":"node-1","outcome":"ok"}]}`)}, nil
			case "task_cancel":
				if cancelConfirmed {
					return mcpclient.Result{Structured: json.RawMessage(`{"confirmed":true}`)}, nil
				}
				return mcpclient.Result{Structured: json.RawMessage(`{"confirmed":false,"status":"unconfirmed"}`)}, nil
			default:
				return mcpclient.Result{IsError: true, Text: "unknown tool"}, nil
			}
		})
	if err != nil {
		t.Fatal(err)
	}

	req := BenchmarkJobRequest{
		Binding: BenchmarkBinding{
			Connection: conn.ID, Revision: conn.Revision, Tool: "bench_run", Validator: benchmarkValidatorV1,
		},
		IdempotencyKey: "op-key-1",
		Case:           "node-bandwidth",
		Targets:        []string{"node-1"},
	}
	if !provider.Supports(req.Binding) {
		t.Fatal("Supports returned false for valid binding")
	}
	jobID, err := provider.Start(ctx, req)
	if err != nil || jobID != "J-101" {
		t.Fatalf("Start: jobID=%q, err=%v", jobID, err)
	}
	foundID, err := provider.Lookup(ctx, req)
	if err != nil || foundID != "J-101" {
		t.Fatalf("Lookup: foundID=%q, err=%v", foundID, err)
	}
	res, err := provider.Get(ctx, req, jobID)
	if err != nil || res.State != "completed" || len(res.Measurements) != 1 {
		t.Fatalf("Get: %+v, err=%v", res, err)
	}
	if err := provider.Cancel(ctx, req, jobID); !errors.Is(err, ErrState) {
		t.Fatalf("unconfirmed cancel did not fail-closed with ErrState: %v", err)
	}
	cancelConfirmed = true
	if err := provider.Cancel(ctx, req, jobID); err != nil {
		t.Fatalf("confirmed cancel failed: %v", err)
	}
}
