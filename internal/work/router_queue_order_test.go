package work

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRouterQueueOrderSchedulesBothTasksWithoutChangingTiers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	second, err := s.Submit(ctx, "relative-second-task", "写会议纪要", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Submit(ctx, "relative-first-task", "写项目周报", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Submit(ctx, "relative-other-task", "整理账单", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE tasks SET priority='high' WHERE id=?", second.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE tasks SET priority='low' WHERE id=?", first.TaskID); err != nil {
		t.Fatal(err)
	}
	sendOrder := func(key, text, before, after string) Run {
		t.Helper()
		run, err := s.SubmitRoutedChat(ctx, key, text, "", "", Delivery{},
			func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
				return DelegationResult{Route: &RouteDecision{Kind: "order_tasks", TaskID: before, AfterTaskID: after}}, nil
			})
		if err != nil || run.RouterIntents == nil || len(*run.RouterIntents) != 1 {
			t.Fatalf("order receipt: %+v, %v", run, err)
		}
		item := (*run.RouterIntents)[0]
		if item.Status != "applied" || item.TaskID != before || item.AfterTaskID != after ||
			!strings.Contains(run.Response, "/tasks/"+before) || !strings.Contains(run.Response, "/tasks/"+after) {
			t.Fatalf("order did not record both tasks: %+v", item)
		}
		return run
	}
	sendOrder("relative-initial-order", "先写会议纪要，再写项目周报", second.TaskID, first.TaskID)
	const text = "先写项目周报，再写会议纪要"
	reversed := sendOrder("relative-reverse-order", text, first.TaskID, second.TaskID)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	retry := sendOrder("relative-reverse-order", text, first.TaskID, second.TaskID)
	if retry.ID != reversed.ID || !reflect.DeepEqual(retry.RouterIntents, reversed.RouterIntents) {
		t.Fatalf("retry changed the receipt: %+v", retry)
	}
	for _, item := range []struct {
		run      Run
		priority string
	}{{other, "normal"}, {first, "low"}, {second, "high"}} {
		run, err := s.claim(ctx)
		if err != nil || run.ID != item.run.ID {
			t.Fatalf("claim order: %+v, want %s, %v", run, item.run.ID, err)
		}
		detail, err := s.Detail(run.TaskID, true)
		if err != nil || detail.Task.Priority != item.priority {
			t.Fatalf("order changed the absolute priority: %+v, %v", detail.Task, err)
		}
	}
}

func TestRouterQueueOrderClarifiesOnlyConflictingIntents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	plan, err := s.CreateWorkflow(ctx, "relative-workflow", WorkflowRequest{Goal: "整理资料", Criteria: "列出已核对成果", Steps: []WorkflowStep{
		{Key: "collect", Goal: "采集资料", Criteria: "列来源"},
		{Key: "review", Goal: "核对资料", Criteria: "列差异", Depends: []string{"collect"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	collect, review := plan.Children[0].TaskID, plan.Children[1].TaskID
	other, err := s.Submit(ctx, "relative-order-other", "整理账单", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := s.Submit(ctx, "relative-order-extra", "整理周报", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	const text = "先核对再采集；先采集再整理账单；先整理账单再核对；先核对再整理周报；先做那件事再整理周报；周报设为高优先级"
	receipt, err := s.SubmitRoutedChat(ctx, "relative-batch-conflict", text, "", "", Delivery{}, batchRouter(
		RouteDecision{Kind: "order_tasks", TaskID: review, AfterTaskID: collect, Fragment: "先核对再采集"},
		RouteDecision{Kind: "order_tasks", TaskID: collect, AfterTaskID: other.TaskID, Fragment: "先采集再整理账单"},
		RouteDecision{Kind: "order_tasks", TaskID: other.TaskID, AfterTaskID: review, Fragment: "先整理账单再核对"},
		RouteDecision{Kind: "order_tasks", TaskID: review, AfterTaskID: extra.TaskID, Fragment: "先核对再整理周报"},
		RouteDecision{Kind: "order_tasks", TaskID: "unknown", AfterTaskID: extra.TaskID, Fragment: "先做那件事再整理周报"},
		RouteDecision{Kind: "set_priority", TaskID: extra.TaskID, Priority: "high", Fragment: "周报设为高优先级"},
	))
	if err != nil || receipt.RouterIntents == nil || len(*receipt.RouterIntents) != 6 {
		t.Fatalf("batch receipt: %+v, %v", receipt, err)
	}
	for index, want := range []string{"clarify", "applied", "applied", "applied", "clarify", "applied"} {
		if got := (*receipt.RouterIntents)[index]; got.Status != want {
			t.Fatalf("intent %d status: %+v", index, got)
		}
	}
	if !strings.Contains((*receipt.RouterIntents)[0].Detail, "任务依赖") {
		t.Fatalf("reverse dependency was not explained: %+v", (*receipt.RouterIntents)[0])
	}
	cycle, err := s.SubmitRoutedChat(ctx, "relative-batch-cycle", "先写周报再整理账单；先整理账单再采集；先采集再写周报", "", "", Delivery{}, batchRouter(
		RouteDecision{Kind: "order_tasks", TaskID: extra.TaskID, AfterTaskID: other.TaskID, Fragment: "先写周报再整理账单"},
		RouteDecision{Kind: "order_tasks", TaskID: other.TaskID, AfterTaskID: collect, Fragment: "先整理账单再采集"},
		RouteDecision{Kind: "order_tasks", TaskID: collect, AfterTaskID: extra.TaskID, Fragment: "先采集再写周报"},
	))
	if err != nil || cycle.RouterIntents == nil || len(*cycle.RouterIntents) != 3 {
		t.Fatalf("cycle receipt: %+v, %v", cycle, err)
	}
	for _, item := range *cycle.RouterIntents {
		if item.Status != "clarify" || !strings.Contains(item.Detail, "循环") {
			t.Fatalf("cycle was partially applied: %+v", item)
		}
	}
	var edges int
	if err = s.db.QueryRow("SELECT count(*) FROM run_queue_order").Scan(&edges); err != nil || edges != 3 {
		t.Fatalf("conflicts changed the saved queue orders: count=%d, %v", edges, err)
	}
}
