package work

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskTitleUsesFirstSentenceWithoutSplittingIdentifiers(t *testing.T) {
	for _, example := range []struct{ goal, heading string }{
		{"整理本周会议纪要。保留负责人和截止日期。", "整理本周会议纪要"},
		{"Review v1.2 and README.md. Include changes.", "Review v1.2 and README.md"},
		{"1. Review alerts. Include sources.", "1. Review alerts"},
		{"核对「副本是否就绪？」。附查询时间。", "核对「副本是否就绪？」"},
		{"整理本周会议纪要。", "整理本周会议纪要。"},
		{"\n\n对齐发布清单\n保留验收记录", "对齐发布清单"},
		{strings.Repeat("工", 31), strings.Repeat("工", 30) + "…"},
	} {
		if got := taskTitle(example.goal); got != example.heading {
			t.Errorf("title for %q = %q, want %q", example.goal, got, example.heading)
		}
	}
}

func TestTaskKeepsIdentityAcrossRunsAndRequiresAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusbot.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	ctx := context.Background()
	submit := func(key, message, conversation string, task bool) Run {
		t.Helper()
		run, err := store.Submit(ctx, key, message, "工作资料", conversation, task)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := submit("request-1", "整理周报", "", true)
	if again := submit("request-1", "整理周报", "", true); again != first {
		t.Fatalf("duplicate changed receipt: %+v", again)
	}
	if _, err := store.Submit(ctx, "request-1", "其他事项", "工作资料", "", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("wanted request conflict, got %v", err)
	}
	if _, err := store.Submit(ctx, "request-2", "补充要求", "", first.ConversationID, false); !errors.Is(err, ErrActive) {
		t.Fatalf("wanted active run conflict, got %v", err)
	}
	run, err := store.claim(ctx)
	if err != nil || run.ID != first.ID {
		t.Fatalf("claim %+v: %v", run, err)
	}
	if err := store.delta(ctx, run.ID, "# 周报\n"); err != nil {
		t.Fatal(err)
	}
	if err := store.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := store.Detail(first.TaskID, true)
	if err != nil || d.Task.Status != "waiting_user" || d.Artifact == nil {
		t.Fatalf("draft must await review: %+v, %v", d, err)
	}
	if err := store.Accept(ctx, first.TaskID, d.Artifact.ID, d.Task.Revision-1); !errors.Is(err, ErrState) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if _, err := store.db.Exec("UPDATE artifacts SET content='unexpected change' WHERE id=?", d.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(ctx, first.TaskID, d.Artifact.ID, d.Task.Revision); !errors.Is(err, ErrState) {
		t.Fatalf("changed content accepted: %v", err)
	}
	if _, err := store.db.Exec("UPDATE artifacts SET content=? WHERE id=?", d.Artifact.Content, d.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(ctx, first.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err = store.Detail(first.TaskID, true)
	if err != nil || d.Task.Status != "completed" || d.Artifact.Content != "# 周报\n" || d.Acceptance == nil || d.Acceptance.Digest != d.Artifact.Digest {
		t.Fatalf("reopen lost completed work: %+v, %v", d, err)
	}
	next := submit("request-2", "补充一段结论", first.ConversationID, false)
	if next.TaskID != first.TaskID || next.ID == first.ID {
		t.Fatalf("follow-up did not keep Task identity: %+v", next)
	}
}

func TestPromoteConversationKeepsHistory(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	chat, err := store.Submit(ctx, "chat-request", "讨论接口", "", "", false)
	if err != nil || chat.TaskID != "" {
		t.Fatalf("ordinary chat created task: %+v, %v", chat, err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, chat.ID, "接口讨论回复"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(chat, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	goal := "把讨论整理成记录。逐项列出接口变更和验收条件。"
	promoted, err := store.Submit(ctx, "promote-request", goal, "ClusBot", chat.ConversationID, true)
	if err != nil || promoted.TaskID == "" || promoted.ConversationID != chat.ConversationID {
		t.Fatalf("promotion lost identity: %+v, %v", promoted, err)
	}
	d, err := store.Detail(promoted.TaskID, true)
	if err != nil || len(d.Messages) != 3 || d.Messages[1].Text != "接口讨论回复" {
		t.Fatalf("promotion lost messages: %+v, %v", d, err)
	}
	if d.Task.Title != "把讨论整理成记录" || d.Task.Goal != goal || d.Messages[2].Text != goal || d.Conversation.Title != "讨论接口" {
		t.Fatalf("promotion changed the original goal or conversation: %+v", d)
	}
}

func TestChatDelegationAdmission(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	chat, err := store.Submit(ctx, "first-question", "接口要如何验收？", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, chat.ID, "验收需要覆盖错误码。"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(chat, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	const requestKey, requestText = "chat-delegation", "请把刚才的结论整理成验收清单"
	classify := func(_ context.Context, current string, history []Message) bool {
		if current != requestText || len(history) != 2 || history[0].Text != "接口要如何验收？" || history[1].Text != "验收需要覆盖错误码。" {
			t.Fatalf("wrong intent input: %q %+v", current, history)
		}
		return true
	}
	// Fail after Task insertion, then retry the same submission.
	if _, err = store.db.Exec(`CREATE TRIGGER fail_chat_delegation BEFORE INSERT ON messages
WHEN NEW.text='请把刚才的结论整理成验收清单' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SubmitChat(ctx, requestKey, requestText, "", chat.ConversationID, Delivery{}, classify); err == nil {
		t.Fatal("injected failure did not roll back")
	}
	d, err := store.Detail(chat.ConversationID, false)
	if err != nil || len(d.Messages) != 2 || d.Run == nil || d.Run.ID != chat.ID {
		t.Fatalf("failed submission changed original conversation: %+v, %v", d, err)
	}
	var tasks int
	if err = store.db.QueryRow("SELECT count(*) FROM tasks WHERE conversation_id=?", chat.ConversationID).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("failed submission left %d Tasks: %v", tasks, err)
	}
	if _, err = store.db.Exec("DROP TRIGGER fail_chat_delegation"); err != nil {
		t.Fatal(err)
	}
	promoted, err := store.SubmitChat(ctx, requestKey, requestText, "", chat.ConversationID, Delivery{}, classify)
	if err != nil || promoted.TaskID == "" || promoted.ConversationID != chat.ConversationID {
		t.Fatalf("promotion failed: %+v, %v", promoted, err)
	}
	unexpected := func(context.Context, string, []Message) bool {
		t.Fatal("accepted or bound chat was reclassified")
		return false
	}
	replayed, err := store.SubmitChat(ctx, requestKey, requestText, "", chat.ConversationID, Delivery{}, unexpected)
	if err != nil || replayed != promoted {
		t.Fatalf("promotion replay changed receipt: %+v, %v", replayed, err)
	}
	if _, err = store.SubmitChat(ctx, requestKey, "不同内容", "", chat.ConversationID, Delivery{}, unexpected); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused key accepted a different message: %v", err)
	}
	if _, err = store.SubmitChat(ctx, "chat-other-key", "另一条补充要求", "", chat.ConversationID, Delivery{}, unexpected); !errors.Is(err, ErrActive) {
		t.Fatalf("parallel input was accepted during active run: %v", err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(promoted, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	followUp, err := store.SubmitChat(ctx, "chat-follow-up", "再补一项", "", chat.ConversationID, Delivery{}, unexpected)
	if err != nil || followUp.TaskID != promoted.TaskID || followUp.ID == promoted.ID {
		t.Fatalf("supplement did not reuse Task: %+v, %v", followUp, err)
	}
}

func TestConcurrentChatDelegationCreatesOneTask(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat, err := store.Submit(ctx, "original-chat", "讨论周报", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(chat, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		run Run
		err error
	}
	started := make(chan struct{}, 3)
	primary := make(chan struct{})
	competing := make(chan struct{})
	send := func(key, text string, gate <-chan struct{}) <-chan result {
		out := make(chan result, 1)
		go func() {
			run, err := store.SubmitChat(ctx, key, text, "", chat.ConversationID, Delivery{}, func(ctx context.Context, _ string, _ []Message) bool {
				started <- struct{}{}
				select {
				case <-gate:
					return true
				case <-ctx.Done():
					return false
				}
			})
			out <- result{run, err}
		}()
		return out
	}
	first := send("promote-1", "请整理一份周报", primary)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first classification did not start")
	}
	retry := send("promote-1", "请整理一份周报", competing)
	other := send("promote-2", "请生成另一份报告", competing)
	for range 1 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("classification held a database transaction open")
		}
	}
	close(primary)
	winner := <-first
	close(competing)
	if winner.err != nil || winner.run.TaskID == "" {
		t.Fatalf("first promotion failed: %+v", winner)
	}
	if replay := <-retry; replay.err != nil || replay.run != winner.run {
		t.Fatalf("concurrent retry did not return the same Run: %+v", replay)
	}
	if conflict := <-other; !errors.Is(conflict.err, ErrActive) {
		t.Fatalf("concurrent different input was not rejected: %+v", conflict)
	}
}

func TestPauseAndResumeKeepTaskAndRetainPreviousRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.Submit(ctx, "control-original", "整理资料", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Detail(first.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Control(ctx, first.TaskID, "pause", before.Task.Revision); err != nil {
		t.Fatal(err)
	}
	// Completion racing with the persisted pause must not publish a draft.
	if err := store.finish(running, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.Detail(first.TaskID, true)
	if err != nil || paused.Task.Status != "interrupted" || paused.Task.StatusReason != "paused" || paused.Run.Status != "interrupted" || paused.Artifact != nil {
		t.Fatalf("pause did not win: %+v, %v", paused, err)
	}
	snapshot, err := store.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 0 || len(snapshot.Groups["active"]) != 1 || snapshot.Groups["active"][0].StatusReason != "paused" {
		t.Fatalf("overview lost paused state: %+v, %v", snapshot.Groups, err)
	}
	next, err := store.Resume(ctx, first.TaskID, "control-resume", paused.Task.Revision)
	if err != nil || next.ID == first.ID || next.TaskID != first.TaskID {
		t.Fatalf("resume changed Task or failed: %+v, %v", next, err)
	}
	retry, err := store.Resume(ctx, first.TaskID, "control-resume", paused.Task.Revision)
	if err != nil || retry.ID != next.ID {
		t.Fatalf("resume retry duplicated run: %+v, %v", retry, err)
	}
	current, err := store.Detail(first.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Control(ctx, first.TaskID, "stop", current.Task.Revision); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.Detail(first.TaskID, true)
	if err != nil || stopped.Task.Status != "cancelled" || stopped.Run.Status != "cancelled" {
		t.Fatalf("queued stop did not cancel: %+v, %v", stopped, err)
	}
}
