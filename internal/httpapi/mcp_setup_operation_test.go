package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These two transports use only local fixtures and exercise the real ingress
// and MCP protocol. No business tool may be invoked during onboarding.
func localOnboardingServer(called func()) *mcp.Server {
	service := mcp.NewServer(&mcp.Implementation{Name: "fixture-private-provider", Version: "1"}, nil)
	for _, name := range []string{"fixture_read", "fixture_write"} {
		destructive := name == "fixture_write"
		service.AddTool(&mcp.Tool{Name: name, Description: "private-tool-description", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !destructive, DestructiveHint: &destructive}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called()
				return &mcp.CallToolResult{}, nil
			})
	}
	return service
}

const stdioProbeLog = "CLUSBOT_TEST_ONBOARDING_FIXTURE_LOG"

func TestHelperOnboardingStdioProcess(t *testing.T) {
	path := os.Getenv(stdioProbeLog)
	if path == "" {
		t.Skip("local MCP subprocess fixture")
	}
	appendFact := func(data string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			os.Exit(3)
		}
		if _, err = f.WriteString(data); err != nil {
			os.Exit(4)
		}
		if err = f.Close(); err != nil {
			os.Exit(5)
		}
	}
	appendFact("start\n")
	if err := localOnboardingServer(func() { appendFact("tool\n") }).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestMCPOnboardingHandshakePermissionBinding(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) { exerciseMCPHandshakePermissionBinding(t, transport) })
	}
}
func exerciseMCPHandshakePermissionBinding(t *testing.T, transport string) {
	var mentions, network, remoteCalls atomic.Int32
	service := localOnboardingServer(func() { remoteCalls.Add(1) })
	mcpHTTP := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { network.Add(1); mcpHTTP.ServeHTTP(w, r) }))
	defer srv.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "probe.activity")
	t.Setenv(stdioProbeLog, log)
	readActivity := func() (int32, int32) {
		if transport == "http" {
			return network.Load(), remoteCalls.Load()
		}
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return int32(strings.Count(string(data), "start\n")), int32(strings.Count(string(data), "tool\n"))
	}
	cfg := map[string]any{"transport": "http", "endpoint": srv.URL}
	target := srv.URL
	if transport == "stdio" {
		cfg = map[string]any{"transport": "stdio", "command": executable, "args": []string{"-test.run=^TestHelperOnboardingStdioProcess$"}, "env": []string{stdioProbeLog}}
		target = executable
	}

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mentions.Add(1)
		raw, _ := io.ReadAll(r.Body)
		for _, private := range []string{srv.URL, executable, stdioProbeLog, "TestHelperOnboardingStdioProcess", "fixture_read", "fixture_write", "private-tool-description", "fixture-private-provider", "renderer-secret"} {
			if strings.Contains(string(raw), private) {
				t.Errorf("private data leaked to model: %s", private)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", `{"next":"open_trusted_form"}`)
	}))
	defer provider.Close()
	t.Setenv("MCP_TEST_LOCAL", "local-only")
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		s := &model.Settings{Store: store}
		_, err := s.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "local", KeyRef: "env:MCP_TEST_LOCAL", Protocol: model.ProtocolResponses})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
	request := func(method, path, key string, data any, want int) map[string]any {
		t.Helper()
		rec := f.request(method, path, key, data, f.cookie)
		if rec.Code != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
		var out map[string]any
		accountJSON(t, rec, &out)
		return out
	}
	origin := request("POST", "/api/v1/conversations", "", map[string]string{"text": "接入 MCP", "client_request_id": "first-setup-turn"}, 202)
	conv := origin["conversation_id"].(string)
	base := "/api/v1/conversations/" + conv + "/mcp-setup/" + origin["run_id"].(string)
	state := request("GET", base, "", nil, 200)
	event := func(receipt map[string]any, component string) map[string]any {
		t.Helper()
		surface := receipt["surface"].([]any)[0].(map[string]any)["createSurface"].(map[string]any)
		for _, raw := range surface["components"].([]any) {
			c := raw.(map[string]any)
			if c["id"] != component {
				continue
			}
			ev := c["action"].(map[string]any)["event"].(map[string]any)
			return map[string]any{"version": "1.0", "action": map[string]any{"name": ev["name"], "context": ev["context"], "sourceComponentId": component, "surfaceId": surface["surfaceId"], "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "userMessage": "renderer-secret"}}
		}
		t.Fatalf("missing component %s in %+v", component, receipt)
		return nil
	}
	selectStep := func(state map[string]any, step, key string) (map[string]any, map[string]any) {
		t.Helper()
		ev := event(state, step)
		prop := request("POST", base+"/actions", key, ev, 200)
		if prop["state"] != "open_trusted_form" {
			t.Fatal(prop)
		}
		commit := map[string]any{"surface_id": ev["action"].(map[string]any)["surfaceId"], "revision": prop["revision"], "form_proof": prop["form_proof"], "request_key": key}
		if step != "http" && step != "stdio" {
			commit["step"] = step
			commit["connection_revision"] = prop["connection_revision"]
		}
		return commit, prop
	}
	bindForm, _ := selectStep(state, transport, "user-selects-transport")
	bindForm["name"] = "fixture-private-provider"
	bindForm["config"] = cfg
	review := request("POST", base+"/preview", "", bindForm, 200)
	if contacts, calls := readActivity(); review["state"] != "configuration_reviewed" || review["target"] != target || contacts != 0 || calls != 0 {
		t.Fatal("local preflight contacted the provider or checked the wrong target")
	}
	connections, err := f.instance.Connections(t.Context())
	if err != nil || len(connections) != 0 {
		t.Fatalf("local preflight registered a connection: %d %v", len(connections), err)
	}
	bindForm["preview_id"] = review["preview_id"]
	state = request("POST", base+"/connection", "", bindForm, 200)
	if contacts, _ := readActivity(); state["status"] != "untested" || contacts != 0 {
		t.Fatal("save triggered probe")
	}
	probe, details := selectStep(state, "probe", "prepare-probe-only")
	targetView, ok := details["probe_target"].(map[string]any)
	if !ok || targetView["target"] != target || targetView["transport"] != transport || targetView["name"] != "fixture-private-provider" {
		t.Fatalf("missing human probe target: %+v", targetView)
	}
	if contacts, _ := readActivity(); contacts != 0 {
		t.Fatal("agent decision triggered probe")
	}
	state = request("POST", base+"/operation", "", probe, 200)
	n, calls := readActivity()
	if state["status"] != "ready" || n == 0 || calls != 0 {
		t.Fatalf("probe did not isolate tool discovery: %+v %d %d", state, n, calls)
	}
	request("POST", base+"/operation", "", probe, 409)
	if contacts, _ := readActivity(); contacts != n {
		t.Fatal("replayed handshake")
	}
	grant, prop := selectStep(state, "permissions", "prepare-permissions")
	tools := prop["tools"].([]any)
	var v, unsafeV string
	for _, row := range tools {
		k := row.(map[string]any)
		if k["allowed"] != false {
			t.Fatal("probe granted tools")
		}
		if k["name"] == "fixture_write" {
			unsafeV = k["version"].(string)
			if k["can_read"] != false {
				t.Fatal("destructive tool offered as read-only")
			}
		}
		if k["name"] == "fixture_read" {
			v = k["version"].(string)
		}
	}
	if v == "" || unsafeV == "" {
		t.Fatal(tools)
	}
	grant["tool"], grant["tool_version"], grant["mode"] = "fixture_write", unsafeV, "read_only"
	request("POST", base+"/operation", "", grant, 400)
	grant["tool"], grant["tool_version"] = "fixture_read", v
	state = request("POST", base+"/operation", "", grant, 200)
	if state["bound_tool"] != nil {
		t.Fatal("grant implicitly bound")
	}
	selection, prop := selectStep(state, "binding", "prepare-conversation-bind")
	selection["tool"] = "fixture_read"
	selection["tool_version"] = v
	selection["binding_revision"] = prop["binding_revision"]
	state = request("POST", base+"/operation", "", selection, 200)
	if state["bound_tool"] != "fixture_read" || state["binding_status"] != "locally_ready" {
		t.Fatalf("binding %+v", state)
	}
	request("POST", base+"/operation", "", selection, 409)
	if contacts, calls := readActivity(); mentions.Load() != 4 || contacts != n || calls != 0 {
		t.Fatalf("calls to model=%d endpoint=%d tool=%d", mentions.Load(), contacts, calls)
	}
	actual := request("GET", "/api/v1/conversations/"+conv+"/mcp-selection", "", nil, 200)
	if actual["selection"].(map[string]any)["tool_name"] != "fixture_read" {
		t.Fatal(actual)
	}
}
