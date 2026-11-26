package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

const modelReceiptPrompt = "你好，请把本地夹具问候写成一句话。"
const modelReceiptReply = "这是保存到原消息的本地问候。"
const modelReceiptCredential = "local-private-run-receipt-secret"

type modelReceiptFixture struct {
	*discoveryA2UIFixture
	calls atomic.Int32
}

func setupModelReceiptFixture(t *testing.T) *modelReceiptFixture {
	t.Helper()
	const environment = "CLUSBOT_TEST_RUN_RECEIPT_MODEL_KEY"
	t.Setenv(environment, modelReceiptCredential)
	f := &modelReceiptFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var in struct{ Instructions string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+modelReceiptCredential {
			t.Error("unexpected local model request")
			http.Error(w, "invalid fixture request", 400)
			return
		}
		text, modelID, inTokens, outTokens := modelReceiptReply, "vendor/reply-actual", 39, 0
		if strings.Contains(in.Instructions, "你是 ClusBot Router") {
			text, modelID, inTokens, outTokens = `{"kind":"chat"}`, "vendor/router-actual", 7, 4
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":%q,\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n", text, modelID, inTokens, outTokens)
	}))
	t.Cleanup(provider.Close)
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		_, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "fixture-requested-model", Protocol: model.ProtocolResponses,
			BaseURL: provider.URL, KeyRef: "env:" + environment})
		if err != nil {
			t.Fatal(err)
		}
		return settings
	})
	ctx, stop := context.WithCancel(t.Context())
	workers := f.store.Start(ctx, f.server.Settings, 1, work.RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: 4 * time.Second, OutputTokens: 1024})
	t.Cleanup(func() { stop(); workers.Wait() })
	return f
}

func (f *modelReceiptFixture) savedReply(t *testing.T, key, conversation string) (work.Run, work.Message, work.Message) {
	t.Helper()
	run, err := f.store.Submit(t.Context(), key, modelReceiptPrompt, "", conversation, false)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		view, err := f.store.Detail(run.ConversationID, false)
		if err != nil {
			t.Fatal(err)
		}
		if view.Run != nil && view.Run.ID == run.ID && view.Run.Status == "completed" {
			var prompt, reply work.Message
			for _, m := range view.Messages {
				if m.RunID == run.ID && m.Role == "user" {
					prompt = m
				} else if m.RunID == run.ID && m.Role == "assistant" {
					reply = m
				}
			}
			if prompt.ID == 0 || prompt.HasModelReceipt || reply.ID == 0 || !reply.HasModelReceipt {
				t.Fatalf("receipt marker exists only on an actual persisted reply: prompt=%+v reply=%+v", prompt, reply)
			}
			return run, reply, prompt
		}
	}
	t.Fatal("controlled local reply was not saved")
	return work.Run{}, work.Message{}, work.Message{}
}

func modelReceiptPath(conversation, run string, message int64) string {
	return fmt.Sprintf("/api/v1/conversations/%s/runs/%s/model-receipt/%d", conversation, run, message)
}

func modelReceiptReadSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var value string
	if err := db.QueryRow(`SELECT json_array(
(SELECT count(*) FROM runs),(SELECT count(*) FROM messages),(SELECT count(*) FROM run_events),
(SELECT count(*) FROM inbox_events),(SELECT count(*) FROM tasks),(SELECT count(*) FROM context_manifests),
(SELECT count(*) FROM model_receipts),(SELECT coalesce(sum(model_requests),0) FROM runs))`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAssistantRunModelReceiptScopedProjection(t *testing.T) {
	f := setupModelReceiptFixture(t)
	run, reply, prompt := f.savedReply(t, "scoped-run-receipt-source", "")
	otherTurn, otherReply, _ := f.savedReply(t, "scoped-run-receipt-next-turn", run.ConversationID)
	otherConversation, _, _ := f.savedReply(t, "scoped-run-receipt-away", "")
	path := modelReceiptPath(run.ConversationID, run.ID, reply.ID)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls, saved := f.calls.Load(), modelReceiptReadSnapshot(t, db)
	check := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("model receipt response: %d %s", w.Code, w.Body)
		}
		var out struct {
			Conversation string                 `json:"conversation_id"`
			Run          string                 `json:"run_id"`
			Message      int64                  `json:"message_id"`
			Surface      []assistantA2UIMessage `json:"surface"`
		}
		accountJSON(t, w, &out)
		if out.Conversation != run.ConversationID || out.Run != run.ID || out.Message != reply.ID || len(out.Surface) != 2 ||
			out.Surface[0].Version != "1.0" || out.Surface[0].CreateSurface == nil || out.Surface[1].UpdateComponents == nil ||
			out.Surface[0].CreateSurface.SurfaceID != out.Surface[1].UpdateComponents.SurfaceID {
			t.Fatalf("response is not a scoped A2UI 1.0 surface: %s", w.Body)
		}
		for _, item := range out.Surface[1].UpdateComponents.Components {
			if (item.Component != "Text" && item.Component != "Column") || item.Action != nil {
				t.Fatalf("model readback issues an action or binding: %+v", item)
			}
		}
		for _, required := range []string{"fixture-requested-model", "vendor/reply-actual", "Token 输入 39 / 输出 0", "提供方报告不等于独立验证"} {
			if !strings.Contains(w.Body.String(), required) {
				t.Fatalf("missing truthful projection %q: %s", required, w.Body)
			}
		}
		for _, secret := range []string{modelReceiptPrompt, modelReceiptReply, modelReceiptCredential, "CLUSBOT_TEST_RUN_RECEIPT_MODEL_KEY", "request_sha256", "last_input_tokens", "127.0.0.1"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("receipt contains raw context or secret %q", secret)
			}
		}
	}
	check(f.request("GET", path, "", nil, f.cookie))
	check(f.request("GET", path, "", nil, f.account.login("admin", "correct horse battery staple")))
	for _, wrong := range []string{
		modelReceiptPath(otherConversation.ConversationID, run.ID, reply.ID),
		modelReceiptPath(run.ConversationID, otherTurn.ID, reply.ID),
		modelReceiptPath(run.ConversationID, run.ID, otherReply.ID),
		modelReceiptPath(run.ConversationID, run.ID, prompt.ID),
	} {
		if w := f.request("GET", wrong, "", nil, f.cookie); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "vendor/reply-actual") {
			t.Fatalf("incorrect origin was readable: %d %s", w.Code, w.Body)
		}
	}
	created := f.account.request("POST", "/api/v1/admin/users", map[string]string{"username": "receipt-member", "password": "receipt fixture second password"}, f.cookie)
	var member struct{ User work.Account }
	accountJSON(t, created, &member)
	if created.Code != http.StatusCreated || member.User.PrincipalID == "" {
		t.Fatalf("member fixture missing: %d %s", created.Code, created.Body)
	}
	memberStore, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "member.db"), f.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer memberStore.Close()
	if err := memberStore.BindAccountBusinessStore(t.Context(), member.User.PrincipalID); err != nil {
		t.Fatal(err)
	}
	server := f.server
	server.PersonalAPI = func(id string) (http.Handler, error) {
		if id == member.User.PrincipalID {
			return server.PersonalHandler(memberStore), nil
		}
		return f.server.PersonalAPI(id)
	}
	f.account.handler = server.Handler(fstest.MapFS{})
	memberCookie := f.account.login("receipt-member", "receipt fixture second password")
	if w := f.request("GET", path, "", nil, memberCookie); w.Code != http.StatusNotFound {
		t.Fatalf("cross-account read: %d %s", w.Code, w.Body)
	}
	if w := f.request("DELETE", "/api/v1/session", "", nil, memberCookie); w.Code != http.StatusNoContent {
		t.Fatalf("logout failed: %d %s", w.Code, w.Body)
	}
	for _, cookie := range []*http.Cookie{nil, memberCookie} {
		if w := f.request("GET", path, "", nil, cookie); w.Code != http.StatusUnauthorized {
			t.Fatalf("inactive session could view receipt: %d %s", w.Code, w.Body)
		}
	}
	if calls != 3 || f.calls.Load() != calls || modelReceiptReadSnapshot(t, db) != saved {
		t.Fatal("read/restore ran a model, created a task, or wrote a receipt")
	}
	// Simulate an archived receipt missing one value. A recorded zero in the
	// previous responses and a missing value below must render differently.
	if _, err := db.Exec(`UPDATE runs SET output_tokens=NULL,reported_requests=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	partial := f.request("GET", path, "", nil, f.cookie)
	if partial.Code != 200 || !strings.Contains(partial.Body.String(), "Token 输入 39 / 输出 未知") ||
		!strings.Contains(partial.Body.String(), "1 次尝试未完整报告 Token") || f.calls.Load() != calls {
		t.Fatalf("partial usage was misrepresented: %d %s", partial.Code, partial.Body)
	}
	if _, err := db.Exec(`UPDATE runs SET usage_tracked=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM context_manifests WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", path, "", nil, f.cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "未记录本轮 Token 用量") ||
		!strings.Contains(w.Body.String(), "未记录本轮模型请求明细") || strings.Contains(w.Body.String(), "vendor/reply-actual") || f.calls.Load() != calls {
		t.Fatalf("missing source invented or restored an old model receipt: %d %s", w.Code, w.Body)
	}
}

func TestAssistantRunModelReceiptBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set CLUSBOT_PLAYWRIGHT_MODULE and CLUSBOT_CHROMIUM to installed browser tools")
	}
	ctx, stop := context.WithTimeout(t.Context(), 80*time.Second)
	defer stop()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(mode string, payload []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", mode)
		cmd.Dir, cmd.Stdin = web, bytes.NewReader(payload)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", mode, err, output)
		}
		return output
	}
	var files map[string][]byte
	if err := json.Unmarshal(browser("bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("could not bundle application in memory: %v", err)
	}
	assets := fstest.MapFS{}
	for name, data := range files {
		assets[name] = &fstest.MapFile{Data: data}
	}
	f := setupModelReceiptFixture(t)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	real := f.server
	real.Origin, real.Hosts = origin, []string{addr}
	server.Config.Handler = real.Handler(assets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "prompt": modelReceiptPrompt, "reply": modelReceiptReply})
	browser("browse-run-model-receipt", input)
	if f.calls.Load() != 2 {
		t.Fatalf("expected one routing and one reply request, none on receipt view/reopen: %d", f.calls.Load())
	}
}
