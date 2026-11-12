package work

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRouterIncludesExplicitOlderTask(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	target, err := s.Submit(ctx, "router-older-target", "整理早期周报", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, err := s.Submit(ctx, fmt.Sprintf("router-recent-%d", i), "整理近期周报", "", "", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("UPDATE tasks SET updated_at=0 WHERE id=?", target.TaskID); err != nil {
		t.Fatal(err)
	}
	progress := "查询 /tasks/" + target.TaskID + " 的进度"
	router := batchRouter(
		RouteDecision{Kind: "progress", TaskID: target.TaskID, Fragment: progress},
		RouteDecision{Kind: "new_task", Goal: "汇总本周事项", Fragment: "汇总本周事项"},
	)
	receipt, err := s.SubmitRoutedChat(ctx, "router-explicit-old", progress+"；汇总本周事项", "", "", Delivery{}, router)
	if err != nil || receipt.RouterIntents == nil || (*receipt.RouterIntents)[0].Status != "applied" ||
		(*receipt.RouterIntents)[0].TaskID != target.TaskID || (*receipt.RouterIntents)[1].Status != "applied" {
		t.Fatalf("explicit older task was lost from the batch: %+v, %v", receipt, err)
	}
}

func batchRouter(decisions ...RouteDecision) Router {
	return func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		return DelegationResult{Route: &RouteDecision{Kind: "batch", Intents: append([]RouteDecision(nil), decisions...)}}, nil
	}
}

func TestRouterUpdatesGoalsAndPrioritiesIndependently(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	submit := func(key, goal string) Run {
		t.Helper()
		run, err := s.Submit(ctx, key, goal, "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	a := submit("router-revise-a", "整理周报")
	b := submit("router-revise-b", "整理会议记录")
	c := submit("router-revise-c", "整理预算")
	d := submit("router-revise-d", "整理资料")
	const newGoal = "汇总本月项目风险"
	text := "周报改成汇总本月项目风险；会议记录设为高；预算设为高；预算设为低；资料设为低；把那个事项改成整理测试记录"
	route := batchRouter(
		RouteDecision{Kind: "set_goal", TaskID: a.TaskID, Goal: newGoal, Fragment: "周报改成汇总本月项目风险"},
		RouteDecision{Kind: "set_priority", TaskID: b.TaskID, Priority: "high", Fragment: "会议记录设为高"},
		RouteDecision{Kind: "set_priority", TaskID: c.TaskID, Priority: "high", Fragment: "预算设为高"},
		RouteDecision{Kind: "set_priority", TaskID: c.TaskID, Priority: "low", Fragment: "预算设为低"},
		RouteDecision{Kind: "set_priority", TaskID: d.TaskID, Priority: "low", Fragment: "资料设为低"},
		RouteDecision{Kind: "set_goal", TaskID: "unknown", Goal: "整理测试记录", Fragment: "把那个事项改成整理测试记录"},
	)
	send := func() (Run, error) {
		return s.SubmitRoutedChat(ctx, "router-revise-batch", text, "", "", Delivery{}, route)
	}
	first, err := send()
	if err != nil || first.RouterIntents == nil || len(*first.RouterIntents) != 6 {
		t.Fatalf("batch receipt: %+v, %v", first, err)
	}
	for i, want := range []string{"applied", "applied", "clarify", "clarify", "applied", "clarify"} {
		if (*first.RouterIntents)[i].Status != want {
			t.Fatalf("intent %d: %+v", i, (*first.RouterIntents)[i])
		}
	}
	if retry, err := send(); err != nil || retry.ID != first.ID || !reflect.DeepEqual(retry.RouterIntents, first.RouterIntents) {
		t.Fatalf("retry changed receipt: %+v, %v", retry, err)
	}
	conflict, err := s.SubmitRoutedChat(ctx, "router-conflicting-goals", "预算改成核对发票；预算改成统计工时；预算改成核对发票",
		"", first.ConversationID, Delivery{}, batchRouter(
			RouteDecision{Kind: "set_goal", TaskID: c.TaskID, Goal: "核对发票", Fragment: "预算改成核对发票"},
			RouteDecision{Kind: "set_goal", TaskID: c.TaskID, Goal: "统计工时", Fragment: "预算改成统计工时"},
			RouteDecision{Kind: "set_goal", TaskID: c.TaskID, Goal: "核对发票", Fragment: "预算改成核对发票"},
		))
	if err != nil || conflict.RouterIntents == nil {
		t.Fatalf("goal conflict receipt: %+v, %v", conflict, err)
	}
	goals := *conflict.RouterIntents
	if len(goals) != 3 || goals[0].Status != "clarify" || goals[1].Status != "clarify" ||
		goals[2].Status != "duplicate" || goals[2].DuplicateOf != goals[0].ID {
		t.Fatalf("conflicting and repeated goals were not separated: %+v", goals)
	}
	unchanged, err := s.Detail(c.TaskID, true)
	if err != nil || unchanged.Task.Goal != "整理预算" || unchanged.Task.Priority != "normal" {
		t.Fatalf("conflict changed the task: %+v, %v", unchanged, err)
	}
	edited, err := s.Detail(a.TaskID, true)
	if err != nil || edited.Task.Goal != newGoal || !edited.Task.RequirementsChanged || edited.Run.ID != a.ID || edited.Run.Status != "interrupted" {
		t.Fatalf("queued task still used old goal: %+v, %v", edited, err)
	}
	var contract string
	if err := s.db.QueryRow("SELECT goal FROM task_revisions WHERE task_id=? AND revision=?", a.TaskID, edited.Task.Revision).Scan(&contract); err != nil || contract != newGoal {
		t.Fatalf("new contract not saved: %q, %v", contract, err)
	}
	if resumed, err := s.Resume(ctx, a.TaskID, "router-resume-revised", edited.Task.Revision); err != nil || resumed.ID == a.ID {
		t.Fatalf("revised goal did not get a fresh run: %+v, %v", resumed, err)
	}
	for _, want := range []string{b.TaskID, c.TaskID, a.TaskID, d.TaskID} {
		run, err := s.claim(ctx)
		if err != nil || run.TaskID != want {
			t.Fatalf("priority order selected %s, want %s: %v", run.TaskID, want, err)
		}
	}
}

func TestRouterKeepsRunningContractWhenChangingPriority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	created, err := s.Submit(ctx, "router-running-revise", "整理周报", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil || run.ID != created.ID {
		t.Fatalf("claim: %+v, %v", run, err)
	}
	const goal = "汇总本月风险"
	route := batchRouter(
		RouteDecision{Kind: "set_goal", TaskID: run.TaskID, Goal: goal, Fragment: "改成汇总本月风险"},
		RouteDecision{Kind: "set_priority", TaskID: run.TaskID, Priority: "high", Fragment: "设为高优先级"},
	)
	receipt, err := s.SubmitRoutedChat(ctx, "router-running-edits", "周报改成汇总本月风险；设为高优先级", "", "", Delivery{}, route)
	if err != nil || receipt.RouterIntents == nil || (*receipt.RouterIntents)[0].Status != "clarify" || (*receipt.RouterIntents)[1].Status != "applied" {
		t.Fatalf("running goal blocked independent priority: %+v, %v", receipt, err)
	}
	detail, err := s.Detail(run.TaskID, true)
	if err != nil || detail.Task.Goal != "整理周报" || detail.Task.Priority != "high" {
		t.Fatalf("wrong task contract: %+v, %v", detail, err)
	}
	if err := RecordModelManifest(s.withManifestRecorder(ctx, run), ManifestRequest{}); err != nil {
		t.Fatalf("priority invalidated active run binding: %v", err)
	}
	if err := s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		return DelegationResult{Route: &RouteDecision{Kind: "set_goal", TaskID: run.TaskID, Goal: goal, ResolveID: (*receipt.RouterIntents)[0].ID}}, nil
	}
	revised, err := s.SubmitRoutedChat(ctx, "router-confirm-goal", "确认采用刚才的新目标", "", receipt.ConversationID, Delivery{}, resolve)
	if err != nil || revised.RouterIntents == nil || (*revised.RouterIntents)[0].Status != "applied" {
		t.Fatalf("goal clarification did not apply the original wording: %+v, %v", revised, err)
	}
	detail, err = s.Detail(run.TaskID, true)
	if err != nil || detail.Task.Goal != goal || detail.Task.Priority != "high" || !detail.Task.RequirementsChanged {
		t.Fatalf("clarification overwrote the wrong fields: %+v, %v", detail, err)
	}
}

func TestRouterBatchDispatchAndClarificationSurviveRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	target, err := s.Submit(ctx, "router-target", "整理会议记录", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	input := "写周报；暂停会议记录；继续会议记录；看看那项进度"
	router := batchRouter(
		RouteDecision{Kind: "new_task", Goal: "写周报", Fragment: "写周报"},
		RouteDecision{Kind: "pause", TaskID: target.TaskID, Fragment: "暂停会议记录"},
		RouteDecision{Kind: "resume", TaskID: target.TaskID, Fragment: "继续会议记录"},
		RouteDecision{Kind: "progress", TaskID: "unknown", Fragment: "看看那项进度"},
		RouteDecision{Kind: "new_task", Goal: "写周报", Fragment: "写周报"},
	)
	calls := 0
	counted := func(ctx context.Context, text string, history []Message, candidates []RouteCandidate, limit int) (DelegationResult, error) {
		calls++
		return router(ctx, text, history, candidates, limit)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_router_intents BEFORE INSERT ON router_intents
BEGIN SELECT RAISE(ABORT,'router receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	submit := func() (Run, error) {
		return s.SubmitRoutedChat(ctx, "router-mixed", input, "", "", Delivery{}, counted)
	}
	if _, err = submit(); err == nil {
		t.Fatal("injected failure did not abort dispatch")
	}
	var taskCount int
	if err = s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("failed receipt leaked task: %d, %v", taskCount, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_router_intents"); err != nil {
		t.Fatal(err)
	}
	first, err := submit()
	if err != nil || first.TaskID != "" || first.RouterIntents == nil || len(*first.RouterIntents) != 5 {
		t.Fatalf("batch receipt: %+v, %v", first, err)
	}
	received := *first.RouterIntents
	for index, want := range []string{"applied", "clarify", "clarify", "clarify", "duplicate"} {
		if received[index].Status != want {
			t.Fatalf("intent %d: %+v", index, received[index])
		}
	}
	if received[4].DuplicateOf != received[0].ID || !strings.Contains(first.Response, "/tasks/"+received[0].TaskID) {
		t.Fatalf("missing duplicate relationship or task link: %+v", received)
	}
	retry, err := submit()
	if err != nil || retry.ID != first.ID || !reflect.DeepEqual(first.RouterIntents, retry.RouterIntents) || calls != 1 {
		t.Fatalf("retry changed receipt or reclassified: %+v, calls=%d, %v", retry, calls, err)
	}
	task, err := s.Detail(received[0].TaskID, true)
	if err != nil || len(task.Messages) != 1 || task.Messages[0].Text != "写周报" {
		t.Fatalf("worker received another intent's text: %+v, %v", task, err)
	}
	targetBefore, err := s.Detail(target.TaskID, true)
	if err != nil || targetBefore.Run.Status != "queued" {
		t.Fatalf("conflicting control changed target: %+v, %v", targetBefore, err)
	}
	answer := func(ctx context.Context, _ string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
		pending := RouterPending(ctx)
		if len(pending) != 3 {
			t.Fatalf("pending questions not supplied: %+v", pending)
		}
		return DelegationResult{Route: &RouteDecision{Kind: "pause", TaskID: target.TaskID, ResolveID: received[1].ID}}, nil
	}
	resolved, err := s.SubmitRoutedChat(ctx, "router-answer", "只暂停会议记录", "", first.ConversationID, Delivery{}, answer)
	if err != nil || resolved.RouterIntents == nil || (*resolved.RouterIntents)[0].Status != "applied" {
		t.Fatalf("clarification not applied: %+v, %v", resolved, err)
	}
	record, err := s.RunRecord(ctx, first.ID, "")
	if err != nil || record.RouterIntents == nil || (*record.RouterIntents)[1].ResolvedBy != (*resolved.RouterIntents)[0].ID ||
		(*record.RouterIntents)[2].ResolvedBy != "" {
		t.Fatalf("clarification affected the wrong items: %+v, %v", record.RouterIntents, err)
	}
}

func TestRouterWorkflowConflictDoesNotBlockUnrelatedWork(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	graph, err := s.CreateWorkflow(ctx, "router-workflow", WorkflowRequest{
		Goal: "整理资料", Criteria: "列出已核对成果", Steps: []WorkflowStep{
			{Key: "collect", Goal: "采集资料", Criteria: "列来源"},
			{Key: "review", Goal: "核对资料", Criteria: "列差异", Depends: []string{"collect"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.SubmitRoutedChat(ctx, "router-cross-task", "暂停采集，继续核对，再拟一份提纲", "", "", Delivery{}, batchRouter(
		RouteDecision{Kind: "pause", TaskID: graph.Children[0].TaskID, Fragment: "暂停采集"},
		RouteDecision{Kind: "follow_up", TaskID: graph.Children[1].TaskID, Fragment: "继续核对"},
		RouteDecision{Kind: "new_task", Goal: "拟提纲", Fragment: "拟一份提纲"},
	))
	if err != nil || run.RouterIntents == nil {
		t.Fatalf("route batch: %+v, %v", run, err)
	}
	items := *run.RouterIntents
	if items[0].Status != "clarify" || items[1].Status != "clarify" || items[2].Status != "applied" ||
		!strings.Contains(items[0].Detail, "任务依赖") {
		t.Fatalf("incorrect coordination: %+v", items)
	}
	wrongChat, err := s.SubmitRoutedChat(ctx, "router-wrong-chat", "确认暂停采集", "", "", Delivery{},
		func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
			return DelegationResult{Route: &RouteDecision{Kind: "pause", TaskID: graph.Children[0].TaskID, ResolveID: items[0].ID}}, nil
		})
	if err != nil || wrongChat.RouterIntents == nil || (*wrongChat.RouterIntents)[0].Status != "clarify" {
		t.Fatalf("another chat resolved the intent: %+v, %v", wrongChat, err)
	}
	unchanged, err := s.Detail(graph.Children[0].TaskID, true)
	if err != nil || unchanged.Run.Status != "queued" {
		t.Fatalf("conflict changed prerequisite: %+v, %v", unchanged, err)
	}
	if _, err := s.SubmitRoutedChat(ctx, "router-cross-task", "不同内容", "", "", Delivery{}, batchRouter()); !errors.Is(err, ErrConflict) {
		t.Fatalf("request identity accepted different content: %v", err)
	}
}

func TestRouterQueuedIntentLinksToItsOwnRun(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	target, err := s.Submit(ctx, "router-followup-target", "整理报告", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.SubmitRoutedChat(ctx, "router-deferred", "补上周五数据，再写摘要", "", "", Delivery{}, batchRouter(
		RouteDecision{Kind: "follow_up", TaskID: target.TaskID, Fragment: "补上周五数据"},
		RouteDecision{Kind: "new_task", Goal: "写摘要", Fragment: "写摘要"},
	))
	if err != nil || source.RouterIntents == nil || (*source.RouterIntents)[0].Status != "queued" || (*source.RouterIntents)[0].RunID != "" {
		t.Fatalf("follow-up linked to active run: %+v, %v", source, err)
	}
	if err = s.finish(active, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Detail(target.TaskID, true)
	if err != nil || detail.Run.ID == active.ID || detail.Messages[len(detail.Messages)-1].Text != "补上周五数据" {
		t.Fatalf("wrong dispatched follow-up: %+v, %v", detail, err)
	}
	record, err := s.RunRecord(ctx, source.ID, "")
	if err != nil || record.RouterIntents == nil || (*record.RouterIntents)[0].RunID != detail.Run.ID {
		t.Fatalf("receipt did not follow its dispatch: %+v, %v", record.RouterIntents, err)
	}
}

func TestRouterBatchRetainsIndependentIntentWhenTargetChanges(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	target, err := s.Submit(ctx, "router-stale-target", "整理财报", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	route := func(ctx context.Context, _ string, _ []Message, candidates []RouteCandidate, _ int) (DelegationResult, error) {
		if len(candidates) != 1 {
			t.Fatalf("candidates: %+v", candidates)
		}
		if err := s.Control(ctx, target.TaskID, "pause", candidates[0].Revision); err != nil {
			return DelegationResult{}, err
		}
		return DelegationResult{Route: &RouteDecision{Kind: "batch", Intents: []RouteDecision{
			{Kind: "stop", TaskID: target.TaskID, Fragment: "停止整理财报"},
			{Kind: "new_task", Goal: "写周报", Fragment: "写周报"},
		}}}, nil
	}
	receipt, err := s.SubmitRoutedChat(ctx, "router-stale-batch", "停止整理财报，另外写周报", "", "", Delivery{}, route)
	if err != nil || receipt.RouterIntents == nil {
		t.Fatalf("batch failed: %+v, %v", receipt, err)
	}
	items := *receipt.RouterIntents
	if items[0].Status != "clarify" || !strings.Contains(items[0].Detail, "状态已变化") || items[1].Status != "applied" {
		t.Fatalf("stale control blocked unrelated work: %+v", items)
	}
	d, err := s.Detail(target.TaskID, true)
	if err != nil || d.Task.StatusReason != "paused" {
		t.Fatalf("stale stop was applied: %+v, %v", d, err)
	}
}
