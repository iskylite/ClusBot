package work

import (
	"context"
	"testing"
	"time"
)

func TestInputRequestIsCommittedWithQuestionAndTaskState(t *testing.T) {
	store, _, _, detail := publicationFixture(t)
	ctx := context.Background()
	run, err := store.Submit(ctx, "input-request", "核对上线时间", "", detail.Conversation.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, run.ID, "模型流式前言"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`CREATE TRIGGER reject_question BEFORE INSERT ON messages WHEN NEW.role='assistant'
BEGIN SELECT RAISE(ABORT,'question write failed'); END`); err != nil {
		t.Fatal(err)
	}
	answer := runAccounting{question: "请确认具体上线日期。"}
	if err = store.finish(run, "completed", "", answer); err == nil {
		t.Fatal("reported a waiting question that could not be saved")
	}
	current, err := store.Detail(run.TaskID, true)
	if err != nil || current.Task.Status != "active" || current.Run.Status != "running" || current.Run.Response != "模型流式前言" {
		t.Fatalf("failed commit changed the task or Run: %+v, %v", current, err)
	}
	if _, err = store.db.Exec("DROP TRIGGER reject_question"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(run, "completed", "", answer); err != nil {
		t.Fatal(err)
	}
	current, err = store.Detail(run.TaskID, true)
	if err != nil || current.Task.Status != "waiting_user" || current.Run.Status != "input_required" || current.Run.Response != answer.question {
		t.Fatalf("waiting Task: %+v, %v", current, err)
	}
	entry, err := store.RunRecord(ctx, run.ID, run.TaskID)
	if err != nil || len(entry.Artifacts) != 0 || len(entry.Messages) != 2 || entry.Messages[1].Role != "assistant" || entry.Messages[1].Text != answer.question {
		t.Fatalf("clarification Run: %+v, %v", entry, err)
	}
}

func TestInputRequestFollowupKeepsUnavailableReadEvidence(t *testing.T) {
	store, successful := taskReadFixture(t)
	ctx := context.Background()
	failed := successful
	failed.Arguments = map[string]any{"mode": "error"}
	first, err := store.SubmitWithDelivery(ctx, "two-sources", "核对上线条件", "", "", true, Delivery{Reads: []TaskReadRequest{failed, successful}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	selected, err := listTaskReads(ctx, store.db, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, err := store.readTaskSources(ctx, first.ID, selected)
	if err != nil || len(results) != 2 || results[0].Status != "tool_error" || results[1].Status != "completed" {
		t.Fatalf("original readings: %+v, %v", results, err)
	}
	if err = store.finish(first, "completed", "", runAccounting{question: "上线日期是哪天？"}); err != nil {
		t.Fatal(err)
	}

	for _, text := range []string{"周四上线", "补充接收人"} {
		next, err := store.Submit(ctx, id(), text, "", first.ConversationID, false)
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.claim(ctx)
		if err != nil || run.ID != next.ID || run.TaskID != first.TaskID {
			t.Fatalf("follow-up claim: %+v, %v", run, err)
		}
		store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			reads := input[len(input)-1].Reads
			if len(reads) != 2 || reads[0].Position != 1 || reads[0].Status != "tool_error" || reads[0].SourceRunID != first.ID ||
				reads[1].Position != 2 || reads[1].Status != "reused" || reads[1].ReceivedAt != results[1].ReceivedAt {
				t.Fatalf("model lost source coverage: %+v", reads)
			}
			return nil, emit("仍有一个来源未核实。")
		}), run, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 64})
		record, err := store.RunRecord(ctx, run.ID, first.TaskID)
		if err != nil || record.Status != "completed" || len(record.Reads) != 2 || record.Reads[0].SourceRunID != first.ID ||
			record.Reads[0].Result == nil || record.Reads[0].Result.Text != results[0].Result.Text || len(record.Artifacts) != 1 {
			t.Fatalf("missing durable reading evidence: %+v, %v", record, err)
		}
	}

	refreshed, err := store.SubmitWithDelivery(ctx, "refresh-read", "重新读取", "", first.ConversationID, false, Delivery{Read: &successful})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(refreshed.TaskID, true)
	if err != nil || detail.Read == nil || detail.Read.Status != "queued" || detail.Read.SourceRunID != "" || len(detail.Reads) != 0 {
		t.Fatalf("explicit source selection inherited prior failures: %+v, %v", detail, err)
	}
	var calls int
	if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("follow-ups repeated source calls: %d, %v", calls, err)
	}
}
