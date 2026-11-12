package work

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestTaskRunHistoryPaginatesWithinTask(t *testing.T) {
	store, _, _, detail := publicationFixture(t)
	ctx := context.Background()
	first := detail.Run.ID
	var newest Run
	for i := 0; i < runPageSize; i++ {
		var err error
		newest, err = store.Submit(ctx, id(), "补充要求\n详细内容", "", detail.Conversation.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.claim(ctx); err != nil {
			t.Fatal(err)
		}
		if err = store.finish(newest, "failed", "样例执行失败", runAccounting{}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.TaskRuns(ctx, detail.Task.ID, "")
	if err != nil || len(page.Items) != runPageSize || page.NextCursor == "" || page.Items[0].ID != newest.ID || page.Items[0].InputExcerpt != "补充要求" {
		t.Fatalf("first page: %+v, %v", page, err)
	}
	tail, err := store.TaskRuns(ctx, detail.Task.ID, page.NextCursor)
	if err != nil || len(tail.Items) != 1 || tail.Items[0].ID != first || tail.NextCursor != "" {
		t.Fatalf("last page: %+v, %v", tail, err)
	}
	other, err := store.Submit(ctx, id(), "独立事项", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.TaskRuns(ctx, other.TaskID, page.NextCursor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("accepted foreign cursor: %v", err)
	}
	if _, err = store.RunRecord(ctx, first, other.TaskID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("accepted foreign Run: %v", err)
	}
}
