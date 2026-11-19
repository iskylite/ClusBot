package work

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"clusbot/internal/mcpclient"
)

func TestConversationMCPSelectionOnlyNarrowsExistingReadCandidates(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "conversation-mcp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, secretEnv = "user:session", "CLUSBOT_TEST_SELECTION_PRIVATE_ENV"
	t.Setenv(secretEnv, "original")
	cfg := mcpclient.Config{Transport: "http", Endpoint: "https://never-connect.invalid/private", TokenEnv: secretEnv}
	conn, err := s.SaveConnection(ctx, "", Connection{Name: "Fixture", Enabled: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cfg.CredentialBinding(s.connectionKey)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE connections SET status='ready',credential_binding=? WHERE id=?", credential, conn.ID)
	for i, name := range []string{"fixture-first", "fixture-second", "not-authorized"} {
		tool := mcpclient.Tool{Name: name, Version: digest(name), InputSchema: json.RawMessage(`{"type":"object"}`)}
		definition, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO connection_tools VALUES(?,?,?,?)", conn.ID, name, string(definition), i < 2)
	}
	chat, handled, err := s.SubmitBenchmarkCommand(ctx, "new-chat", actor, "", "跑 POD7 全套基准", "", nil)
	if err != nil || !handled {
		t.Fatalf("cannot create idle conversation: %+v %v", chat, err)
	}
	choice := func(name string) ConversationMCPInput {
		return ConversationMCPInput{Action: "bind_mcp_read", MCPReadSelection: MCPReadSelection{
			ConnectionID: conn.ID, Revision: conn.Revision, ToolName: name, ToolVersion: digest(name),
		}}
	}
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, choice("not-authorized")); !errors.Is(err, ErrToolPermission) {
		t.Fatalf("selection granted new authority: %v", err)
	}
	if _, err := s.SetConversationMCPSelection(ctx, "other", chat.ConversationID, choice("fixture-second")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign principal selected a tool: %v", err)
	}
	selected, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, choice("fixture-second"))
	if err != nil || selected.Selection == nil || selected.LocalValidity != "locally_ready" {
		t.Fatalf("current selection: %+v %v", selected, err)
	}
	clearFirst := ConversationMCPInput{Action: "clear_mcp_read", ExpectedBindingRevision: &selected.BindingRevision}
	if selected.BindingRevision != 1 {
		t.Fatalf("first selection has wrong revision: %+v", selected)
	}
	cleared, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, clearFirst)
	if err != nil || cleared.Selection != nil || cleared.BindingRevision != 2 {
		t.Fatalf("lost cleared MCP revision: %+v %v", cleared, err)
	}
	renew := choice("fixture-second")
	renew.ExpectedBindingRevision = &cleared.BindingRevision
	again, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, renew)
	if err != nil || again.BindingRevision != 3 {
		t.Fatalf("same MCP pin did not advance: %+v %v", again, err)
	}
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, clearFirst); !errors.Is(err, ErrConversationBindingConflict) {
		t.Fatalf("stale clear won after MCP ABA: %v", err)
	}
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, choice("fixture-first")); !errors.Is(err, ErrConversationBindingConflict) {
		t.Fatalf("omitting MCP revision overwrote existing selection: %v", err)
	}
	if same, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, choice("fixture-second")); err != nil || same.BindingRevision != 3 {
		t.Fatalf("legacy no-op unexpectedly mutated revision: %+v %v", same, err)
	}
	drift := choice("fixture-first")
	drift.ExpectedBindingRevision = &again.BindingRevision
	drift.Revision++
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, drift); !errors.Is(err, ErrToolPermission) {
		t.Fatalf("CAS bypassed source connection revision: %v", err)
	}

	// Both windows use the same snapshot and attempt actual, distinct writes.
	left := choice("fixture-first")
	right := ConversationMCPInput{Action: "clear_mcp_read"}
	left.ExpectedBindingRevision, right.ExpectedBindingRevision = &again.BindingRevision, &again.BindingRevision
	begin := make(chan struct{})
	outcomes := make(chan error, 2)
	for _, window := range []ConversationMCPInput{left, right} {
		go func() {
			<-begin
			_, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, window)
			outcomes <- err
		}()
	}
	close(begin)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-outcomes
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConversationBindingConflict):
			conflicts++
		default:
			t.Fatalf("unexpected competing edit: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("competing writes both applied: %d/%d", successes, conflicts)
	}
	next := choice("fixture-second")
	current := again.BindingRevision + 1
	next.ExpectedBindingRevision = &current
	again, err = s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, next)
	if err != nil || again.BindingRevision != current+1 {
		t.Fatalf("cannot recover using actual revision: %+v %v", again, err)
	}

	tools, err := s.TaskTools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("selection changed global grants: %+v %v", tools, err)
	}
	if _, err := s.Submit(ctx, "chosen-run", "Organize the report", "reports", chat.ConversationID, true); err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := s.automaticTaskTools(ctx, run)
	if err != nil || len(automatic) != 1 || automatic[0].source.ToolName != "fixture-second" ||
		automatic[0].source.request.Revision != conn.Revision {
		t.Fatalf("actual model candidate list not narrowed: %+v %v", automatic, err)
	}
	if view, err := s.ReadRunMCPBindings(ctx, actor, run.ID); err != nil || view.AutomaticPin == nil ||
		view.AutomaticPin.ToolName != "fixture-second" || len(view.Bindings) != 0 {
		t.Fatalf("pin should be distinct from actual tool invocation: %+v %v", view, err)
	}

	clear := ConversationMCPInput{Action: "clear_mcp_read", ExpectedBindingRevision: &again.BindingRevision}
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, clear); !errors.Is(err, ErrActive) {
		t.Fatalf("changed an active run's context: %v", err)
	}
	t.Setenv(secretEnv, "rotated")
	if err := s.checkAutomaticMCPPin(ctx, run.ID); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("new model round used drifted credential binding: %v", err)
	}
	t.Setenv(secretEnv, "original")
	exec("UPDATE connection_tools SET allowed=0 WHERE connection_id=? AND name='fixture-second'", conn.ID)
	if out, err := s.ReadConversationMCPSelection(ctx, actor, chat.ConversationID); err != nil || out.LocalValidity != "unavailable" {
		t.Fatalf("revoked selection reported ready: %+v %v", out, err)
	}
	if candidates, err := s.automaticTaskTools(ctx, run); !errors.Is(err, ErrReadCheckpoint) || len(candidates) != 0 {
		t.Fatalf("revocation silently widened candidate set: %+v %v", candidates, err)
	}
	exec("UPDATE runs SET status='interrupted' WHERE id=?", run.ID)
	if value, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, clear); err != nil || value.Selection != nil {
		t.Fatalf("idle selection clear: %+v %v", value, err)
	}
	exec("UPDATE conversations SET archived=1 WHERE id=?", chat.ConversationID)
	if _, err := s.SetConversationMCPSelection(ctx, actor, chat.ConversationID, clear); !errors.Is(err, ErrConversationArchived) {
		t.Fatalf("archived MCP binding changed: %v", err)
	}
	run.Generation = 1
	if candidates, err := s.automaticTaskTools(ctx, run); !errors.Is(err, ErrReadCheckpoint) || len(candidates) != 0 {
		t.Fatalf("clearing in conversation overrode durable run pin: %+v %v", candidates, err)
	}
}
