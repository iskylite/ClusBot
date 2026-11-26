package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const admissionPrompt = "跑 POD7 全套基准"

type benchmarkAdmissionFixture struct {
	*discoveryA2UIFixture
	bench      *localIntegrationBenchmark
	modelCalls atomic.Int32
	baseline   []work.Task
}

func newBenchmarkAdmissionFixture(t *testing.T, answer func(*http.Request) string) *benchmarkAdmissionFixture {
	t.Helper()
	fixture := &benchmarkAdmissionFixture{bench: &localIntegrationBenchmark{}}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32768))
		var msg struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err != nil || json.Unmarshal(raw, &msg) != nil || r.Method != "POST" || r.URL.Path != "/responses" ||
			len(msg.Input) != 1 || msg.Input[0].Content != `{"action":"preview_benchmark_admission","scope_kind":"single_bound_plan"}` ||
			strings.Contains(string(raw), "POD7") || strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "node-1") {
			t.Error("model received more than the authorized preview intent")
			http.Error(w, "unexpected egress", 400)
			return
		}
		fixture.modelCalls.Add(1)
		decision := "preview"
		if answer != nil {
			decision = answer(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", decision)
	}))
	t.Cleanup(provider.Close)
	fixture.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(db *work.Store) *model.Settings {
		settings := &model.Settings{Store: db}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "fixture", Protocol: model.ProtocolResponses,
			BaseURL: provider.URL, KeyRef: "env:CLUSBOT_BENCHMARK_PREVIEW_TEST_MODEL_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	f := fixture.discoveryA2UIFixture
	f.server.BenchmarkProvider = fixture.bench
	f.server.AccountBenchmarkDispatch = true
	fixture.attach(f.store)
	ctx := t.Context()
	grant := work.DomainBenchmarkGrant{Connection: "bench", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := f.store.ImportDomainAssetsForAccount(ctx, f.actor, []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "private-fixture-machine", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PublishBenchmarkProfile(ctx, f.actor, work.BenchmarkProfile{Key: "full", Version: 1, Cases: []work.BenchmarkCase{
		{Key: "first", Targets: []string{"node-1"}, Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateBenchmarkChatTarget(ctx, f.actor, "POD7", &work.BenchmarkProfileRef{Profile: "full", Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.baseline, err = f.store.ListTasks(ctx, work.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *benchmarkAdmissionFixture) attach(store *work.Store) {
	f.store = store
	server := f.server
	server.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor != f.actor {
			return nil, work.ErrAccountDenied
		}
		return server.PersonalHandler(store), nil
	}
	f.server = server
	f.account.handler = server.Handler(fstest.MapFS{})
}

func (f *benchmarkAdmissionFixture) issue(t *testing.T, id string) (string, string, map[string]any) {
	t.Helper()
	r := f.request("POST", "/api/v1/conversations", "", map[string]string{"client_request_id": id, "text": admissionPrompt}, f.cookie)
	var suggestion struct {
		Conversation string                          `json:"conversation_id"`
		Run          string                          `json:"run_id"`
		Task         string                          `json:"task_id"`
		Suggestion   *work.BenchmarkRouterSuggestion `json:"benchmark_suggestion"`
	}
	accountJSON(t, r, &suggestion)
	if r.Code != 202 || suggestion.Conversation == "" || suggestion.Run == "" || suggestion.Task != "" || suggestion.Suggestion == nil {
		t.Fatalf("normal composer did not produce a non-executing suggestion: %d %s", r.Code, r.Body)
	}
	path := "/api/v1/conversations/" + suggestion.Conversation + "/a2ui/benchmark-admission/" + suggestion.Run
	view := f.request("GET", path, "", nil, f.cookie)
	var out benchmarkAdmissionReply
	accountJSON(t, view, &out)
	if view.Code != 200 || out.State != "ready" || out.Preview != nil || len(out.Surface) != 1 || out.Run != suggestion.Run {
		t.Fatalf("wrong signed view: %d %s", view.Code, view.Body)
	}
	return path, suggestion.Conversation, renderedA2UIEvent(t, a2uiDiscoveryReply{Surface: out.Surface}, work.BenchmarkAdmissionPreviewAction)
}

func (f *benchmarkAdmissionFixture) noDispatch(t *testing.T) {
	t.Helper()
	tasks, err := f.store.ListTasks(t.Context(), work.TaskFilter{})
	if err != nil || !reflect.DeepEqual(tasks, f.baseline) || f.bench.starts.Load() != 0 || f.bench.queries.Load() != 0 {
		t.Fatalf("preview created tasks or touched transport: tasks %d (before %d), start %d/query %d: %v",
			len(tasks), len(f.baseline), f.bench.starts.Load(), f.bench.queries.Load(), err)
	}
}

func TestBenchmarkAdmissionSurfaceScopedActionAndReopen(t *testing.T) {
	f := newBenchmarkAdmissionFixture(t, nil)
	path, conversation, event := f.issue(t, "intake-scoped")
	fresh := f.account.login("admin", "correct horse battery staple")
	if res := f.request("POST", path, "stale-login", event, fresh); res.Code != 409 {
		t.Fatalf("new login replayed old surface: %d %s", res.Code, res.Body)
	}
	cross := strings.Replace(path, conversation, strings.Repeat("a", 32), 1)
	if res := f.request("POST", cross, "different-conversation", event, f.cookie); res.Code != 404 {
		t.Fatalf("cross-conversation: %d %s", res.Code, res.Body)
	}
	c := event["action"].(map[string]any)["context"].(map[string]any)
	c["target"] = "POD8"
	if res := f.request("POST", path, "forged-action", event, f.cookie); res.Code != 400 {
		t.Fatalf("wrong event body accepted: %d %s", res.Code, res.Body)
	}
	delete(c, "target")
	if f.modelCalls.Load() != 0 {
		t.Fatal("render or forgery triggered the model")
	}
	var previewID string
	for range 2 {
		res := f.request("POST", path, "same-signed-action", event, f.cookie)
		var receipt benchmarkAdmissionReply
		accountJSON(t, res, &receipt)
		if res.Code != 200 || receipt.State != "previewed" || receipt.Preview == nil || receipt.Preview.ExecutionStarted ||
			receipt.Preview.AssetCount != 1 || receipt.Preview.Facility != "not_verified" || receipt.Preview.ID == "" ||
			previewID != "" && previewID != receipt.Preview.ID || len(receipt.Surface) != 0 || receipt.AcceptedTask != "" {
			t.Fatalf("preview missing or executed: %d %s", res.Code, res.Body)
		}
		previewID = receipt.Preview.ID
	}
	f.noDispatch(t)
	// Reconstruct the personal SQLite store and handler, not just the browser tab.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := work.OpenAccountBusinessStore(f.path, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := reopened.BindAccountBusinessStore(t.Context(), f.actor); err != nil {
		t.Fatal(err)
	}
	f.attach(reopened)
	res := f.request("GET", path, "", nil, fresh)
	var restored benchmarkAdmissionReply
	accountJSON(t, res, &restored)
	if res.Code != 200 || restored.Preview == nil || restored.Preview.ID != previewID || f.modelCalls.Load() != 1 {
		t.Fatalf("restore lost or re-ran preview: %d %s model=%d", res.Code, res.Body, f.modelCalls.Load())
	}
	// The same target rebound to the same plan still invalidates the old view.
	one, two := 1, 2
	if err := f.store.UpdateBenchmarkChatTarget(t.Context(), f.actor, "POD7", nil, &one); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateBenchmarkChatTarget(t.Context(), f.actor, "POD7", &work.BenchmarkProfileRef{Profile: "full", Version: 1}, &two); err != nil {
		t.Fatal(err)
	}
	res = f.request("GET", path, "", nil, fresh)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"state":"stale"`) || strings.Contains(res.Body.String(), previewID) {
		t.Fatalf("rebound target recovered an invalid preview: %d %s", res.Code, res.Body)
	}
	f.noDispatch(t)
}

func TestBenchmarkAdmissionSurfaceClarifiesOrRefusesModelAction(t *testing.T) {
	for _, caseData := range []struct {
		answer, expected string
		status           int
	}{
		{"clarify", "clarify", 200}, {`{"action":"start_now"}`, "not_completed", 502},
	} {
		t.Run(caseData.expected, func(t *testing.T) {
			f := newBenchmarkAdmissionFixture(t, func(*http.Request) string { return caseData.answer })
			path, _, event := f.issue(t, "intake-model-outcome")
			res := f.request("POST", path, "record-intent", event, f.cookie)
			if res.Code != caseData.status {
				t.Fatalf("model result: %d %s", res.Code, res.Body)
			}
			res = f.request("GET", path, "", nil, f.cookie)
			if res.Code != 200 || !strings.Contains(res.Body.String(), `"state":"`+caseData.expected+`"`) ||
				strings.Contains(res.Body.String(), "preview_id") || f.modelCalls.Load() != 1 {
				t.Fatalf("unexpected stored outcome: %d %s", res.Code, res.Body)
			}
			f.noDispatch(t)
		})
	}
}

func TestBenchmarkAdmissionSurfaceRechecksAfterModel(t *testing.T) {
	for _, reason := range []string{"logout", "authorization"} {
		t.Run(reason, func(t *testing.T) {
			started, finish := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-finish:
				default:
					close(finish)
				}
			}()
			f := newBenchmarkAdmissionFixture(t, func(r *http.Request) string {
				close(started)
				select {
				case <-finish:
				case <-r.Context().Done():
				}
				return "preview"
			})
			path, _, event := f.issue(t, "inflight-benchmark-suggestion")
			returned := make(chan *httptest.ResponseRecorder, 1)
			go func() { returned <- f.request("POST", path, "inflight-benchmark-action", event, f.cookie) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("model event did not start")
			}
			if reason == "logout" {
				if r := f.request("DELETE", "/api/v1/session", "", nil, f.cookie); r.Code != 204 {
					t.Fatalf("failed to logout: %d %s", r.Code, r.Body)
				}
			} else {
				db, err := sql.Open("sqlite", f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND object_ref='node-1'", time.Now().UnixMilli(), f.actor); err != nil {
					t.Fatal(err)
				}
			}
			close(finish)
			select {
			case r := <-returned:
				if r.Code != 403 {
					t.Fatalf("revoked preview accepted: %d %s", r.Code, r.Body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("revoked action did not settle")
			}
			db, err := sql.Open("sqlite", f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var n int
			if err := db.QueryRow("SELECT count(*) FROM benchmark_admission_previews").Scan(&n); err != nil || n != 0 {
				t.Fatalf("revoked user left a confirmed preview: %d %v", n, err)
			}
			f.noDispatch(t)
		})
	}
}

func TestBenchmarkAdmissionSurfaceBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local Playwright module and installed browser")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browse := func(stage string, input []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v\n%s", stage, err, stderr.String())
		}
		return data
	}
	var files map[string][]byte
	if err := json.Unmarshal(browse("bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("missing current app bundle: %v", err)
	}
	appAssets := fstest.MapFS{}
	for name, data := range files {
		appAssets[name] = &fstest.MapFile{Data: data}
	}
	f := newBenchmarkAdmissionFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	api := f.server
	api.Origin, api.Hosts = origin, []string{addr}
	server.Config.Handler = api.Handler(appAssets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "prompt": admissionPrompt})
	browse("browse-benchmark-admission", input)
	if f.modelCalls.Load() != 1 {
		t.Fatalf("browser readback or later turns called model: %d", f.modelCalls.Load())
	}
	f.noDispatch(t)
}
