package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
	"clusbot/internal/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTaskWorkerRolePinsBothReadBranchesAndFailsBeforeFallback(t *testing.T) {
	const ref = "env:CLUSBOT_INTENT_TEST_KEY"
	t.Setenv("CLUSBOT_INTENT_TEST_KEY", "local-worker-fixture")
	modelNames := make(chan string, 8)
	var modelCalls, readCalls atomic.Int32
	mcpEntered := make(chan struct{}, 1)
	releaseMCP := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseMCP) }) }

	localModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		modelCalls.Add(1)
		modelNames <- req.Model
		w.Header().Set("Content-Type", "text/event-stream")
		wire, _ := json.Marshal(req.Input)
		if strings.Contains(string(wire), "function_call_output") {
			if !strings.Contains(string(wire), "fixture-authorized-read-result") {
				t.Error("final worker model call did not carry the authorized tool result")
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"汇总完成\"}\n\n")
			if req.Model == "worker-v1" {
				// Lose the terminal message after the committed tool result.
				return
			}
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")
			return
		}
		for _, tool := range req.Tools {
			args := "{}"
			if strings.HasPrefix(tool.Name, "mcp_") {
				args = `{"mode":"normal"}`
			} else if tool.Name != taskReadFunction {
				continue
			}
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":%q,\"call_id\":\"read-once\",\"status\":\"completed\",\"arguments\":%q}],\"usage\":{\"input_tokens\":1,\"output_tokens\":5}}}\n\n", tool.Name, args)
			return
		}
		t.Error("expected the existing selected/automatic read tool")
	}))
	defer localModel.Close()
	store, s := classificationSettings(t, localModel.URL)
	ctx := t.Context()
	legacy, legacyBinding, err := s.TextRunProvider(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := s.ResolveTaskRunProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defaultText, defaultBinding, err := pinned.TextRunProvider(ctx, true)
	if err != nil || defaultBinding != legacyBinding || defaultText.(*Provider).model != legacy.(*Provider).model {
		t.Fatalf("unbound task checkpoint changed: %q -> %q (%v)", legacyBinding, defaultBinding, err)
	}
	defaultRead, err := pinned.TaskReadModel(ctx)
	if err != nil || defaultRead != defaultText.(*Provider) {
		t.Fatalf("unbound read did not use the one resolved provider: %v", err)
	}
	defaultMulti, err := pinned.MultiTaskReadModel(ctx)
	if err != nil || defaultMulti != defaultText.(*Provider) {
		t.Fatalf("unbound MultiTaskReadModel did not use the one resolved provider: %v", err)
	}

	connector := mcp.NewServer(&mcp.Implementation{Name: "local-worker-evidence", Version: "1"}, nil)
	type readArgs struct {
		Mode string `json:"mode"`
	}
	mcp.AddTool(connector, &mcp.Tool{Name: "read_evidence", Description: "Read a fixed local fixture", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(callCtx context.Context, _ *mcp.CallToolRequest, a readArgs) (*mcp.CallToolResult, any, error) {
			if a.Mode != "normal" {
				t.Error("source call changed verified arguments")
			}
			if readCalls.Add(1) == 1 {
				mcpEntered <- struct{}{}
				select {
				case <-releaseMCP:
				case <-callCtx.Done():
					return nil, nil, callCtx.Err()
				}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fixture-authorized-read-result"}}}, nil, nil
		})
	localMCP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return connector }, nil))
	defer localMCP.Close()
	defer release()
	conn, err := store.SaveConnection(ctx, "", work.Connection{Name: "Local fixture", Enabled: true, Config: mcpclient.Config{Transport: "http", Endpoint: localMCP.URL + "/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = store.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil || len(conn.Tools) != 1 {
		t.Fatalf("discover declared fixture: %+v %v", conn, err)
	}
	tool := conn.Tools[0]
	if err := store.SetToolPermission(ctx, conn.ID, tool.Name, tool.Version, conn.Revision, true); err != nil {
		t.Fatal(err)
	}
	read := work.TaskReadRequest{ConnectionID: conn.ID, Revision: conn.Revision + 1, ToolName: tool.Name, ToolVersion: tool.Version, Arguments: map[string]any{"mode": "normal"}}

	profile := func(name string) ProfileView {
		t.Helper()
		p, err := s.SaveProfile(ctx, "", ProfileInput{Name: name, ModelID: name, Protocol: ProtocolResponses, BaseURL: localModel.URL, KeyRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first, second := profile("worker-v1"), profile("worker-v2")
	bind := func(profile ProfileView, revision int) {
		t.Helper()
		v, err := s.SetTaskWorkerRole(ctx, TaskWorkerRoleInput{Action: "bind_profile", Revision: revision, ProfileID: profile.ID,
			ProfileRevision: profile.Revision, ConfirmTaskContextEgress: true})
		if err != nil || v.Status != "locally_configured" {
			t.Fatalf("bind worker role: %+v %v", v, err)
		}
	}
	bind(first, 0)
	explicit, err := store.SubmitWithDelivery(ctx, "explicit-worker-read", "汇总固定读源", "", "", true, work.Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	workers := store.Start(runCtx, s, 1, work.RunLimits{ActiveTime: 15 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 768})
	defer func() { release(); cancel(); workers.Wait() }()
	select {
	case <-mcpEntered:
	case <-time.After(4 * time.Second):
		t.Fatal("selected read did not dispatch")
	}
	bind(second, 1)
	release()
	done := waitConversationReplyRun(t, store, explicit.ID)
	if done.Status != "interrupted" || done.ErrorCode != "model_error" || modelCalls.Load() != 2 || readCalls.Load() != 1 {
		t.Fatalf("selected read run failed, or changed dispatch count: %+v model=%d read=%d", done, modelCalls.Load(), readCalls.Load())
	}
	if got := []string{<-modelNames, <-modelNames}; !reflect.DeepEqual(got, []string{"worker-v1", "worker-v1"}) {
		t.Fatalf("rebind changed the in-flight selected read completion: %v", got)
	}

	interrupted, err := store.Detail(explicit.TaskID, true)
	if err != nil || interrupted.ReadCheckpoint != "ready" {
		t.Fatalf("typed read original checkpoint not available: %+v %v", interrupted, err)
	}
	resumed, err := store.Resume(ctx, explicit.TaskID, "resume-typed-worker", interrupted.Task.Revision)
	if err != nil || resumed.ID != explicit.ID || resumed.Generation != 1 {
		t.Fatalf("resume altered original run: %+v %v", resumed, err)
	}
	mismatch := waitConversationReplyRun(t, store, explicit.ID)
	if mismatch.ErrorCode != "read_checkpoint_unavailable" || mismatch.Generation != 1 || modelCalls.Load() != 2 || readCalls.Load() != 1 {
		t.Fatalf("resume used changed Worker or re-dispatched the old read: %+v model=%d read=%d", mismatch, modelCalls.Load(), readCalls.Load())
	}

	automatic, err := store.Submit(ctx, "automatic-worker-read", "用已授权只读来源做汇总", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	autoDone := waitConversationReplyRun(t, store, automatic.ID)
	if autoDone.Status != "completed" || modelCalls.Load() != 4 || readCalls.Load() != 2 {
		t.Fatalf("automatic branch did not use exactly one authorized source: %+v model=%d read=%d", autoDone, modelCalls.Load(), readCalls.Load())
	}
	if got := []string{<-modelNames, <-modelNames}; !reflect.DeepEqual(got, []string{"worker-v2", "worker-v2"}) {
		t.Fatalf("automatic branch switched or fell back: %v", got)
	}

	if err := s.DeleteProfile(ctx, second.ID, second.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveTaskRunProvider(ctx); !errors.Is(err, work.ErrTaskWorkerModelRoleUnavailable) {
		t.Fatalf("explicit deletion silently fell back: %v", err)
	}
	stale, err := store.SubmitWithDelivery(ctx, "stale-worker-read", "档案失效不得读源", "", "", true, work.Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitConversationReplyRun(t, store, stale.ID)
	if failed.ErrorCode != "task_worker_role_unavailable" || modelCalls.Load() != 4 || readCalls.Load() != 2 || strings.Contains(failed.Error, localModel.URL) {
		t.Fatalf("stale Worker sent a model or tool call, or exposed config: %+v model=%d read=%d", failed, modelCalls.Load(), readCalls.Load())
	}
}
