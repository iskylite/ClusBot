package work

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
