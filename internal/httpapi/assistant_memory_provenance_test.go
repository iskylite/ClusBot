package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

const memorySourceHere = "本会话合成来源 <em>不应解释为 HTML</em>"
const memorySourceAway = "另一个会话的合成来源，不应进本卡"
const memoryContentFixture = "独立的合成记忆内容，不用于扩展请求"
const memoryLocalTitle = "同会话来源夹具"
const memoryOtherTitle = "其他会话来源夹具"

type assistantMemoryFixture struct {
	*discoveryA2UIFixture
	run      work.Run
	local    work.Memory
	other    work.Memory
	otherRun work.Run
	calls    atomic.Int32
}

func produceAssistantMemoryReply(t *testing.T, s *work.Store, key, request, conversation string) (work.Run, int64) {
	t.Helper()
	ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
	defer stop()
	run, err := s.Submit(ctx, key, request, "", conversation, false)
	if err != nil {
		t.Fatal(err)
	}
	workers := s.Start(ctx, assistantArtifactFixtureWorker("由本地夹具产生的原回复，不使用记忆正文。"), 1, work.RunLimits{
		ActiveTime: 5 * time.Second, ModelTimeout: 4 * time.Second, OutputTokens: 1024,
	})
	defer func() { stop(); workers.Wait() }()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("fixture reply never completed: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
			result, err := s.Detail(run.ConversationID, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Run == nil || result.Run.ID != run.ID || result.Run.Status != "completed" {
				continue
			}
			for _, m := range result.Messages {
				if m.RunID == run.ID && m.Role == "user" && m.Text == request {
					return run, m.ID
				}
			}
			t.Fatal("original user message did not persist")
		}
	}
}

func makeAssistantMemoryFixture(t *testing.T, reply func(*http.Request) string) *assistantMemoryFixture {
	t.Helper()
	f := &assistantMemoryFixture{}
	t.Setenv("CLUSBOT_MEMORY_REVIEW_FIXTURE_KEY", "local-memory-review-credential")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32768))
		var query struct{ Input []struct{ Content string } }
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(raw, &query) != nil || len(query.Input) != 1 ||
			query.Input[0].Content != `{"requested_action":"review_memory_provenance","scope_kind":"original_reply"}` {
			t.Error("private data or unexpected input in model request")
			http.Error(w, "unexpected fixture model request", 400)
			return
		}
		for _, forbidden := range []string{memorySourceHere, memorySourceAway, memoryLocalTitle, memoryOtherTitle, memoryContentFixture, "private-browser-memo", "memory_slot"} {
			if strings.Contains(string(raw), forbidden) {
				t.Error("private provenance data reached the model")
				http.Error(w, "private provenance data", 400)
				return
			}
		}
		f.calls.Add(1)
		out := `{"next":"show_source"}`
		if reply != nil {
			out = reply(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	t.Cleanup(provider.Close)
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "memory-provenance-fixture", Protocol: model.ProtocolResponses,
			BaseURL: provider.URL, KeyRef: "env:CLUSBOT_MEMORY_REVIEW_FIXTURE_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	otherRun, otherSource := produceAssistantMemoryReply(t, f.store, "memory-away-source", memorySourceAway, "")
	f.otherRun = otherRun
	localRun, localSource := produceAssistantMemoryReply(t, f.store, "memory-local-source", memorySourceHere+"\n"+strings.Repeat("甲", 180)+"END_AFTER_FRAGMENT", "")
	save := func(title string, source int64) work.Memory {
		item, err := f.store.SaveMemory(t.Context(), "", work.Memory{Kind: "preference", Title: title, Content: memoryContentFixture}, source)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	f.other = save(memoryOtherTitle, otherSource)
	save("手动录入夹具", 0)
	f.local = save(memoryLocalTitle, localSource)
	f.run, _ = produceAssistantMemoryReply(t, f.store, "memory-review-original", "给我整理一段回复", localRun.ConversationID)
	d, err := f.store.Detail(f.run.ConversationID, false)
	if err != nil || len(d.MemoryRefs) != 3 {
		t.Fatalf("original reply did not record its real runtime memory selection: %+v %v", d.MemoryRefs, err)
	}
	return f
}

func (f *assistantMemoryFixture) scope(t *testing.T) (string, map[string]any) {
	t.Helper()
	path := "/api/v1/conversations/" + f.run.ConversationID + "/memory-card/" + f.run.ID
	res := f.request("GET", path, "", nil, f.cookie)
	var card a2uiDiscoveryReply
	accountJSON(t, res, &card)
	if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Body.String(), "来源位于其他会话，此处不展开") || !strings.Contains(res.Body.String(), "无关联的来源消息") {
		t.Fatalf("incorrect original provenance: %d %s", res.Code, res.Body)
	}
	for _, unwanted := range []string{memorySourceHere, memorySourceAway, memoryContentFixture, memoryOtherTitle, f.local.ID, f.other.ID} {
		if strings.Contains(res.Body.String(), unwanted) {
			t.Fatalf("unverified original content/identity surfaced: %q", unwanted)
		}
	}
	return path, renderedA2UIEvent(t, card, assistantMemoryReviewAction)
}

func TestAssistantMemoryProvenanceScopedNoWrite(t *testing.T) {
	f := makeAssistantMemoryFixture(t, nil)
	path, action := f.scope(t)
	before, _, err := f.store.ReplayRunEvents(f.run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	inboxCount := func() int {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM inbox_events").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeInbox := inboxCount()
	fresh := f.account.login("admin", "correct horse battery staple")
	if res := f.request("POST", path+"/actions", "memory-cross-login", action, fresh); res.Code != 409 {
		t.Fatalf("new session used another login's action: %d %s", res.Code, res.Body)
	}
	otherConversation := strings.Replace(path, f.run.ConversationID, f.otherRun.ConversationID, 1)
	if res := f.request("POST", otherConversation+"/actions", "memory-cross-thread", action, f.cookie); res.Code != 404 {
		t.Fatalf("cross-thread read was not rejected: %d %s", res.Code, res.Body)
	}
	if _, err := f.store.ReadAssistantMemoryProvenance(t.Context(), "user:fixture-other-owner", f.run.ConversationID, f.run.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another private account could read references: %v", err)
	}
	a := action["action"].(map[string]any)
	fields := a["context"].(map[string]any)
	fields["memory_id"] = f.other.ID
	if res := f.request("POST", path+"/actions", "memory-injected-reference", action, f.cookie); res.Code != 400 {
		t.Fatalf("raw memory ID accepted: %d %s", res.Code, res.Body)
	}
	delete(fields, "memory_id")
	initialSlot := fields["memory_slot"]
	fields["memory_slot"] = "16"
	if res := f.request("POST", path+"/actions", "memory-altered-slot", action, f.cookie); res.Code != 409 {
		t.Fatalf("altered selection was accepted: %d %s", res.Code, res.Body)
	}
	fields["memory_slot"] = initialSlot
	if f.calls.Load() != 0 {
		t.Fatal("invalid event reached the model")
	}
	a["userMessage"] = "private-browser-memo"
	res := f.request("POST", path+"/actions", "memory-show-excerpt", action, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "不应解释为 HTML") || !strings.Contains(res.Body.String(), "仅显示前 180 个字符") ||
		strings.Contains(res.Body.String(), "END_AFTER_FRAGMENT") || strings.Contains(res.Body.String(), memorySourceAway) || strings.Contains(res.Body.String(), memoryContentFixture) {
		t.Fatalf("unsafe excerpt or unexpected scope: %d %s", res.Code, res.Body)
	}
	if res := f.request("GET", path, "", nil, f.cookie); res.Code != 200 || strings.Contains(res.Body.String(), "不应解释为 HTML") || f.calls.Load() != 1 {
		t.Fatalf("reopening restored the sensitive excerpt or called model: %d %s", res.Code, res.Body)
	}
	after, _, err := f.store.ReplayRunEvents(f.run.ID, 0)
	items, memErr := f.store.Memories(t.Context())
	if afterInbox := inboxCount(); afterInbox != beforeInbox {
		t.Fatalf("read action left a durable receipt: inbox count %d→%d", beforeInbox, afterInbox)
	}
	if err != nil || memErr != nil || len(before) != len(after) || len(items) != 3 {
		t.Fatalf("read changed the original Run or personal memory: %d/%d %v %v", len(before), len(after), err, memErr)
	}
	for _, item := range items {
		if item.Revision != 1 || item.Content != memoryContentFixture {
			t.Fatal("preview changed personal memory")
		}
	}

	// Read metadata only for a source removed from another conversation;
	// preserve the actual memory entry and show why its source isn't visible.
	otherDetails, err := f.store.Detail(f.otherRun.ConversationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteConversation(t.Context(), f.otherRun.ConversationID, otherDetails.Conversation.Revision); err != nil {
		t.Fatal(err)
	}
	if res := f.request("GET", path, "", nil, f.cookie); res.Code != 200 || !strings.Contains(res.Body.String(), "来源已撤回") ||
		strings.Contains(res.Body.String(), memoryOtherTitle) || strings.Contains(res.Body.String(), memorySourceAway) {
		t.Fatalf("removed source leaked or missing state: %d %s", res.Code, res.Body)
	}
}

func TestAssistantMemoryProvenanceRechecksChangedVersionAfterModel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := makeAssistantMemoryFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return `{"next":"show_source"}`
	})
	path, action := f.scope(t)
	replies := make(chan *httptest.ResponseRecorder, 1)
	go func() { replies <- f.request("POST", path+"/actions", "memory-revise-during-model", action, f.cookie) }()
	select {
	case <-started:
	case <-time.After(6 * time.Second):
		t.Fatal("controlled model never received action")
	}
	change := f.local
	change.Title = "更新后的合成标题"
	change.Content = "新版本的合成记忆内容"
	edited, err := f.store.SaveMemory(t.Context(), change.ID, change, 0)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case res := <-replies:
		if res.Code != 409 || strings.Contains(res.Body.String(), "不应解释为 HTML") {
			t.Fatalf("changed source was disclosed using old action: %d %s", res.Code, res.Body)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("scope recheck did not finish")
	}
	res := f.request("GET", path, "", nil, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "当前为第 2 版，原引用与当前版本不同") ||
		strings.Contains(res.Body.String(), memoryLocalTitle) || strings.Contains(res.Body.String(), change.Title) ||
		strings.Contains(res.Body.String(), "memorysource_") {
		t.Fatalf("stale or private reference was rendered: %d %s", res.Code, res.Body)
	}
	if err := f.store.DeleteMemory(t.Context(), edited.ID, edited.Revision); err != nil {
		t.Fatal(err)
	}
	res = f.request("GET", path, "", nil, f.cookie)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "记忆已删除，不再展示来源") || strings.Contains(res.Body.String(), memoryLocalTitle) ||
		strings.Contains(res.Body.String(), "不应解释为 HTML") || f.calls.Load() != 1 {
		t.Fatalf("deleted memory still exposed: %d %s", res.Code, res.Body)
	}
}

func TestAssistantMemoryProvenanceBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local browser tooling")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, src, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(src), "..", "..", "web")
	browser := func(stage string, in []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(in)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", stage, err, stderr.String())
		}
		return out
	}
	var files map[string][]byte
	if err := json.Unmarshal(browser("bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("could not bundle app in memory: %v", err)
	}
	assets := fstest.MapFS{}
	for path, data := range files {
		assets[path] = &fstest.MapFile{Data: data}
	}
	f := makeAssistantMemoryFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	real := f.server
	real.Origin, real.Hosts = origin, []string{addr}
	server.Config.Handler = real.Handler(assets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "conversation": f.run.ConversationID, "run": f.run.ID, "title": memoryLocalTitle})
	_ = browser("browse-memory-provenance", input)
	if f.calls.Load() != 1 {
		t.Fatalf("view returned or resent to model without an explicit action: %d", f.calls.Load())
	}
}
