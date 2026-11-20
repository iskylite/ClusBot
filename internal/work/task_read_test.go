package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func taskReadFixture(t *testing.T) (*Store, TaskReadRequest) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "task-read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := lookupServer()
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(endpoint.Close)
	connection, err := store.SaveConnection(ctx, "", Connection{Name: "样例资料", Enabled: true,
		Config: mcpclient.Config{Transport: "http", Endpoint: endpoint.URL + "/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	connection, err = store.TestConnection(ctx, connection.ID, connection.Revision)
	if err != nil || len(connection.Tools) != 1 {
		t.Fatalf("discover: %+v, %v", connection, err)
	}
	tool := connection.Tools[0]
	if err = store.SetToolPermission(ctx, connection.ID, tool.Name, tool.Version, connection.Revision, true); err != nil {
		t.Fatal(err)
	}
	return store, TaskReadRequest{ConnectionID: connection.ID, Revision: connection.Revision + 1, ToolName: tool.Name,
		ToolVersion: tool.Version, Arguments: map[string]any{"mode": "normal"}}
}

func TestTaskReadBindsArgumentsAndReusesSavedResult(t *testing.T) {
	ctx := context.Background()
	store, read := taskReadFixture(t)
	submit := func() (Run, error) {
		return store.SubmitWithDelivery(ctx, "task-read-request", "整理样例状态", "", "", true, Delivery{Read: &read})
	}
	run, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := submit()
	if err != nil || duplicate.ID != run.ID {
		t.Fatalf("duplicate run: %+v, %v", duplicate, err)
	}
	read.Arguments = map[string]any{"mode": "large"}
	if _, err = submit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed arguments reused key: %v", err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := store.runTaskRead(ctx, run.ID)
	if err != nil || record.Result == nil || record.Result.Text != "样例服务状态：可用" {
		t.Fatalf("read: %+v, %v", record, err)
	}
	if _, err = store.runTaskRead(ctx, run.ID); !errors.Is(err, ErrState) {
		t.Fatalf("replayed first read: %v", err)
	}
	if err = store.finish(run, "failed", "样例模型失败", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Resume(ctx, run.TaskID, "continue-read", detail.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	reused, err := store.runTaskRead(ctx, next.ID)
	if err != nil || reused.Status != "reused" || reused.ReceivedAt != record.ReceivedAt || reused.Result.Text != record.Result.Text {
		t.Fatalf("reuse: %+v, %v", reused, err)
	}
	var calls int
	if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("wanted one external call, got %d: %v", calls, err)
	}
	firstRun, err := store.RunRecord(ctx, run.ID, run.TaskID)
	if err != nil || firstRun.EventAfter == 0 || len(firstRun.Messages) != 1 || len(firstRun.Reads) != 1 ||
		firstRun.Reads[0].Status != "completed" || firstRun.Reads[0].Result.Text != record.Result.Text || firstRun.InheritedInput != nil {
		t.Fatalf("original Run record: %+v, %v", firstRun, err)
	}
	retriedRun, err := store.RunRecord(ctx, next.ID, run.TaskID)
	if err != nil || len(retriedRun.Messages) != 0 || len(retriedRun.Reads) != 1 || retriedRun.Reads[0].Status != "reused" ||
		retriedRun.InheritedInput == nil || retriedRun.InheritedInput.ID != firstRun.Messages[0].ID {
		t.Fatalf("retried Run record: %+v, %v", retriedRun, err)
	}
}

func TestTaskReadRevocationHidesHistoryAndBlocksAlreadyQueuedReuse(t *testing.T) {
	ctx := context.Background()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "revocable-read", "整理样例状态", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if result, err := store.runTaskRead(ctx, run.ID); err != nil || result.Result == nil {
		t.Fatalf("original authorized result: %+v, %v", result, err)
	}
	if err = store.finish(run, "failed", "本地测试模型失败", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Resume(ctx, run.TaskID, "before-revocation", detail.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{run.ID, next.ID} {
		record, err := store.RunRecord(ctx, id, run.TaskID)
		if err != nil || len(record.Reads) != 1 || !record.Reads[0].SourceUnavailable ||
			record.Reads[0].Result != nil || record.Reads[0].request.Arguments != nil {
			t.Fatalf("private result or arguments remained visible: %+v, %v", record, err)
		}
	}
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != next.ID {
		t.Fatalf("wanted queued reuse: %+v, %v", claimed, err)
	}
	dispatched := false
	store.execute(ctx, testProvider(func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
		dispatched = true
		return nil, errors.New("must not send revoked read to model")
	}), claimed, RunLimits{OutputTokens: 64, ActiveTime: time.Minute, ModelTimeout: time.Minute})
	if dispatched {
		t.Fatal("old private result was still eligible for model context")
	}
	var settled, failure string
	if err = store.db.QueryRowContext(ctx, "SELECT status,error FROM runs WHERE id=?", next.ID).Scan(&settled, &failure); err != nil ||
		settled != "interrupted" || failure != ErrReadNeedsSelection.Error() {
		t.Fatalf("run did not stop for explicit reselection: %s / %s / %v", settled, failure, err)
	}
	var original string
	if err = store.db.QueryRowContext(ctx, "SELECT result FROM run_reads WHERE run_id=?", run.ID).Scan(&original); err != nil || original == "" {
		t.Fatalf("stored original lost: %v", err)
	}
	var calls int
	if err = store.db.QueryRowContext(ctx, "SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("original tool was replayed: %d, %v", calls, err)
	}
	c, err := store.Connection(ctx, read.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, c.Revision, true); err != nil {
		t.Fatal(err)
	}
	if err = store.checkTaskReadModelAuthority(ctx, next.ID); !errors.Is(err, ErrReadNeedsSelection) {
		t.Fatalf("regrant restored old source authority: %v", err)
	}
}

func TestTaskReadRecoveryDoesNotReplayDispatchedCall(t *testing.T) {
	ctx := context.Background()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "recover-task-read", "读取状态", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("UPDATE run_reads SET status='checking' WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	callID := id()
	if err = store.dispatchConnectionCall(ctx, callID, read.ConnectionID, mcpclient.Tool{Name: read.ToolName, Version: read.ToolVersion},
		read.Revision, run.ID, 1); err != nil {
		t.Fatal(err)
	}
	active, err := store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Control(ctx, run.TaskID, "stop", active.Task.Revision); err != nil {
		t.Fatal(err)
	}
	if err = store.Recover(); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || detail.Read == nil || detail.Read.Status != "unknown" || detail.Task.Status != "interrupted" || detail.Artifact != nil {
		t.Fatalf("recovered unconfirmed read: %+v, %v", detail.Read, err)
	}
	if _, err = store.Resume(ctx, run.TaskID, "continue-unknown", detail.Task.Revision); !errors.Is(err, ErrReadUnconfirmed) {
		t.Fatalf("resume bypassed an unknown read: %v", err)
	}
	if err = store.Control(ctx, run.TaskID, "stop", detail.Task.Revision); !errors.Is(err, ErrReadUnconfirmed) {
		t.Fatalf("stop bypassed an unknown read: %v", err)
	}
	if err = store.DeleteConversation(ctx, detail.Conversation.ID, detail.Conversation.Revision); !errors.Is(err, ErrReadUnconfirmed) {
		t.Fatalf("delete bypassed an unknown read: %v", err)
	}
	if err = store.AcknowledgeRead(ctx, run.TaskID, run.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil || detail.UnknownReadRunID != "" || detail.Read.Status != "unknown" {
		t.Fatalf("acknowledgment altered evidence: %+v, %v", detail, err)
	}
	if _, err = store.Resume(ctx, run.TaskID, "continue-unknown", detail.Task.Revision); !errors.Is(err, ErrReadNeedsSelection) {
		t.Fatalf("resume automatically retried: %v", err)
	}
	if err = store.Control(ctx, run.TaskID, "stop", detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteConversation(ctx, detail.Conversation.ID, detail.Conversation.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil || detail.Task.Status != "cancelled" || !detail.Conversation.Archived || detail.Read.Status != "unknown" {
		t.Fatalf("lost evidence after stop and archive: %+v, %v", detail, err)
	}
}

func TestTaskReadRechecksPermissionAfterSubmission(t *testing.T) {
	ctx := context.Background()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "queued-read", "读取状态", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := store.runTaskRead(ctx, run.ID)
	if !errors.Is(err, errReadFailed) || record.Status != "failed" || record.Result != nil {
		t.Fatalf("revoked tool was not blocked: %+v, %v", record, err)
	}
	var calls int
	if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("revoked tool reached dispatch: %d, %v", calls, err)
	}
}

func TestMCPDerivedReplyDoesNotReappearAfterRevocation(t *testing.T) {
	ctx := t.Context()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "derived-read", "用户原文应保留", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "样例服务状态：可用"
	provider := testProvider(func(_ context.Context, messages []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		for _, message := range messages {
			if message.Read != nil && message.Read.Result != nil && message.Read.Result.Text == marker {
				return nil, emit(message.Read.Result.Text)
			}
		}
		return nil, errors.New("fake model did not get fixture read")
	})
	store.execute(ctx, provider, run, RunLimits{OutputTokens: 64, ActiveTime: time.Minute, ModelTimeout: time.Minute})
	before, err := store.Detail(run.TaskID, true)
	if err != nil || before.Artifact == nil || before.Artifact.Content != marker || before.Run.Status != "completed" {
		t.Fatalf("expected genuine direct answer and artifact: %+v %v", before, err)
	}
	if err := store.OpenWorkspace(filepath.Join(t.TempDir(), "workspace")); err != nil {
		t.Fatal(err)
	}
	pending, err := store.RequestPublication(ctx, run.TaskID, before.Artifact.ID, "本地验证.md", "before-revocation-publication", before.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, _, err := store.Events(run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A separately authored reply in the same conversation has no MCP source.
	unrelated := "同会话的无关回复仍可查看"
	unrelatedRun := id()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO runs(id,conversation_id,status,response,created_at) VALUES(?,?,'completed',?,?)`, unrelatedRun, run.ConversationID, unrelated, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)`, run.ConversationID, unrelatedRun, unrelated); err != nil {
		t.Fatal(err)
	}
	if err := store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecidePublication(ctx, pending.ID, "approve", pending.taskRevision); !errors.Is(err, ErrReadNeedsSelection) {
		t.Fatalf("previous approval could newly publish revoked content: %v", err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.RunRecord(ctx, run.ID, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	history, err := store.MessageHistory(ctx, run.ConversationID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	model, err := store.modelMessages(ctx, run.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	events, state, err := store.Events(run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Artifact(before.Artifact.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"detail": detail, "run history": record, "messages": history, "model input": model, "events": events, "event state": state, "artifact": artifact} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), marker) {
			t.Errorf("%s re-exposed revoked source", name)
		}
	}
	if len(events) != len(beforeEvents) {
		t.Fatalf("SSE changed the retained sequence length: %d vs %d", len(events), len(beforeEvents))
	}
	for n, original := range beforeEvents {
		if events[n].Seq != original.Seq || (original.Kind == "message.delta" && events[n].Kind != "message.content_unavailable") ||
			(original.Kind != "message.delta" && events[n] != original) {
			t.Fatalf("SSE changed cursor/order or unrelated event: %+v -> %+v", original, events[n])
		}
	}
	// Reconnect from a cursor issued before revocation must preserve the same
	// sequence and redaction as a full stream, not resurrect source content.
	priorCursor := -1
	for i := 1; i < len(beforeEvents); i++ {
		if beforeEvents[i].Kind == "message.delta" {
			priorCursor = i - 1
			break
		}
	}
	if priorCursor < 0 {
		t.Fatal("fixture did not emit a resumable reply fragment")
	}
	replayed, _, err := store.ReplayRunEvents(run.ID, beforeEvents[priorCursor].Seq)
	if err != nil || !slices.Equal(replayed, events[priorCursor+1:]) {
		t.Fatalf("resumed stream did not preserve redaction: %+v (%v)", replayed, err)
	}
	if !record.ResponseUnavailable || len(record.Messages) < 2 || !record.Messages[1].ContentUnavailable ||
		detail.Artifact == nil || !detail.Artifact.ContentUnavailable ||
		len(detail.ArtifactVersions) == 0 || !detail.ArtifactVersions[0].ContentUnavailable {
		t.Fatalf("missing explicit unavailability marker: %+v %+v", record, detail.Artifact)
	}
	latestModel, err := json.Marshal(model)
	if err != nil || !strings.Contains(string(latestModel), unrelated) || strings.Contains(string(latestModel), "此回复可能包含已撤权来源") {
		t.Fatalf("unrelated reply missing or redacted text passed into model: %s / %v", latestModel, err)
	}
	var raw string
	if err := store.db.QueryRowContext(ctx, "SELECT response FROM runs WHERE id=?", run.ID).Scan(&raw); err != nil || raw != marker {
		t.Fatalf("run audit changed: %q %v", raw, err)
	}
	if state.Status != "completed" || len(history.Items) == 0 || history.Items[0].Text != "用户原文应保留" {
		t.Fatalf("actual status or user message changed: %+v %+v", detail.Run, history.Items)
	}
}

func TestMCPRevokedFinishedReadIsNoLongerAReadyCheckpoint(t *testing.T) {
	ctx := t.Context()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "revoked-checkpoint", "恢复读取", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.runTaskRead(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `INSERT INTO run_read_checkpoints(run_id,version,provider_binding,context_digest,output_budget,active_budget_ms)
VALUES (?,?,'local-test','local-test',128,60000)`, run.ID, readCheckpointVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(run, "interrupted", "", runAccounting{errorCode: "model_timeout"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Detail(run.TaskID, true)
	if err != nil || before.ReadCheckpoint != "ready" {
		t.Fatalf("valid checkpoint is not ready: %q / %v", before.ReadCheckpoint, err)
	}
	if err = store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
		t.Fatal(err)
	}
	after, err := store.Detail(run.TaskID, true)
	if err != nil || after.ReadCheckpoint != "unavailable" || after.Run.Status != "interrupted" {
		t.Fatalf("revoked checkpoint is not unavailable, or status changed: %+v / %v", after, err)
	}
}
