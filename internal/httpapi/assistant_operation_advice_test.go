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

const adviceFixtureText = "看一下 POD7 节点并给出排查建议。"
const adviceSecondLabel = "比较近期记录"

type adviceFixture struct {
	*discoveryA2UIFixture
	prepare, selects atomic.Int32
	baselineTasks    []work.Task
}

func setupAdviceFixture(t *testing.T, gate func(*http.Request) string) *adviceFixture {
	t.Helper()
	f := &adviceFixture{}
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var outer struct {
			Input []struct{ Content string }
			Tools json.RawMessage
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&outer); err != nil || len(outer.Input) != 1 || len(outer.Tools) != 0 {
			t.Errorf("non-bounded model input: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		content := outer.Input[0].Content
		for _, value := range []string{"POD7", "test-pod-seven", "test-node-seven", "fixture-hidden-identity", adviceSecondLabel, adviceFixtureText, "http://"} {
			if strings.Contains(content, value) {
				t.Errorf("private source field reached model: %q", value)
			}
		}
		answer := `{"tool":"propose_operation_options","body":{"title":"排查建议","options":[{"label":"核对健康摘要","recommended":true},{"label":"比较近期记录","detail":"这只是调查方向。"}]}}`
		if strings.Contains(content, assistantAdviceActionName) {
			f.selects.Add(1)
			var action map[string]string
			if err := json.Unmarshal([]byte(content), &action); err != nil || len(action) != 4 || action["action"] != assistantAdviceActionName ||
				action["scope_kind"] != "pod" || action["focus_kind"] != "node_investigation" || action["choice_slot"] != "2" {
				t.Errorf("model did not receive only the scoped enums: %+v %v", action, err)
			}
			answer = "record_selection"
			if gate != nil {
				answer = gate(r)
			}
		} else {
			f.prepare.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	t.Cleanup(modelServer.Close)
	const envKey = "CLUSBOT_ADVICE_LOCAL_TEST_KEY"
	t.Setenv(envKey, "fixture-local-only")
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "advice-fixture", Protocol: model.ProtocolResponses, BaseURL: modelServer.URL, KeyRef: "env:" + envKey}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	if err := f.store.ImportDomainAssetsForAccount(t.Context(), f.actor, []work.DomainAssetEntry{
		{ObjectRef: "test-pod-seven", Kind: "pod", Identity: "POD7", Read: true},
		{ObjectRef: "test-node-seven", Kind: "node", Identity: "fixture-hidden-identity", PodRef: "test-pod-seven", Read: true},
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ref := range []string{"test-pod-seven", "test-node-seven"} {
		if _, err = db.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at)
VALUES(?,?,'export_model',?,0,'fixture',0,0)`, "export-"+ref, f.actor, ref); err != nil {
			t.Fatal(err)
		}
	}
	f.baselineTasks, err = f.store.ListTasks(t.Context(), work.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *adviceFixture) open(t *testing.T) (string, string, string) {
	t.Helper()
	const requestKey = "advice-origin-source-key"
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "https://ops.example/api/v1/conversations", strings.NewReader(`{"text":"`+adviceFixtureText+`","client_message_id":"`+requestKey+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://ops.example")
		req.Header.Set("X-ClusBot-Assistant-Cards", "1")
		req.AddCookie(f.cookie)
		rec := httptest.NewRecorder()
		f.account.handler.ServeHTTP(rec, req)
		return rec
	}
	var expected conversationAdviceOrigin
	for i := 0; i < 2; i++ {
		res := send()
		if res.Code != 202 {
			t.Fatalf("advice not anchored to chat: %d %s", res.Code, res.Body)
		}
		var got conversationAdviceOrigin
		accountJSON(t, res, &got)
		if !validID(got.Conversation) || !validID(got.Run) || i != 0 && got != expected {
			t.Fatalf("unexpected original receipt: %+v", got)
		}
		expected = got
	}
	base := "/api/v1/conversations/" + expected.Conversation
	history := f.request("GET", base+"/history", "", nil, f.cookie)
	if history.Code != 200 || !strings.Contains(history.Body.String(), `"assistant_operation_advice":true`) || f.prepare.Load() != 1 {
		t.Fatalf("original history missing/doubled: %d %s, models %d", history.Code, history.Body, f.prepare.Load())
	}
	return base + "/operation-advice/" + expected.Run, expected.Conversation, expected.Run
}

type conversationAdviceOrigin struct {
	Conversation string `json:"conversation_id"`
	Run          string `json:"run_id"`
}

func (f *adviceFixture) action(t *testing.T, path string) map[string]any {
	t.Helper()
	reply := f.request("GET", path, "", nil, f.cookie)
	if reply.Code != 200 {
		t.Fatalf("read advice: %d %s", reply.Code, reply.Body)
	}
	var parsed a2uiDiscoveryReply
	accountJSON(t, reply, &parsed)
	// Use the exact signed A2UI button the client displays for slot 2.
	surface := parsed.Surface[0]["createSurface"].(map[string]any)
	for _, raw := range surface["components"].([]any) {
		node := raw.(map[string]any)
		if node["component"] != "Button" {
			continue
		}
		event := node["action"].(map[string]any)["event"].(map[string]any)
		ctx := event["context"].(map[string]any)
		if event["name"] == assistantAdviceActionName && ctx["advice_slot"] == "2" {
			return map[string]any{"version": "1.0", "action": map[string]any{
				"name": assistantAdviceActionName, "surfaceId": surface["surfaceId"], "sourceComponentId": node["id"], "context": ctx,
				"timestamp": time.Now().UTC().Format(time.RFC3339Nano)}}
		}
	}
	t.Fatal("missing signed button")
	return nil
}

func (f *adviceFixture) noBusinessWrite(t *testing.T) {
	t.Helper()
	state, err := f.store.DomainSnapshot(t.Context())
	tasks, taskErr := f.store.ListTasks(t.Context(), work.TaskFilter{})
	if err != nil || taskErr != nil || len(state.Assets) != 2 || len(state.Operations) != 0 || len(state.AssetTransitions) != 0 ||
		len(state.Proposals) != 0 || len(state.Tickets) != 0 || !reflect.DeepEqual(tasks, f.baselineTasks) {
		t.Fatalf("advice created a task or mutated domain records: counts=%v tasks=%d (before %d) %v %v", state.Counts, len(tasks), len(f.baselineTasks), err, taskErr)
	}
}

func TestAssistantOperationAdviceScopedChoiceAndReceipt(t *testing.T) {
	f := setupAdviceFixture(t, nil)
	path, conversation, run := f.open(t)
	action := f.action(t, path)
	fresh := f.account.login("admin", "correct horse battery staple")
	if out := f.request("POST", path+"/actions", "outside-original-login", action, fresh); out.Code != 409 {
		t.Fatalf("new session replayed old surface: %d %s", out.Code, out.Body)
	}
	envelope := action["action"].(map[string]any)
	fields := envelope["context"].(map[string]any)
	fields["advice_slot"] = "1"
	if out := f.request("POST", path+"/actions", "different-option-token", action, f.cookie); out.Code != 409 {
		t.Fatalf("changed choice accepted under old button: %d %s", out.Code, out.Body)
	}
	fields["advice_slot"] = "2"
	fields["command"] = "ignored by this fixture"
	if out := f.request("POST", path+"/actions", "no-hidden-business-input", action, f.cookie); out.Code != 400 {
		t.Fatalf("business input accepted: %d %s", out.Code, out.Body)
	}
	delete(fields, "command")
	otherPath := strings.Replace(path, conversation, strings.Repeat("e", 32), 1)
	if out := f.request("POST", otherPath+"/actions", "outside-advice-conversation", action, f.cookie); out.Code != 404 {
		t.Fatalf("cross-conversation action code: %d %s", out.Code, out.Body)
	}
	if f.selects.Load() != 0 {
		t.Fatal("forged action reached model")
	}
	for i := 0; i < 2; i++ {
		res := f.request("POST", path+"/actions", "same-original-choice", action, f.cookie)
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"state":"recorded"`) {
			t.Fatalf("choice not recorded: %d %s", res.Code, res.Body)
		}
	}
	read := f.request("GET", path, "", nil, fresh)
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"slot":"2"`) || f.selects.Load() != 1 {
		t.Fatalf("resume replayed model or lost choice: %d %s %d", read.Code, read.Body, f.selects.Load())
	}
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Data, adviceSecondLabel) || strings.Contains(e.Data, "test-node-seven") {
			t.Fatal("private advice data entered replay stream")
		}
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='read' AND object_ref='test-node-seven'", time.Now().UnixMilli(), f.actor); err != nil {
		t.Fatal(err)
	}
	if read = f.request("GET", path, "", nil, f.cookie); read.Code != 403 || strings.Contains(read.Body.String(), adviceSecondLabel) {
		t.Fatalf("retained advice disclosed revoked facts: %d %s", read.Code, read.Body)
	}
	f.noBusinessWrite(t)
}

func TestAssistantOperationAdviceCannotRecordAfterLogout(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	f := setupAdviceFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		return "request_details"
	})
	path, _, _ := f.open(t)
	a := f.action(t, path)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.request("POST", path+"/actions", "revoked-advice-choice", a, f.cookie) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(finish)
		t.Fatal("model did not receive selection")
	}
	out := f.request("DELETE", "/api/v1/session", "", nil, f.cookie)
	close(finish)
	if out.Code != 204 {
		t.Fatalf("logout failed: %d %s", out.Code, out.Body)
	}
	select {
	case ok := <-result:
		if ok.Code != 403 {
			t.Fatalf("logged out choice stored: %d %s", ok.Code, ok.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logged out action did not settle")
	}
	newCookie := f.account.login("admin", "correct horse battery staple")
	r := f.request("GET", path, "", nil, newCookie)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"not_completed"`) || strings.Contains(r.Body.String(), `"choice":`) {
		t.Fatalf("revoked login left a confirmed choice: %d %s", r.Code, r.Body)
	}
	f.noBusinessWrite(t)
}

func TestAssistantOperationAdviceRejectsAnActionFromModel(t *testing.T) {
	f := setupAdviceFixture(t, func(*http.Request) string { return `{"action":"submit_operation"}` })
	path, _, _ := f.open(t)
	button := f.action(t, path)
	out := f.request("POST", path+"/actions", "no-model-approval", button, f.cookie)
	if out.Code != 502 {
		t.Fatalf("unrecognized model result was accepted: %d %s", out.Code, out.Body)
	}
	read := f.request("GET", path, "", nil, f.cookie)
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"state":"not_completed"`) || f.selects.Load() != 1 {
		t.Fatalf("unsafe choice persisted or replayed: %d %s", read.Code, read.Body)
	}
	f.noBusinessWrite(t)
}

func TestAssistantOperationAdviceBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set installed playwright module and browser executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	run := func(mode string, stdin []byte) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", mode)
		command.Dir = web
		command.Stdin = bytes.NewReader(stdin)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		result, err := command.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", mode, err, stderr.String())
		}
		return result
	}
	var files map[string][]byte
	if err := json.Unmarshal(run("bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("invalid web bundle: %v", err)
	}
	assets := fstest.MapFS{}
	for path, data := range files {
		assets[path] = &fstest.MapFile{Data: data}
	}
	f := setupAdviceFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	backend := f.server
	backend.Origin = origin
	backend.Hosts = []string{addr}
	server.Config.Handler = backend.Handler(assets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "prompt": adviceFixtureText, "label": adviceSecondLabel})
	run("browse-operation-advice", input)
	if f.prepare.Load() != 1 || f.selects.Load() != 1 {
		t.Fatalf("restore sent new model traffic: %d/%d", f.prepare.Load(), f.selects.Load())
	}
	f.noBusinessWrite(t)
}
