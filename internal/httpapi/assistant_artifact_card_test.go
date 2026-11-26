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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

type assistantArtifactFixtureWorker string

func (p assistantArtifactFixtureWorker) Stream(_ context.Context, _ []work.Message, _ bool, _ int, emit func(string) error) (*work.TokenUsage, error) {
	return nil, emit(string(p))
}

const artifactFixtureTitle = "只有个人账本可见的成果任务"
const artifactFixtureText = "纯文本测试内容 <strong>不要解析</strong>\n行二：<https://example.invalid/不应访问>"

func produceAssistantArtifact(t *testing.T, store *work.Store, title, text, key string) (work.Run, work.Artifact) {
	t.Helper()
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	run, err := store.Submit(ctx, key, title, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	workers := store.Start(ctx, assistantArtifactFixtureWorker(text), 1, work.RunLimits{
		ActiveTime: 5 * time.Second, ModelTimeout: 4 * time.Second, OutputTokens: 4096,
	})
	defer func() { stop(); workers.Wait() }()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("artifact worker did not finish: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
			d, err := store.Detail(run.TaskID, true)
			if err != nil {
				t.Fatal(err)
			}
			if d.Artifact != nil {
				if d.Task.Status != "waiting_user" || d.Artifact.Content != text {
					t.Fatalf("invalid real worker result: task=%s artifact=%+v", d.Task.Status, d.Artifact)
				}
				return run, *d.Artifact
			}
			if d.Run.Status == "interrupted" {
				t.Fatalf("artifact worker was interrupted: %+v", d.Run)
			}
		}
	}
}

func TestAssistantArtifactCardScopeAndPreview(t *testing.T) {
	var calls atomic.Int32
	t.Setenv("CLUSBOT_ARTIFACT_FIXTURE_KEY", "local-asset-fixture-key")
	modelProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Input) != 1 || req.Input[0].Content != `{"requested_action":"preview_artifact_text"}` {
			t.Error("model input not limited to the preview action")
		}
		for _, secret := range []string{"不要解析", "example.invalid", "助手成果.md", artifactFixtureTitle, "fixture-private-note", "artifact_slot"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("model received private field %q", secret)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", `{"next":"show_preview"}`)
	}))
	defer modelProvider.Close()
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		s := &model.Settings{Store: store}
		if _, err := s.Initialize(t.Context(), work.ModelSettings{BaseURL: modelProvider.URL, ModelID: "artifact-card-fixture", Protocol: model.ProtocolResponses, KeyRef: "env:CLUSBOT_ARTIFACT_FIXTURE_KEY"}); err != nil {
			t.Fatal(err)
		}
		return s
	})
	content := artifactFixtureText + strings.Repeat("甲", 900) + "END_NOT_IN_PREVIEW"
	run, artifact := produceAssistantArtifact(t, f.store, artifactFixtureTitle, content, "artifact-result-task")
	_, binary := produceAssistantArtifact(t, f.store, "不支持预览的成果", "只用于构造类型校验", "artifact-binary-task")
	private, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if _, err := private.Exec("UPDATE artifacts SET content=? WHERE id=?", []byte{0, 1, 2}, binary.ID); err != nil {
		t.Fatal(err)
	}
	// Synthetic source metadata, no MCP process or request. The normal
	// exactMCPAuthority predicate reads the attached instance catalog.
	instance, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.path), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if _, err = instance.Exec(`INSERT INTO connections(id,name,config,enabled,revision,status) VALUES('fixture-source','fixture-source','{}',1,3,'ready');
INSERT INTO connection_tools(connection_id,name,definition,allowed,read_only)
VALUES('fixture-source','fixture_tool','{"version":"1"}',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = private.Exec(`INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,result)
VALUES(?,1,'fixture-source',3,'local-only fixture','fixture_tool','fixture','1','{}','completed','{"fixture":true}')`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Artifact(artifact.ID); err != nil {
		t.Fatalf("fixture evidence is not readable: %v", err)
	}
	request := func(method, path, key string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(raw))
		if method != "GET" {
			req.Header.Set("Origin", "https://ops.example")
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-ClusBot-Assistant-Cards", "1")
		if key != "" {
			req.Header.Set("X-ClusBot-Request-Key", key)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.account.handler.ServeHTTP(w, req)
		return w
	}
	opened := request("POST", "/api/v1/conversations", "", map[string]string{"text": "查看我的成果", "client_message_id": "artifact-open-fixture"}, f.cookie)
	if opened.Code != 202 {
		t.Fatalf("artifact composer: %d %s", opened.Code, opened.Body)
	}
	var origin struct {
		Conversation string `json:"conversation_id"`
		Run          string `json:"run_id"`
	}
	accountJSON(t, opened, &origin)
	base := "/api/v1/conversations/" + origin.Conversation + "/artifact-card/" + origin.Run
	detail, err := f.store.Detail(origin.Conversation, false)
	if err != nil || len(detail.Messages) < 2 || !detail.Messages[len(detail.Messages)-1].AssistantArtifactList {
		t.Fatalf("artifact card not attached to history: %v", err)
	}
	view := request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || view.Header().Get("Cache-Control") != "no-store" || !strings.Contains(view.Body.String(), artifactFixtureTitle) ||
		strings.Contains(view.Body.String(), "不要解析") || strings.Contains(view.Body.String(), artifact.ID) || calls.Load() != 0 {
		t.Fatalf("initial card includes body, target ID, or unexpected egress: %d %s", view.Code, view.Body)
	}
	var card a2uiDiscoveryReply
	accountJSON(t, view, &card)
	binaryAction := renderedA2UIEvent(t, card, assistantArtifactPreviewAction)
	eventBody := binaryAction["action"].(map[string]any)
	eventBody["userMessage"] = "fixture-private-note"
	if w := request("POST", base+"/actions", "artifact-binary-fixture", binaryAction, f.cookie); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "没有可安全显示的纯文本预览") || calls.Load() != 1 {
		t.Fatalf("binary ignored text-only requirement: %d %s", w.Code, w.Body)
	}
	view = request("GET", base, "", nil, f.cookie)
	accountJSON(t, view, &card)
	action := renderedA2UIEvent(t, card, assistantArtifactPreviewAction)
	event := action["action"].(map[string]any)
	event["name"] = "accept_artifact"
	if w := request("POST", base+"/actions", "artifact-fake-accept", action, f.cookie); w.Code != 400 || calls.Load() != 1 {
		t.Fatalf("read card enables acceptance: %d %s", w.Code, w.Body)
	}
	event["name"] = assistantArtifactPreviewAction
	newSession := f.account.login("admin", "correct horse battery staple")
	if w := request("POST", base+"/actions", "artifact-foreign-session", action, newSession); w.Code != 409 || calls.Load() != 1 {
		t.Fatalf("old session signs new card: %d %s", w.Code, w.Body)
	}
	for range 2 {
		w := request("POST", base+"/actions", "artifact-preview-once", action, f.cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "不要解析") || !strings.Contains(w.Body.String(), "仅展示前 600") ||
			strings.Contains(w.Body.String(), "END_NOT_IN_PREVIEW") || calls.Load() != 2 {
			t.Fatalf("unexpected preview or replay: %d calls=%d %s", w.Code, calls.Load(), w.Body)
		}
	}
	d, err := f.store.Detail(run.TaskID, true)
	if err != nil || d.Task.Status != "waiting_user" || d.Acceptance != nil || d.Publication != nil {
		t.Fatalf("preview changed workflow: %v", err)
	}
	f.account.handler = f.server.Handler(nil)
	view = request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || !strings.Contains(view.Body.String(), "不要解析") || calls.Load() != 2 {
		t.Fatalf("preview not restored without another model call: %d %s", view.Code, view.Body)
	}
	// Binding a receipt to an artifact ID alone could expose a newer body.
	if _, err := private.Exec("UPDATE artifacts SET sha256=? WHERE id=?", strings.Repeat("1", 64), artifact.ID); err != nil {
		t.Fatal(err)
	}
	view = request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || strings.Contains(view.Body.String(), artifactFixtureTitle) ||
		!strings.Contains(view.Body.String(), "原预览不再显示") || calls.Load() != 2 {
		t.Fatalf("changed artifact digest reused an old preview: %d %s", view.Code, view.Body)
	}
	if _, err := private.Exec("UPDATE artifacts SET sha256=? WHERE id=?", artifact.Digest, artifact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Exec("UPDATE connection_tools SET allowed=0 WHERE connection_id='fixture-source'"); err != nil {
		t.Fatal(err)
	}
	view = request("GET", base, "", nil, f.cookie)
	if view.Code != 200 || strings.Contains(view.Body.String(), artifactFixtureTitle) || strings.Contains(view.Body.String(), "不要解析") ||
		!strings.Contains(view.Body.String(), "原预览不再显示") || calls.Load() != 2 {
		t.Fatalf("revoked source still visible in restored preview: %d %s", view.Code, view.Body)
	}
	events, _, err := f.store.ReplayRunEvents(origin.Run, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.HasPrefix(e.Kind, "assistant.artifact_") && e.Data != "{}" {
			t.Fatalf("artifact data visible in unrelated run replay: %s", e.Kind)
		}
	}
}
