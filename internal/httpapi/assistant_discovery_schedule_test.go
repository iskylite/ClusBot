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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const assistantSchedulePrompt = "查看定时发现状态"
const assistantSchedulePlan = "a2ui-fixture-plan"

type scheduleReadFixtureSource struct{ scans atomic.Int32 }

func (*scheduleReadFixtureSource) AllowsDiscovery(source, scope string) bool {
	return source == "fixture-local" && scope == "fixture-private-scope"
}

func (s *scheduleReadFixtureSource) ScanDiscovery(context.Context, string, string) ([]work.DiscoveryObservation, error) {
	s.scans.Add(1)
	return nil, fmt.Errorf("read-only schedule preview must not call discovery")
}

type assistantScheduleFixture struct {
	*discoveryA2UIFixture
	source scheduleReadFixtureSource
	calls  atomic.Int32
}

func makeAssistantScheduleFixture(t *testing.T, next func(*http.Request) string) *assistantScheduleFixture {
	t.Helper()
	f := &assistantScheduleFixture{}
	t.Setenv("CLUSBOT_SCHEDULE_VIEW_FIXTURE_KEY", "loopback-scheduled-model")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		var req struct{ Input []struct{ Content string } }
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(raw, &req) != nil || len(req.Input) != 1 ||
			req.Input[0].Content != `{"requested_action":"preview_discovery_schedule","scope_kind":"existing_discovery_plan"}` ||
			bytes.Contains(raw, []byte(assistantSchedulePlan)) || bytes.Contains(raw, []byte("fixture-local")) ||
			bytes.Contains(raw, []byte("fixture-private-scope")) || bytes.Contains(raw, []byte("192.0.2.184")) {
			t.Error("unexpected or private timer data reached the model")
			http.Error(w, "not a permitted request", 400)
			return
		}
		f.calls.Add(1)
		out := `{"next":"show_schedule"}`
		if next != nil {
			out = next(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	t.Cleanup(provider.Close)
	// The shared fixture has already advanced an authorized real timer
	// occurrence and shut its worker down. Any new scan would hit the trap.
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "schedule-view-fixture", Protocol: model.ProtocolResponses, BaseURL: provider.URL,
			KeyRef: "env:CLUSBOT_SCHEDULE_VIEW_FIXTURE_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	s := f.server
	s.DiscoverySource = &f.source
	s.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor != f.actor {
			return nil, work.ErrAccountDenied
		}
		return s.PersonalHandler(f.store), nil
	}
	f.server = s
	f.account.handler = s.Handler(nil)
	return f
}

func (f *assistantScheduleFixture) open(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": assistantSchedulePrompt, "client_message_id": "fixture-schedule-open"})
	r := httptest.NewRequest("POST", "https://ops.example/api/v1/conversations", bytes.NewReader(raw))
	r.Header.Set("Origin", "https://ops.example")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-ClusBot-Assistant-Cards", "1")
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.account.handler.ServeHTTP(w, r)
	var ids struct {
		Conversation string `json:"conversation_id"`
		Run          string `json:"run_id"`
		Task         string `json:"task_id"`
	}
	accountJSON(t, w, &ids)
	if w.Code != 202 || ids.Conversation == "" || ids.Run == "" || ids.Task != "" {
		t.Fatalf("failed to locate original chat view: %d %s", w.Code, w.Body)
	}
	path := "/api/v1/conversations/" + ids.Conversation + "/schedule-card/" + ids.Run
	res := f.request("GET", path, "", nil, f.cookie)
	var card a2uiDiscoveryReply
	accountJSON(t, res, &card)
	if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" || strings.Contains(res.Body.String(), assistantSchedulePlan) ||
		!strings.Contains(res.Body.String(), "展开计划和最近回执") || f.calls.Load() != 0 {
		t.Fatalf("snapshot disclosed plan before scoped selection: %d %s", res.Code, res.Body)
	}
	return path, ids.Run, renderedA2UIEvent(t, card, assistantDiscoveryScheduleAction)
}

func TestAssistantDiscoveryScheduleProvenanceAndNoScan(t *testing.T) {
	f := makeAssistantScheduleFixture(t, nil)
	path, run, action := f.open(t)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current := func() string {
		t.Helper()
		var shape string
		if err := db.QueryRow(`SELECT json_array(
(SELECT count(*) FROM discovery_plans),(SELECT count(*) FROM discovery_occurrences),
(SELECT count(*) FROM discovery_observations),(SELECT count(*) FROM authorizations),
(SELECT count(*) FROM tasks),(SELECT count(*) FROM inbox_events),
(SELECT next_at FROM discovery_plans WHERE plan_id=?))`, assistantSchedulePlan).Scan(&shape); err != nil {
			t.Fatal(err)
		}
		return shape
	}
	before := current()
	fresh := f.account.login("admin", "correct horse battery staple")
	if res := f.request("POST", path+"/actions", "schedule-other-login", action, fresh); res.Code != 409 {
		t.Fatalf("other session used scope action: %d %s", res.Code, res.Body)
	}
	fields := action["action"].(map[string]any)["context"].(map[string]any)
	fields["plan_id"] = "not-authorized"
	if res := f.request("POST", path+"/actions", "schedule-raw-plan", action, f.cookie); res.Code != 400 {
		t.Fatalf("client-provided plan accepted: %d %s", res.Code, res.Body)
	}
	delete(fields, "plan_id")
	res := f.request("POST", path+"/actions", "schedule-valid-read", action, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "每 60 秒") || !strings.Contains(res.Body.String(), " UTC") ||
		!strings.Contains(res.Body.String(), "这次发现已记录 1 条观测") || strings.Contains(res.Body.String(), "192.0.2.184") {
		t.Fatalf("unproven or private receipt: %d %s", res.Code, res.Body)
	}
	if res := f.request("GET", path, "", nil, f.cookie); res.Code != 200 || strings.Contains(res.Body.String(), "每 60 秒") || f.calls.Load() != 1 {
		t.Fatalf("reopened card replayed model or showed old data: %d %s", res.Code, res.Body)
	}
	if f.source.scans.Load() != 0 || current() != before {
		t.Fatal("read path changed schedules, authorization or triggered discovery")
	}
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "assistant.discovery_schedule_requested" && e.Data != "{}" {
			t.Fatal("internal plan anchor leaked through generic replay")
		}
	}

	// A newer live plan under the same scope may never replace this card's
	// selected plan after the original is stopped.
	if err := f.store.PublishDiscoveryPlan(t.Context(), f.actor, work.DiscoveryPlanRequest{ID: "fixture-second-plan", Source: "fixture-local", Scope: "fixture-private-scope",
		PeriodSeconds: 120, FirstAt: time.Now().Add(time.Hour).UnixMilli()}, &f.source); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DisableDiscoveryPlan(t.Context(), f.actor, assistantSchedulePlan); err != nil {
		t.Fatal(err)
	}
	res = f.request("GET", path, "", nil, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "原计划已变化或不可读取") ||
		strings.Contains(res.Body.String(), "fixture-second-plan") || strings.Contains(res.Body.String(), "scheduleview_") {
		t.Fatalf("stale card silently selected a new plan: %d %s", res.Code, res.Body)
	}
	if res := f.request("POST", path+"/actions", "schedule-stale-card", action, f.cookie); res.Code != 409 || f.calls.Load() != 1 {
		t.Fatalf("old action queried newly enabled plan: %d %s", res.Code, res.Body)
	}
}

func TestAssistantDiscoveryScheduleRechecksAdministratorAfterModel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := makeAssistantScheduleFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return `{"next":"show_schedule"}`
	})
	path, _, action := f.open(t)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.request("POST", path+"/actions", "schedule-revoked-role", action, f.cookie) }()
	select {
	case <-started:
	case <-time.After(6 * time.Second):
		t.Fatal("model did not receive scoped request")
	}
	// Test only: the ordinary account can still reach its own business DB,
	// but may not keep an administrator-only plan view after demotion.
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.path), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE accounts SET is_superadmin=0 WHERE principal_id=?", f.actor); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case res := <-result:
		if res.Code != 403 || strings.Contains(res.Body.String(), assistantSchedulePlan) || f.source.scans.Load() != 0 {
			t.Fatalf("old administrator privileges still authorized: %d %s", res.Code, res.Body)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("authorization recheck did not finish")
	}
	if res := f.request("GET", path, "", nil, f.cookie); res.Code != 403 {
		t.Fatalf("ordinary account opened administrator view: %d %s", res.Code, res.Body)
	}
}

func TestAssistantDiscoveryScheduleBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local browser tooling")
	}
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	defer stop()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(stage string, input []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", stage, err, stderr.String())
		}
		return out
	}
	var bundle map[string][]byte
	if err := json.Unmarshal(browser("bundle", nil), &bundle); err != nil || len(bundle["index.html"]) == 0 {
		t.Fatalf("failed to bundle app: %v", err)
	}
	assets := fstest.MapFS{}
	for name, data := range bundle {
		assets[name] = &fstest.MapFile{Data: data}
	}
	f := makeAssistantScheduleFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	actual := f.server
	actual.Origin, actual.Hosts = origin, []string{addr}
	server.Config.Handler = actual.Handler(assets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "prompt": assistantSchedulePrompt})
	_ = browser("browse-discovery-schedule", input)
	if f.calls.Load() != 1 || f.source.scans.Load() != 0 {
		t.Fatalf("dashboard triggered discovery or replayed the model: %d %d", f.calls.Load(), f.source.scans.Load())
	}
}
