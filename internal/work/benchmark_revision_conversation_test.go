package work

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationCollectiveRevisionUsesLocalCaseAndRequiresConfirmation(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "revision.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	p := &benchFixtureProvider{}
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: binding.Connection, Revision: 1, Tool: binding.Tool, ExpiresAt: now() + 60000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "serial-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	var cases []BenchmarkCase
	for _, key := range []string{"first", "collective", "deepep"} {
		cases = append(cases, BenchmarkCase{Key: key, Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}})
	}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "profile", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"revision-fixture", "profile", 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	chat, _, err := s.SubmitBenchmarkCommand(ctx, "revision-chat", actor, "", "/benchmark-status "+task, "", p)
	if err != nil {
		t.Fatal(err)
	}
	text := "查 " + task[:8] + " 四层状态，把未派发的集合通信测试移除，先预览影响。"
	target := BenchmarkRevisionTarget{TaskID: task, ExpectedRevision: 1}
	suggestion, err := s.SubmitBenchmarkRevisionSuggestion(ctx, "revision-message", actor, chat.ConversationID, text, "", target)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.ReadBenchmarkRevisionSuggestionForActor(ctx, actor, chat.ConversationID, suggestion.ID)
	if err != nil || restored == nil || *restored != target {
		t.Fatalf("authorized card: %+v %v", restored, err)
	}
	for _, bad := range []struct{ actor, conversation string }{{"other", chat.ConversationID}, {actor, strings.Repeat("a", 32)}} {
		if value, err := s.ReadBenchmarkRevisionSuggestionForActor(ctx, bad.actor, bad.conversation, suggestion.ID); err != nil || value != nil {
			t.Fatalf("foreign card: %+v %v", value, err)
		}
	}
	preview, err := s.SubmitBenchmarkRevisionChat(ctx, actor, chat.ConversationID, BenchmarkRevisionChatInput{Action: "preview_benchmark_revision", SourceRunID: suggestion.ID, RequestID: "revision-preview"}, p,
		func(context.Context, BenchmarkRevisionModelInput, int) (string, error) {
			t.Fatal("fixed local wording should not call model")
			return "", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Proposal.Method != "explicit_shortcut" || len(preview.Proposal.Preview.Changes.Removed) != 1 || preview.Proposal.Preview.Changes.Removed[0] != "collective" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	before, err := s.ReadBenchmarkRevision(ctx, actor, task, 0)
	if err != nil || before.Revision != 1 || len(before.Profile.Cases) != 3 {
		t.Fatalf("preview mutated plan: %+v %v", before, err)
	}
	if _, err := s.ReopenBenchmarkRevisionChat(ctx, actor, chat.ConversationID, preview.RunID, p); err != nil {
		t.Fatalf("reopen preview: %v", err)
	}
	metadata, err := s.ReadBenchmarkRevisionChatMetadata(ctx, actor, chat.ConversationID, preview.RunID)
	if err != nil || metadata.TaskID != task || metadata.SourceRunID != suggestion.ID || metadata.ExpectedRevision != 1 || metadata.AppliedRevision != 0 {
		t.Fatalf("preview metadata: %+v %v", metadata, err)
	}
	if _, err := s.ReadBenchmarkRevisionChatMetadata(ctx, "other", chat.ConversationID, preview.RunID); err == nil {
		t.Fatal("another principal read preview metadata")
	}
	if _, err := s.db.Exec("UPDATE benchmark_cases SET phase='submitting' WHERE task_id=? AND case_key='collective'", task); err != nil {
		t.Fatal(err)
	}
	confirm := BenchmarkRevisionChatConfirmInput{Action: "apply_benchmark_revision", ExpectedRevision: 1}
	if _, err := s.ConfirmBenchmarkRevisionChat(ctx, actor, chat.ConversationID, preview.RunID, confirm, p); !errors.Is(err, ErrBenchmarkRevisionPreviewStale) {
		t.Fatalf("dispatched case must be stale: %v", err)
	}
	if _, err := s.db.Exec("UPDATE benchmark_cases SET phase='queued' WHERE task_id=? AND case_key='collective'", task); err != nil {
		t.Fatal(err)
	}
	if receipt, err := s.ConfirmBenchmarkRevisionChat(ctx, actor, chat.ConversationID, preview.RunID, confirm, p); err != nil || receipt.Revision != 2 || receipt.State != "applied" {
		t.Fatalf("confirmation: %+v %v", receipt, err)
	}
	metadata, err = s.ReadBenchmarkRevisionChatMetadata(ctx, actor, chat.ConversationID, preview.RunID)
	if err != nil || metadata.AppliedRevision != 2 {
		t.Fatalf("applied receipt: %+v %v", metadata, err)
	}
	second, err := s.SubmitBenchmarkRevisionSuggestion(ctx, "revision-second", actor, chat.ConversationID, text, "", BenchmarkRevisionTarget{TaskID: task, ExpectedRevision: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitBenchmarkRevisionChat(ctx, actor, chat.ConversationID, BenchmarkRevisionChatInput{Action: "preview_benchmark_revision", SourceRunID: second.ID, RequestID: "revision-missing-case"}, p,
		func(context.Context, BenchmarkRevisionModelInput, int) (string, error) {
			t.Fatal("missing collective must not choose another case")
			return "", nil
		}); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("missing local case: %v", err)
	}
}

func TestConversationCollectiveRevisionShortcutIsBounded(t *testing.T) {
	for _, text := range []string{"如果把未派发的集合通信测试移除，先预览影响。", "不要把未派发的集合通信测试移除，先预览影响。", "把未派发的其他测试移除，先预览影响。", "把未派发的集合通信测试移除，先预览影响？"} {
		if _, ok, _ := benchmarkRevisionChatShortcut(text, false); ok {
			t.Fatalf("unsafe shortcut %q", text)
		}
	}
	if _, ok, err := benchmarkRevisionChatShortcut("将测试项 first 的参数 size 改为 32 并同步实测要求", false); !ok || !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("actual configuration confirmation bypassed: %v %v", ok, err)
	}
}
