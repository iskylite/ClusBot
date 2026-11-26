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

const skillCardPrompt = "查看本会话可绑定的 Skill"
const skillCardCandidate = "已批准的巡检夹具"
const skillCardDraft = "仅导入未启用的夹具"
const skillCardBody = "fixture-private-embedded-text-do-not-read"
const skillCardPreview = "拟选用：" + skillCardCandidate + " · 第 1 版。仅影响以后此会话；尚未绑定。"

type assistantSkillFixture struct {
	*discoveryA2UIFixture
	current    work.SkillVersion
	draft      work.SkillVersion
	catalog    *sql.DB
	modelCalls atomic.Int32
}

func makeAssistantSkillFixture(t *testing.T, answer func(*http.Request) string) *assistantSkillFixture {
	t.Helper()
	f := &assistantSkillFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32768))
		if err != nil || r.Method != "POST" || r.URL.Path != "/responses" || json.Unmarshal(raw, &message) != nil || len(message.Input) != 1 ||
			message.Input[0].Content != `{"requested_action":"preview_conversation_skill","scope_kind":"future_conversation"}` ||
			strings.Contains(string(raw), skillCardBody) || strings.Contains(string(raw), skillCardCandidate) || strings.Contains(string(raw), skillCardDraft) ||
			strings.Contains(string(raw), "untrusted-browser-message") || strings.Contains(string(raw), skillCardPrompt) {
			t.Error("unexpected settings data, document or object reached the preview model")
			http.Error(w, "unexpected model input", 400)
			return
		}
		f.modelCalls.Add(1)
		out := `{"next":"show_binding_preview"}`
		if answer != nil {
			out = answer(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", out)
	}))
	t.Cleanup(provider.Close)
	f.discoveryA2UIFixture = newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "skill-card-fixture", Protocol: model.ProtocolResponses, BaseURL: provider.URL,
			KeyRef: "env:CLUSBOT_SKILL_CARD_TEST_MODEL_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	importDoc := func(scope string) work.SkillVersion {
		input := work.SkillDocumentInput{Filename: "SKILL.md", Scope: scope,
			Document: "---\nname: local-skill-fixture\ndescription: Synthetic local Skill validation\n---\n" + skillCardBody}
		preview, err := f.instance.PreviewSkillDocument(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := f.instance.ImportSkillDocument(t.Context(), input, preview.Source.SourceSHA256, preview.PreviewBinding)
		if err != nil || draft.Active || draft.ApprovedAt != nil {
			t.Fatalf("draft import changed authorization: %+v %v", draft, err)
		}
		return draft
	}
	f.current, f.draft = importDoc(skillCardCandidate), importDoc(skillCardDraft)
	var err error
	f.catalog, err = sql.Open("sqlite", filepath.Join(filepath.Dir(f.path), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.catalog.Close() })
	// Test-only initial state: the live attached catalog already contains an
	// approved Skill. The actual conversational path never calls activation.
	tx, err := f.catalog.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE personal_skills SET active_version=?,revision=revision+1 WHERE id=?", f.current.Version, f.current.SkillID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE personal_skill_versions SET approved_at=? WHERE skill_id=? AND version=?", time.Now().UnixMilli(), f.current.SkillID, f.current.Version); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *assistantSkillFixture) open(t *testing.T) (path, conversation, run string, event map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"text": skillCardPrompt, "client_message_id": "fixture-skill-card-open"})
	r := httptest.NewRequest("POST", "https://ops.example/api/v1/conversations", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://ops.example")
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
	if w.Code != 202 || ids.Run == "" || ids.Conversation == "" || ids.Task != "" {
		t.Fatalf("composer did not keep a safe original Run: %d %s", w.Code, w.Body)
	}
	path = "/api/v1/conversations/" + ids.Conversation + "/skill-card/" + ids.Run
	response := f.request("GET", path, "", nil, f.cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), skillCardCandidate) || strings.Contains(response.Body.String(), skillCardDraft) ||
		strings.Contains(response.Body.String(), skillCardBody) || strings.Contains(response.Body.String(), f.current.SkillID) || f.modelCalls.Load() != 0 {
		t.Fatalf("catalog revealed drafts, instructions or identity: %d %s", response.Code, response.Body)
	}
	var out a2uiDiscoveryReply
	accountJSON(t, response, &out)
	return path, ids.Conversation, ids.Run, renderedA2UIEvent(t, out, assistantSkillPreviewAction)
}

func (f *assistantSkillFixture) unchanged(t *testing.T, conversation, run string) {
	t.Helper()
	current, err := f.store.ReadConversationSkillSelection(t.Context(), f.actor, conversation)
	binding, bindingErr := f.store.ReadRunSkillBinding(t.Context(), f.actor, run)
	if err != nil || bindingErr != nil || current.Binding != nil || current.BindingRevision != 0 || binding.Binding != nil || binding.TaskID != "" || binding.RunStatus != "completed" {
		t.Fatalf("preview changed configuration or selected executable content: %+v %+v %v %v", current, binding, err, bindingErr)
	}
	var active sql.NullInt64
	if err := f.catalog.QueryRow("SELECT active_version FROM personal_skills WHERE id=?", f.draft.SkillID).Scan(&active); err != nil || active.Valid {
		t.Fatalf("unapproved draft was enabled: %v %v", active, err)
	}
}

func TestAssistantSkillOptionsScopeAndReceipt(t *testing.T) {
	f := makeAssistantSkillFixture(t, nil)
	path, conversation, run, event := f.open(t)
	fresh := f.account.login("admin", "correct horse battery staple")
	if r := f.request("POST", path+"/actions", "skill-cross-login", event, fresh); r.Code != 409 {
		t.Fatalf("new login reused old action: %d %s", r.Code, r.Body)
	}
	cross := strings.Replace(path, conversation, strings.Repeat("e", 32), 1)
	if r := f.request("POST", cross+"/actions", "skill-cross-conv", event, f.cookie); r.Code != 404 {
		t.Fatalf("cross-conversation accepted: %d %s", r.Code, r.Body)
	}
	action := event["action"].(map[string]any)
	request := action["context"].(map[string]any)
	action["userMessage"] = "untrusted-browser-message"
	request["skill_id"] = f.draft.SkillID
	if r := f.request("POST", path+"/actions", "skill-raw-injection", event, f.cookie); r.Code != 400 {
		t.Fatalf("draft identity accepted from client: %d %s", r.Code, r.Body)
	}
	delete(request, "skill_id")
	if f.modelCalls.Load() != 0 {
		t.Fatal("forged requests invoked model")
	}
	for range 2 {
		r := f.request("POST", path+"/actions", "skill-same-request", event, f.cookie)
		if r.Code != 200 || !strings.Contains(r.Body.String(), skillCardPreview) || f.modelCalls.Load() != 1 {
			t.Fatalf("wrong or repeated preview: %d %s model=%d", r.Code, r.Body, f.modelCalls.Load())
		}
	}
	f.unchanged(t, conversation, run)
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range events {
		if strings.HasPrefix(v.Kind, "assistant.skill_binding_") && v.Data != "{}" {
			t.Fatal("private Skill reference leaked in event replay")
		}
	}
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
	f.store = reopened
	server := f.server
	server.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor != f.actor {
			return nil, work.ErrAccountDenied
		}
		return server.PersonalHandler(reopened), nil
	}
	f.account.handler = server.Handler(nil)
	r := f.request("GET", path, "", nil, fresh)
	if r.Code != 200 || !strings.Contains(r.Body.String(), skillCardPreview) || f.modelCalls.Load() != 1 || !strings.Contains(r.Body.String(), `"binding_confirmations":null`) {
		t.Fatalf("reopened SQLite preview missing, new login gained a confirmation, or model replayed: %d %s", r.Code, r.Body)
	}
	if _, err := f.catalog.Exec("UPDATE personal_skills SET active_version=NULL,revision=revision+1 WHERE id=?", f.current.SkillID); err != nil {
		t.Fatal(err)
	}
	r = f.request("GET", path, "", nil, fresh)
	if r.Code != 200 || strings.Contains(r.Body.String(), skillCardCandidate) || !strings.Contains(r.Body.String(), "有条目已停用") {
		t.Fatalf("disabled Skill remained visible: %d %s", r.Code, r.Body)
	}
	f.unchanged(t, conversation, run)
}

func TestAssistantSkillOptionsRechecksDuringModel(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}()
	f := makeAssistantSkillFixture(t, func(r *http.Request) string {
		close(started)
		select {
		case <-done:
		case <-r.Context().Done():
		}
		return `{"next":"show_binding_preview"}`
	})
	path, conversation, run, event := f.open(t)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.request("POST", path+"/actions", "skill-change-during-model", event, f.cookie) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("missing model action")
	}
	if _, err := f.catalog.Exec("UPDATE personal_skills SET active_version=NULL,revision=revision+1 WHERE id=?", f.current.SkillID); err != nil {
		t.Fatal(err)
	}
	close(done)
	select {
	case r := <-result:
		if r.Code != 409 {
			t.Fatalf("old result became valid after revoke: %d %s", r.Code, r.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model action did not settle")
	}
	fresh := f.request("GET", path, "", nil, f.cookie)
	if fresh.Code != 200 || strings.Contains(fresh.Body.String(), skillCardPreview) || strings.Contains(fresh.Body.String(), skillCardCandidate) {
		t.Fatalf("stale Skill visible: %d %s", fresh.Code, fresh.Body)
	}
	f.unchanged(t, conversation, run)
}

func TestAssistantSkillOptionsDoesNotReuseAChangedBinding(t *testing.T) {
	f := makeAssistantSkillFixture(t, nil)
	path, conversation, run, event := f.open(t)
	if _, err := f.store.AssistantSkillOptions(t.Context(), "user:another-owner", conversation, run); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign account could access the selection: %v", err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The user-visible state may still be "default" after bind/clear in
	// another window, but a new binding generation must invalidate old input.
	if _, err := db.Exec("INSERT INTO conversation_binding_revisions(conversation_id,kind,revision) VALUES(?,'skill',2)", conversation); err != nil {
		t.Fatal(err)
	}
	if r := f.request("POST", path+"/actions", "skill-changed-binding", event, f.cookie); r.Code != 409 || f.modelCalls.Load() != 0 {
		t.Fatalf("an old card became valid again: %d %s", r.Code, r.Body)
	}
	if r := f.request("GET", path, "", nil, f.cookie); r.Code != 200 || !strings.Contains(r.Body.String(), "当前会话绑定已变化") || strings.Contains(r.Body.String(), "skillpreview_") {
		t.Fatalf("changed selection still rendered an old action: %d %s", r.Code, r.Body)
	}
	got, err := f.store.ReadConversationSkillSelection(t.Context(), f.actor, conversation)
	if err != nil || got.Binding != nil || got.BindingRevision != 2 {
		t.Fatalf("preview altered the changed settings: %+v %v", got, err)
	}
}

func TestAssistantSkillOptionsRejectsModelBindingAction(t *testing.T) {
	f := makeAssistantSkillFixture(t, func(*http.Request) string { return `{"next":"bind_skill"}` })
	path, conversation, run, event := f.open(t)
	r := f.request("POST", path+"/actions", "skill-model-cannot-bind", event, f.cookie)
	if r.Code != 502 {
		t.Fatalf("model proposed setting mutation: %d %s", r.Code, r.Body)
	}
	f.unchanged(t, conversation, run)
}

func (f *assistantSkillFixture) previewConfirmation(t *testing.T, path string, event map[string]any) assistantSkillConfirmation {
	t.Helper()
	response := f.request("POST", path+"/actions", "skill-prepare-confirmation", event, f.cookie)
	var out struct {
		Forms []assistantSkillConfirmationForm `json:"binding_confirmations"`
	}
	accountJSON(t, response, &out)
	if response.Code != 200 || len(out.Forms) != 1 || len(out.Forms[0].Proof) != 32 || out.Forms[0].Label != skillCardCandidate+" · 第 1 版" {
		t.Fatalf("no fixed scoped confirm form: %d %s", response.Code, response.Body)
	}
	initial := out.Forms[0].assistantSkillConfirmation
	restored := f.request("GET", path, "", nil, f.cookie)
	out.Forms = nil
	accountJSON(t, restored, &out)
	if restored.Code != 200 || len(out.Forms) != 1 || out.Forms[0].assistantSkillConfirmation != initial || f.modelCalls.Load() != 1 {
		t.Fatalf("reading the successful preview renewed the proof or retried model: %d %s", restored.Code, restored.Body)
	}
	return initial
}

func skillConfirmBody(in assistantSkillConfirmation) map[string]any {
	return map[string]any{"action": assistantSkillConfirmAction, "preview_confirmation": in}
}

func (f *assistantSkillFixture) checkBound(t *testing.T, conversation, run string) {
	t.Helper()
	current, err := f.store.ReadConversationSkillSelection(t.Context(), f.actor, conversation)
	binding, bindingErr := f.store.ReadRunSkillBinding(t.Context(), f.actor, run)
	if err != nil || bindingErr != nil || current.Binding == nil || current.BindingRevision != 1 ||
		current.Binding.SkillID != f.current.SkillID || current.Binding.Version != f.current.Version || current.Binding.Validity != "valid" ||
		binding.Binding != nil || binding.TaskID != "" || binding.RunStatus != "completed" || f.modelCalls.Load() != 1 {
		t.Fatalf("did not bind exactly once for future runs, or executed Skill/model: %+v %+v %v %v", current, binding, err, bindingErr)
	}
	var active, draft sql.NullInt64
	var instructions string
	if err := f.catalog.QueryRow("SELECT active_version FROM personal_skills WHERE id=?", f.draft.SkillID).Scan(&draft); err != nil || draft.Valid {
		t.Fatalf("draft changed during binding: %v %v", draft, err)
	}
	if err := f.catalog.QueryRow(`SELECT s.active_version,v.instructions FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id AND v.version=s.active_version WHERE s.id=?`, f.current.SkillID).Scan(&active, &instructions); err != nil || !active.Valid || active.Int64 != int64(f.current.Version) || instructions != skillCardBody {
		t.Fatal("binding changed the approved Skill or instructions")
	}
}

func TestAssistantSkillBindingExplicitConfirmation(t *testing.T) {
	f := makeAssistantSkillFixture(t, nil)
	path, conversation, run, event := f.open(t)
	savePath := "/api/v1/conversations/" + conversation + "/skill-binding"
	if r := f.request("PUT", savePath, "", skillConfirmBody(assistantSkillConfirmation{}), f.cookie); r.Code != 409 {
		t.Fatalf("no preview proof accepted: %d %s", r.Code, r.Body)
	}
	in := f.previewConfirmation(t, path, event)
	f.unchanged(t, conversation, run)
	fresh := f.account.login("admin", "correct horse battery staple")
	cross := strings.Replace(savePath, conversation, strings.Repeat("e", 32), 1)
	for label, r := range map[string]*httptest.ResponseRecorder{
		"another login":        f.request("PUT", savePath, "", skillConfirmBody(in), fresh),
		"another conversation": f.request("PUT", cross, "", skillConfirmBody(in), f.cookie),
	} {
		if r.Code != 409 {
			t.Fatalf("%s accepted prior login or conversation: %d %s", label, r.Code, r.Body)
		}
	}
	if r := f.request("PUT", savePath, "", skillConfirmBody(in), nil); r.Code != 401 {
		t.Fatalf("no logged-in account used confirmation: %d %s", r.Code, r.Body)
	}
	for _, vary := range []func(*assistantSkillConfirmation){
		func(c *assistantSkillConfirmation) { c.Run = strings.Repeat("e", 32) },
		func(c *assistantSkillConfirmation) { c.Slot = "2" },
		func(c *assistantSkillConfirmation) { c.Revision++ },
		func(c *assistantSkillConfirmation) { c.Expiry++ },
	} {
		stale := in
		vary(&stale)
		if r := f.request("PUT", savePath, "", skillConfirmBody(stale), f.cookie); r.Code != 409 {
			t.Fatalf("altered scope accepted: %d %s", r.Code, r.Body)
		}
	}
	mixed := skillConfirmBody(in)
	mixed["skill_id"] = f.draft.SkillID
	mixed["version"] = f.draft.Version
	if r := f.request("PUT", savePath, "", mixed, f.cookie); r.Code != 400 {
		t.Fatalf("explicit preview supplied a different Skill: %d %s", r.Code, r.Body)
	}
	f.unchanged(t, conversation, run)
	replies := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { replies <- f.request("PUT", savePath, "", skillConfirmBody(in), f.cookie) }()
	}
	var first work.AssistantSkillBindingReceipt
	for i := range 2 {
		response := <-replies
		var receipt work.AssistantSkillBindingReceipt
		accountJSON(t, response, &receipt)
		if response.Code != 200 || receipt.Slot != in.Slot || receipt.Conversation != conversation ||
			receipt.Run != run || receipt.BindingRevision != 1 || receipt.ConfirmedAt <= 0 || (i > 0 && receipt != first) {
			t.Fatalf("duplicate or invalid confirm receipt: %d %s", response.Code, response.Body)
		}
		first = receipt
	}
	f.checkBound(t, conversation, run)
	read := f.request("GET", path, "", nil, fresh)
	if read.Code != 200 || !strings.Contains(read.Body.String(), "已保存本次 Skill 绑定。仅影响以后此会话。") ||
		!strings.Contains(read.Body.String(), `"binding_confirmations":null`) {
		t.Fatalf("different login did not read prior result safely: %d %s", read.Code, read.Body)
	}
	if r := f.request("PUT", savePath, "", map[string]any{"action": "clear_skill", "expected_binding_revision": 1}, f.cookie); r.Code != 200 {
		t.Fatalf("existing fixed settings could not clear the same binding: %d %s", r.Code, r.Body)
	}
	if r := f.request("PUT", savePath, "", skillConfirmBody(in), f.cookie); r.Code != 200 {
		t.Fatalf("same confirmation did not return its original receipt: %d %s", r.Code, r.Body)
	}
	current, err := f.store.ReadConversationSkillSelection(t.Context(), f.actor, conversation)
	if err != nil || current.Binding != nil || current.BindingRevision != 2 {
		t.Fatalf("replayed old confirmation wrote again: %+v %v", current, err)
	}
	read = f.request("GET", path, "", nil, f.cookie)
	if read.Code != 200 || !strings.Contains(read.Body.String(), "本次 Skill 绑定曾保存；当前选用已变化或失效。") {
		t.Fatalf("did not separate historical receipt from current selection: %d %s", read.Code, read.Body)
	}
	events, _, err := f.store.ReplayRunEvents(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == "assistant.skill_binding_applied" {
			count++
			if e.Data != "{}" {
				t.Fatal("private apply receipt details leaked into run event replay")
			}
		}
	}
	if count != 1 {
		t.Fatalf("same preview produced %d stored confirm events", count)
	}
}

func TestAssistantSkillBindingRejectsStaleOrBusy(t *testing.T) {
	for _, test := range []string{"disabled", "working", "revised", "expired"} {
		t.Run(test, func(t *testing.T) {
			f := makeAssistantSkillFixture(t, nil)
			path, conversation, run, e := f.open(t)
			in := f.previewConfirmation(t, path, e)
			savePath := "/api/v1/conversations/" + conversation + "/skill-binding"
			switch test {
			case "disabled":
				if _, err := f.catalog.Exec("UPDATE personal_skills SET active_version=NULL,revision=revision+1 WHERE id=?", f.current.SkillID); err != nil {
					t.Fatal(err)
				}
			case "working":
				if _, err := f.store.Submit(t.Context(), "skill-fixture-active", "fixture queued message", "", conversation, true); err != nil {
					t.Fatal(err)
				}
			case "revised":
				db, err := sql.Open("sqlite", f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec("UPDATE conversations SET revision=revision+1 WHERE id=?", conversation); err != nil {
					t.Fatal(err)
				}
			case "expired":
				in.Expiry = time.Now().Add(-time.Minute).UnixMilli()
			}
			r := f.request("PUT", savePath, "", skillConfirmBody(in), f.cookie)
			if r.Code != 409 || !strings.Contains(r.Body.String(), "skill_binding_preview_stale") || f.modelCalls.Load() != 1 {
				t.Fatalf("%s confirmation was not rejected: %d %s", test, r.Code, r.Body)
			}
			f.unchanged(t, conversation, run)
		})
	}
}

func TestAssistantSkillOptionsBrowserRoundTrip(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set local browser tooling")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(stage string, stdin []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", stage)
		cmd.Dir = web
		cmd.Stdin = bytes.NewReader(stdin)
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
		t.Fatalf("in-memory app unavailable: %v", err)
	}
	assets := fstest.MapFS{}
	for path, data := range files {
		assets[path] = &fstest.MapFile{Data: data}
	}
	f := makeAssistantSkillFixture(t, nil)
	server := httptest.NewUnstartedServer(nil)
	addr := server.Listener.Addr().String()
	origin := "http://" + addr
	actual := f.server
	actual.Origin, actual.Hosts = origin, []string{addr}
	server.Config.Handler = actual.Handler(assets)
	server.Start()
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"origin": origin, "title": skillCardCandidate, "prompt": skillCardPrompt})
	var scope struct {
		Conversation string `json:"conversation"`
		Run          string `json:"run"`
	}
	if err := json.Unmarshal(browser("browse-skill-binding-preview", input), &scope); err != nil || scope.Conversation == "" || scope.Run == "" {
		t.Fatalf("browser did not return original scope: %+v %v", scope, err)
	}
	if f.modelCalls.Load() != 1 {
		t.Fatalf("preview or confirmed recover replayed model: %d", f.modelCalls.Load())
	}
	f.checkBound(t, scope.Conversation, scope.Run)
}
