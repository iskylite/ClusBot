package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Simulates the wire shape emitted by Cluskit2-MCP, not the execution of
// SSH, Redfish or a real Cluskit binary. No cluster targets are contacted.
func TestHelperDiscoveryContractMCP(t *testing.T) {
	if os.Getenv("CLUSBOT_TEST_DISCOVERY_CONTRACT") != "run-discovery-contract-fixture" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "cluskit-contract-fixture", Version: "1"}, nil)
	add := func(name string, fn func(map[string]any) (map[string]any, error)) {
		mcp.AddTool(server, &mcp.Tool{Name: name, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
			func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
				body, err := fn(args)
				if err != nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fixture"}}}, body, nil
			})
	}
	add("discover", func(args map[string]any) (map[string]any, error) {
		ranges, _ := args["ip_range"].([]any)
		bmc, _ := args["bmc_range"].([]any)
		if args["protocol"] != "both" || args["sync"] != false || len(ranges) != 1 || ranges[0] != "192.0.2.0/30" ||
			len(bmc) != 1 || bmc[0] != "198.51.100.5-198.51.100.7" || !strings.HasPrefix(args["idempotency_key"].(string), "discovery:") {
			return nil, fmt.Errorf("wrong approved scan args: %#v", args)
		}
		return map[string]any{"task_id": "discover-example", "state": "RUNNING"}, nil
	})
	add("task_status", func(args map[string]any) (map[string]any, error) {
		if args["task_id"] != "discover-example" {
			return nil, fmt.Errorf("wrong task")
		}
		return map[string]any{"task_id": "discover-example", "state": "COMPLETE", "total": 3}, nil
	})
	add("task_stream", func(args map[string]any) (map[string]any, error) {
		if args["task_id"] != "discover-example" || args["limit"] != float64(1) || args["event_filter"] != "result" {
			return nil, fmt.Errorf("wrong page: %+v", args)
		}
		seq := int(args["from_seq"].(float64))
		row := map[string]any{"target": "p3c0", "status": "succeeded"}
		switch seq {
		case 1:
			row["observation"] = map[string]any{"serial": "SER-1", "address": "192.0.2.1"}
			row["ssh"] = map[string]any{"serial": "SER-1", "hostname": "p3c0", "address": "192.0.2.1", "test_large_inventory": strings.Repeat("a", 32<<10)}
			row["management_endpoint"] = "198.51.100.6"
			row["redfish"] = map[string]any{"assets": []any{map[string]any{"kind": "server", "serial": "SER-1"}}}
		case 2:
			row["target"], row["management_endpoint"] = "198.51.100.7", "198.51.100.7:443"
			row["observation"] = map[string]any{"serial": "SW-1", "address": "198.51.100.7"}
			row["redfish"] = map[string]any{"assets": []any{map[string]any{"kind": "switch", "serial": "SW-1"}}}
		case 3:
			row["target"], row["status"] = "p3c1", "failed"
		default:
			return nil, fmt.Errorf("unexpected cursor %d", seq)
		}
		event := map[string]any{"task_id": "discover-example", "seq": seq, "data": map[string]any{"asset_discovery": row}}
		return map[string]any{"task_id": "discover-example", "next_seq": seq + 1, "terminal": seq == 3, "events": []any{event}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDiscoveryMCPPinnedScopeReceiptsAndAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	catalog, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	db, err := Open(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("CLUSBOT_TEST_DISCOVERY_CONTRACT", "run-discovery-contract-fixture")
	conn, err := catalog.SaveConnection(ctx, "", Connection{Name: "fixture-cluskit", Enabled: true, Config: mcpclient.Config{
		Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestHelperDiscoveryContractMCP$"}, Env: []string{"CLUSBOT_TEST_DISCOVERY_CONTRACT"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = catalog.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range conn.Tools {
		v, err := catalog.Connection(ctx, conn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = catalog.SetToolPermissionWithMode(ctx, v.ID, tool.Name, tool.Version, v.Revision, true, true); err != nil {
			t.Fatal(err)
		}
	}
	checks := 0
	source := GuardDiscoverySource(catalog.DynamicDiscoverySource(nil), func(context.Context) error { checks++; return nil })
	scan := &DiscoveryScopeSpec{Protocol: "both", IPRange: []string{"192.0.2.0/30"}, BMCRange: []string{"198.51.100.5-198.51.100.7"}}
	plan := DiscoveryPlanRequest{ID: "confirmed-scope", Source: "fixture-cluskit", Scope: "192.0.2.0/30", Scan: scan, PeriodSeconds: 60, FirstAt: now() + 400}
	grant := DiscoveryGrant{Source: plan.Source, Scope: plan.Scope, Scan: scan, ExpiresAt: now() + 100000}
	if _, err = db.GrantDiscovery(ctx, "user:session", grant, source); err != nil {
		t.Fatal(err)
	}
	altered := plan
	altered.Scan = &DiscoveryScopeSpec{Protocol: "ssh", Nodes: "p3c[0-9]"}
	if err = db.PublishDiscoveryPlan(ctx, "user:session", altered, source); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("different approved scope accepted: %v", err)
	}
	if err = db.PublishDiscoveryPlan(ctx, "user:session", plan, source); err != nil {
		t.Fatal(err)
	}
	if worked, err := db.runDiscoveryOnce(ctx, source, plan.FirstAt); err != nil || !worked {
		t.Fatalf("run=%t %v", worked, err)
	}
	state, err := db.ReadDiscoverySetupStatus(ctx, "user:session", source, plan.Source, plan.Scope)
	if err != nil || state.LastRun == nil || state.LastRun.Coverage == nil || state.LastRun.Coverage.Outcome != "partial" || state.LastRun.Coverage.Observed != 2 || state.LastRun.Coverage.Total != 3 {
		t.Fatalf("partial state: %+v run=%+v coverage=%+v %v", state, state.LastRun, state.LastRun.Coverage, err)
	}
	candidates, err := db.ListDiscoveryCandidates(ctx, "user:session")
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates %+v %v", candidates, err)
	}
	for _, c := range candidates {
		if c.Evidence == nil || c.Evidence.ProviderTaskID != "discover-example" || c.Evidence.Sequence == 0 {
			t.Fatalf("receipt absent: %+v", c)
		}
		if c.Serial == "SW-1" && (c.Evidence.Kind != "switch" || c.Evidence.SSHEndpoint != "" || c.Evidence.BMCEndpoint != "198.51.100.7:443") {
			t.Fatalf("switch endpoints altered: %+v", c.Evidence)
		}
		if c.Serial == "SER-1" && (c.Evidence.Kind != "node" || c.Evidence.SSHEndpoint != "p3c0" || c.Evidence.BMCEndpoint != "198.51.100.6") {
			t.Fatalf("server endpoints altered: %+v", c.Evidence)
		}
		admitted, err := db.AdmitDiscoveryCandidate(ctx, "user:session", DiscoveryAdmission{Candidate: c.ID, ConfirmedSerial: c.Serial,
			ExpectedSeenAt: c.SeenAt, Owner: "user:session", Location: "rack", Role: "fixture"})
		if err != nil || !strings.HasPrefix(admitted.Asset, c.Evidence.Kind+":discovered:") {
			t.Fatalf("admit: %+v %v", admitted, err)
		}
		switch c.Evidence.Kind {
		case "node":
			if endpoint, err := db.DiscoveryAdmittedEndpoint(ctx, admitted.Asset, "ssh"); err != nil || endpoint != "p3c0" {
				t.Fatalf("node ssh endpoint: %q %v", endpoint, err)
			}
		case "switch":
			if endpoint, err := db.DiscoveryAdmittedEndpoint(ctx, admitted.Asset, "ssh"); err == nil {
				t.Fatalf("invented switch SSH endpoint: %q", endpoint)
			}
			if endpoint, err := db.DiscoveryAdmittedEndpoint(ctx, admitted.Asset, "redfish"); err != nil || endpoint != "198.51.100.7:443" {
				t.Fatalf("switch Redfish endpoint: %q %v", endpoint, err)
			}
		}
		var persisted string
		if err = db.db.QueryRow("SELECT evidence FROM discovery_admission_evidence WHERE candidate_id=?", c.ID).Scan(&persisted); err != nil ||
			!strings.Contains(persisted, "discover-example") {
			t.Fatalf("admission did not persist receipt: %q %v", persisted, err)
		}
	}
	if checks < 4 {
		t.Fatalf("account guard not passed through: %d", checks)
	}
	// A revision change does not silently retarget the frozen plan.
	current, err := catalog.Connection(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range current.Tools {
		if tool.Name == "task_stream" {
			if err = catalog.SetToolPermissionWithMode(ctx, current.ID, tool.Name, tool.Version, current.Revision, false, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	state, err = db.ReadDiscoverySetupStatus(ctx, "user:session", source, plan.Source, plan.Scope)
	if err != nil || state.State != "source_unavailable" {
		t.Fatalf("stale plan appeared ready: %+v %v", state, err)
	}
}

func TestDiscoveryMCPContractSchemaUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v136.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"discovery_plan_bindings", "discovery_scan_coverages", "discovery_observation_evidence", "discovery_admission_evidence"} {
		if _, err = s.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("PRAGMA user_version=136"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion {
		t.Fatalf("version=%d: %v", version, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('discovery_plan_bindings','discovery_scan_coverages','discovery_observation_evidence','discovery_admission_evidence')").Scan(&count); err != nil || count != 4 {
		t.Fatalf("discovery tables=%d: %v", count, err)
	}
}

func TestDiscoveryMCPContractValidatesScopeForms(t *testing.T) {
	for _, label := range []string{"p3c0", "p3c[0-9]", "192.0.2.7-192.0.2.10", "192.0.2.0/24", "192.0.2.0/31"} {
		if _, err := normalizeDiscoveryScope(label, nil); err != nil {
			t.Fatalf("valid %s rejected: %v", label, err)
		}
	}
	for _, label := range []string{"all", "192.0.2.7-192.0.2.10/24", "192.0.2.7/24", "192.0.2.0/16", "p3c[0-400]", "p3c[9-0]", "2001:db8::/64"} {
		if _, err := normalizeDiscoveryScope(label, nil); err == nil {
			t.Fatalf("invalid %s accepted", label)
		}
	}
	if _, err := normalizeDiscoveryScope("r", &DiscoveryScopeSpec{Protocol: "both", IPRange: []string{"192.0.2.0/24"}, BMCRange: []string{"198.51.100.0/24"}}); err == nil {
		t.Fatal("combined bound not applied")
	}
}
