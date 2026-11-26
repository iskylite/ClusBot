package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Optional end-to-end fixture. It serves production frontend assets and real
// authenticated ClusBot routes over an ephemeral loopback server. Only the
// *external* model and MCP providers are replaced, by local fixture servers.
func TestMCPOnboardingBrowserRoundTrip(t *testing.T) {
	exerciseMCPOnboardingBrowser(t, false)
}

// This fixture launches real Cluskit2 source via stdio as the current user.
// It only checks an absent synthetic idempotency key in a fresh local session.
func TestMCPOnboardingBrowserRoundTripWithCluskit2(t *testing.T) {
	if os.Getenv("CLUSBOT_E2E_REAL_CLUSKIT2") != "1" {
		t.Skip("set CLUSBOT_E2E_REAL_CLUSKIT2=1 to build the adjacent Cluskit2 checkout and run its real stdio server")
	}
	if os.Geteuid() == 0 {
		t.Skip("the real Cluskit2 browser fixture intentionally runs without root")
	}
	exerciseMCPOnboardingBrowser(t, true)
}

func exerciseMCPOnboardingBrowser(t *testing.T, realCluskit bool) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set CLUSBOT_PLAYWRIGHT_MODULE and CLUSBOT_CHROMIUM to installed local browser tooling")
	}
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	script := filepath.Join(web, "tests", "mcpSetupBrowserHarness.mjs")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	runBrowser := func(mode string, stdin []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", script, mode)
		cmd.Dir = web
		if len(stdin) > 0 {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, stderr.String())
		}
		return out
	}
	var assetFiles map[string][]byte
	if err := json.Unmarshal(runBrowser("bundle", nil), &assetFiles); err != nil || len(assetFiles["index.html"]) == 0 {
		t.Fatalf("invalid in-memory frontend bundle: %v", err)
	}
	assets := fstest.MapFS{}
	for name, data := range assetFiles {
		assets[name] = &fstest.MapFile{Data: data}
	}

	var calls, earlyMCP, mcpRequests, toolCalls atomic.Int32
	var probeConfirmed atomic.Bool
	var target string
	var cluskit *browserCluskitSource
	if realCluskit {
		cluskit = buildBrowserCluskitSource(t, ctx, web)
		target = cluskit.executable
	} else {
		toolServer := localOnboardingServer(func() { toolCalls.Add(1) })
		sdkHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return toolServer }, nil)
		t.Setenv("CLUSBOT_TEST_BROWSER_MCP_TOKEN", "loopback-mcp-fixture-token")
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !probeConfirmed.Load() {
				earlyMCP.Add(1)
			}
			mcpRequests.Add(1)
			if r.Header.Get("Authorization") != "Bearer loopback-mcp-fixture-token" {
				t.Error("trusted MCP connection did not receive its configured fixture credential")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			sdkHandler.ServeHTTP(w, r)
		}))
		defer service.Close()
		target = service.URL
	}

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if cluskit != nil && !bytes.Contains(raw, []byte("MCP 接入助手")) {
			cluskit.modelDecision(t, w, raw)
			return
		}
		calls.Add(1)
		for _, v := range []string{target, "CLUS_ROOT", "CLUS_CONFIG_FILE", "CLUSBOT_TEST_BROWSER_MCP_TOKEN", "loopback-mcp-fixture-token", "fixture_read", "fixture_write", "task_status", "只在受信表单可见", "我有一个 MCP 要接入", "correct horse battery staple"} {
			if bytes.Contains(raw, []byte(v)) {
				t.Error("incidental browser or connection data reached the model")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", `{"next":"open_trusted_form"}`)
	}))
	defer provider.Close()

	t.Setenv("CLUSBOT_TEST_BROWSER_MODEL_KEY", "loopback-model-fixture-key")
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(state, "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()

	if cluskit != nil {
		defer func() {
			if t.Failed() {
				if rows, err := instance.Connections(context.Background()); err == nil {
					for _, c := range rows {
						t.Logf("stdio fixture: state=%s, detail=%q, tools=%d", c.Status, c.Detail, c.ToolCount)
					}
				}
				cluskit.probeDiagnostic(t)
			}
		}()
	}
	instance.ConfigureAccountModelConcurrency(2)
	setup := Server{Store: instance, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	f := &accountFixture{t: t, store: instance, handler: setup.Handler(assets)}
	actor := f.setup()
	personal, err := work.OpenAccountBusinessStore(filepath.Join(state, "account.db"), instance)
	if err != nil {
		t.Fatal(err)
	}
	defer personal.Close()
	if err := personal.BindAccountBusinessStore(ctx, actor); err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: instance}
	if _, err := settings.Initialize(ctx, work.ModelSettings{BaseURL: provider.URL, ModelID: "browser-fixture", KeyRef: "env:CLUSBOT_TEST_BROWSER_MODEL_KEY", Protocol: model.ProtocolResponses}); err != nil {
		t.Fatal(err)
	}
	if cluskit != nil {
		workersCtx, stop := context.WithCancel(ctx)
		workers := personal.StartWithModelRoles(workersCtx, settings, 1, work.RunLimits{ActiveTime: 20 * time.Second, ModelTimeout: 8 * time.Second, OutputTokens: 512},
			func() bool { return instance.AccountIsActive(workersCtx, actor) }, settings.RoleAvailable)
		defer func() { stop(); workers.Wait() }()
	}
	backend := httptest.NewUnstartedServer(nil)
	addr := backend.Listener.Addr().String()
	origin := "http://" + addr
	server := Server{Store: instance, Settings: settings, Origin: origin, Hosts: []string{addr}}
	personalHandler := server.PersonalHandler(personal)
	server.PersonalAPI = func(account string) (http.Handler, error) {
		if account != actor {
			return nil, work.ErrAccountDenied
		}
		return personalHandler, nil
	}
	handler := server.Handler(assets)
	backend.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/mcp-setup/") && strings.HasSuffix(r.URL.Path, "/operation") {
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			var request struct {
				Step string `json:"step"`
			}
			if err != nil {
				t.Error(err)
			}
			if json.Unmarshal(raw, &request) == nil && request.Step == "probe" {
				probeConfirmed.Store(true)
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		handler.ServeHTTP(w, r)
	})
	backend.Start()
	defer backend.Close()

	args := map[string]any{"origin": origin, "target": target, "username": "admin", "password": "correct horse battery staple", "transport": "http",
		"readTool": "fixture_read", "writeTool": "fixture_write"}
	// Composer, four A2UI choices, three offline checks, one registration, three operations.
	wantPosts := 1 + 4 + 3 + 1 + 3
	if cluskit != nil {
		args["transport"] = "stdio"
		args["readTool"] = "task_status"
		args["writeTool"] = "run"
		args["arguments"] = cluskit.arguments
		args["envNames"] = cluskit.envNames
		args["lookupKey"] = cluskit.lookupKey
		wantPosts++
	}
	input, _ := json.Marshal(args)
	var receipt struct {
		Conversation string `json:"conversation"`
		Run          string `json:"run"`
		Posts        int    `json:"posts"`
	}
	if err := json.Unmarshal(runBrowser("browse", input), &receipt); err != nil || !validID(receipt.Conversation) || receipt.Posts != wantPosts {
		t.Fatalf("invalid browser receipt %+v: %v", receipt, err)
	}
	binding, err := personal.ReadConversationMCPSelection(ctx, actor, receipt.Conversation)
	if err != nil || binding.Selection == nil || binding.Selection.ToolName != args["readTool"] || binding.LocalValidity != "locally_ready" {
		t.Fatalf("missing durable conversation binding: %+v: %v", binding, err)
	}
	if cluskit != nil {
		cluskit.verify(t, ctx, personal, actor, receipt.Conversation, receipt.Run, calls.Load())
		return
	}
	if calls.Load() != 4 || mcpRequests.Load() == 0 || earlyMCP.Load() != 0 || toolCalls.Load() != 0 {
		t.Fatalf("model=%d, MCP handshake=%d, premature MCP requests=%d, business calls=%d", calls.Load(), mcpRequests.Load(), earlyMCP.Load(), toolCalls.Load())
	}
	t.Logf("real UI and Go server: %d model decisions, %d MCP protocol requests after confirmation, no business tool execution", calls.Load(), mcpRequests.Load())
}
