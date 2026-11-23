package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkCommandPreviewParsing(t *testing.T) {
	for _, tc := range []struct {
		input, body string
		version     int
		composed    bool
		invalid     bool
	}{
		{"/benchmark plan@v4\n  只用健康节点\n不要 node-03  ", "  只用健康节点\n不要 node-03  ", 4, true, false},
		{"/benchmark plan@4 用健康节点", "用健康节点", 4, true, false},
		{"/benchmark plan@4", "", 0, false, false},
		{"/benchmark-status task anything", "", 0, false, false},
		{"/benchmark plan@bad\n使用健康节点", "使用健康节点", 0, true, true},
	} {
		ref, body, composed, err := ParseBenchmarkCommandPreview(tc.input)
		if body != tc.body || ref.Version != tc.version || composed != tc.composed || (err != nil) != tc.invalid {
			t.Errorf("parse %q = %+v, %q, %v, %v", tc.input, ref, body, composed, err)
		}
	}
}

func benchmarkCommandPreviewFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-preview.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := t.Context()
	profile := BenchmarkProfile{Key: "plan", Version: 4, Cases: []BenchmarkCase{{
		Key: "network", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, Parameters: map[string]string{"count": "4"},
		ActualConfig: map[string]string{"count": "4"}, RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}
	if err := s.PublishBenchmarkProfile(ctx, "user:session", profile); err != nil {
		t.Fatal(err)
	}
	grant := DomainBenchmarkGrant{Connection: "bench-test", Revision: 1, Tool: "test", Allowed: map[string][]string{"count": {"4"}}, ExpiresAt: now() + 120000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBenchmarkCommandPreviewPreservesRequirementsAndDoesNotDispatch(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	body := "  只跑 7 个健康节点，排除 node-03\n结束后核对结果  "
	text := "/benchmark plan@v4\n" + body
	material, err := saveMaterial(ctx, s.db, "requirements.txt", []byte("材料要求：不要占用生产集群"), nil)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	model := func(_ context.Context, in BenchmarkCommandPreviewModelInput, _ int) (string, error) {
		modelCalls++
		if in.Text != body || in.Profile != (BenchmarkProfileRef{"plan", 4}) || len(in.Cases) != 1 ||
			in.Cases[0].Parameters["count"] != "4" || len(in.Materials) != 1 || in.Materials[0].Content != "材料要求：不要占用生产集群" {
			t.Fatalf("incomplete model context: %+v", in)
		}
		return `{"advice":"先比对冻结目标与排除条件，再核对候选节点的健康状态。","checks":["targets","health","scale"]}`, nil
	}
	run, err := s.SubmitBenchmarkCommandPreview(ctx, "composed-1", "user:session", "", text, "", []string{material.ID}, nil, model)
	if err != nil || run.Status != "completed" || run.TaskID != "" || !strings.Contains(run.Response, "以上要求尚未应用") ||
		!strings.Contains(run.Response, "没有创建或派发任务") || !strings.Contains(run.Response, "排除 node-03") || !strings.Contains(run.Response, "当前材料") ||
		!strings.Contains(run.Response, "模型建议（待核对）：\n\n    先比对冻结目标与排除条件，再核对候选节点的健康状态。") {
		t.Fatalf("preview: %+v %v", run, err)
	}
	var savedText string
	var attached int
	if err := s.db.QueryRow(`SELECT m.text,(SELECT count(*) FROM message_materials WHERE message_seq=m.seq)
FROM messages m WHERE m.run_id=? AND m.role='user'`, run.ID).Scan(&savedText, &attached); err != nil || savedText != text || attached != 1 {
		t.Fatalf("stored input = %q, materials = %d, %v", savedText, attached, err)
	}
	again, err := s.SubmitBenchmarkCommandPreview(ctx, "composed-1", "user:session", "", text, "", []string{material.ID}, nil, model)
	if err != nil || again.ID != run.ID || modelCalls != 1 {
		t.Fatalf("retry = %+v, calls=%d, %v", again, modelCalls, err)
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "composed-1", "user:session", "", text+"变化", "", []string{material.ID}, nil, model); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request did not conflict: %v", err)
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "composed-1", "user:other", "", text, "", []string{material.ID}, nil, model); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign receipt did not conflict: %v", err)
	}
	for _, table := range []string{"tasks", "benchmark_runs", "operations", "benchmark_admission_previews"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("unexpected %s records: %d, %v", table, n, err)
		}
	}

	// Subsequent requests receive the ordinary conversation history and active
	// material snapshot, without turning the earlier preview into a task.
	followup := func(_ context.Context, in BenchmarkCommandPreviewModelInput, _ int) (string, error) {
		if len(in.History) < 2 || len(in.Materials) != 1 || in.Materials[0].Content != "材料要求：不要占用生产集群" {
			t.Fatalf("history/material missing: %+v", in)
		}
		return `{"checks":["other"]}`, nil
	}
	if next, err := s.SubmitBenchmarkCommandPreview(ctx, "composed-2", "user:session", run.ConversationID, "/benchmark plan@4 按之前的材料", "", nil, nil, followup); err != nil || next.TaskID != "" {
		t.Fatalf("follow-up = %+v, %v", next, err)
	}
}

func TestBenchmarkCommandPreviewRejectsMissingVersionPermissionsAndFailure(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	calls := 0
	model := func(context.Context, BenchmarkCommandPreviewModelInput, int) (string, error) {
		calls++
		return "", errors.New("provider unavailable")
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "missing-version", "user:session", "", "/benchmark plan@v3\n只用健康节点", "", nil, nil, model); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing version: %v", err)
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "denied-actor", "user:other", "", "/benchmark plan@v4\n只用健康节点", "", nil, nil, model); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("unauthorized actor: %v", err)
	}
	if calls != 0 {
		t.Fatal("model received unauthorized or missing plan")
	}
	text := "/benchmark plan@4\n只用健康节点"
	for range 2 {
		if _, err := s.SubmitBenchmarkCommandPreview(ctx, "model-fails", "user:session", "", text, "", nil, nil, model); !errors.Is(err, ErrClassification) {
			t.Fatalf("model failure: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("uncertain request repeated %d times", calls)
	}
	var tasks, inbox int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM inbox_events WHERE event_id='model-fails')").Scan(&tasks, &inbox); err != nil || tasks != 0 || inbox != 0 {
		t.Fatalf("failed preview created work: tasks=%d inbox=%d err=%v", tasks, inbox, err)
	}
}

func TestBenchmarkCommandPreviewRechecksAuthorizationAfterModel(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	model := func(_ context.Context, _ BenchmarkCommandPreviewModelInput, _ int) (string, error) {
		if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject='user:session'", now()); err != nil {
			t.Fatal(err)
		}
		return `{"checks":["targets"]}`, nil
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "revoked-after-model", "user:session", "", "/benchmark plan@v4\n仅使用健康节点", "", nil, nil, model); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked grant accepted: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&n); err != nil || n != 0 {
		t.Fatalf("created task after revocation: %d %v", n, err)
	}
}

func TestBenchmarkCommandPreviewDoesNotAlterConversationTask(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	taskRun, err := s.Submit(ctx, "existing-task", "准备报告", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// Finish the current reply while leaving its associated task in place.
	if _, err := s.db.Exec("UPDATE runs SET status='completed' WHERE id=?", taskRun.ID); err != nil {
		t.Fatal(err)
	}
	var beforeStatus string
	var beforeRevision int
	if err := s.db.QueryRow("SELECT status,revision FROM tasks WHERE id=?", taskRun.TaskID).Scan(&beforeStatus, &beforeRevision); err != nil {
		t.Fatal(err)
	}
	model := func(_ context.Context, input BenchmarkCommandPreviewModelInput, _ int) (string, error) {
		if input.Task != nil {
			t.Fatalf("unselected task was sent to model: %+v", input.Task)
		}
		return `{"checks":[]}`, nil
	}
	preview, err := s.SubmitBenchmarkCommandPreview(ctx, "standalone-preview", "user:session", taskRun.ConversationID, "/benchmark plan@4 评估目标", "", nil, nil, model)
	if err != nil || preview.TaskID != "" {
		t.Fatalf("preview attached to task: %+v %v", preview, err)
	}
	var afterStatus string
	var afterRevision, tasks int
	if err := s.db.QueryRow("SELECT status,revision,(SELECT count(*) FROM tasks) FROM tasks WHERE id=?", taskRun.TaskID).Scan(&afterStatus, &afterRevision, &tasks); err != nil ||
		afterStatus != beforeStatus || afterRevision != beforeRevision || tasks != 1 {
		t.Fatalf("task changed: %s/%d, before %s/%d, count %d: %v", afterStatus, afterRevision, beforeStatus, beforeRevision, tasks, err)
	}
}

func TestBenchmarkCommandPreviewRejectsUntrustedModelOutput(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	model := func(context.Context, BenchmarkCommandPreviewModelInput, int) (string, error) {
		return `{"checks":["targets"],"confirm":"execute"}`, nil
	}
	if _, err := s.SubmitBenchmarkCommandPreview(t.Context(), "unsafe-model-output", "user:session", "", "/benchmark plan@4 仅健康节点", "", nil, nil, model); !errors.Is(err, ErrClassification) {
		t.Fatalf("unexpected model output accepted: %v", err)
	}
	var tasks int
	if err := s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("unexpected model output created work: %d %v", tasks, err)
	}
}

func benchmarkCommandReferencedTaskFixture(t *testing.T, s *Store) (string, string) {
	t.Helper()
	task, err := s.SubmitBenchmarkRun(t.Context(), "user:session", BenchmarkRunRequest{"fixture-benchmark", "plan", 4}, &benchFixtureProvider{})
	if err != nil {
		t.Fatal(err)
	}
	var conversation string
	if err := s.db.QueryRow("SELECT conversation_id FROM tasks WHERE id=?", task).Scan(&conversation); err != nil {
		t.Fatal(err)
	}
	return task, conversation
}

func TestBenchmarkCommandPreviewUsesAndPersistsExplicitTaskReference(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	task, conversation := benchmarkCommandReferencedTaskFixture(t, s)
	other, err := s.Submit(ctx, "unrelated-task", "不相关任务机密", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	material, err := saveMaterial(ctx, s.db, "requirement.txt", []byte("本轮附件内容"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var beforeRevision int
	var beforeStatus, beforePhase, beforeTargets string
	if err := s.db.QueryRow(`SELECT t.revision,t.status,c.phase,c.frozen_targets FROM tasks t
JOIN benchmark_cases c ON c.task_id=t.id WHERE t.id=?`, task).
		Scan(&beforeRevision, &beforeStatus, &beforePhase, &beforeTargets); err != nil {
		t.Fatal(err)
	}
	ref := &MessageTaskRef{TaskID: task}
	text := "/benchmark plan@v4\n参照所选任务，排除 node-03。文字提到 " + other.TaskID
	calls := 0
	model := func(_ context.Context, input BenchmarkCommandPreviewModelInput, _ int) (string, error) {
		calls++
		if input.Task == nil || input.Task.ID != task || input.Task.Benchmark == nil ||
			input.Task.Benchmark.Profile != (BenchmarkProfileRef{"plan", 4}) || input.Task.Benchmark.PlanRevision != 1 ||
			len(input.Task.Benchmark.Cases) != 1 || input.Task.Benchmark.Cases[0].Key != "network" ||
			len(input.Task.Benchmark.Cases[0].Targets) != 1 || input.Task.Benchmark.Cases[0].Targets[0] != "node-1" ||
			len(input.Materials) != 1 || input.Materials[0].Content != "本轮附件内容" {
			t.Fatalf("selected task or material missing: %+v", input)
		}
		data, err := json.Marshal(input)
		if err != nil || strings.Contains(string(data), "不相关任务机密") || strings.Contains(string(data), "authorization_refs") {
			t.Fatalf("unrelated task data or authorization records sent: %s, %v", data, err)
		}
		return `{"checks":["targets"]}`, nil
	}
	run, err := s.SubmitBenchmarkCommandPreview(ctx, "ref-preview", "user:session", conversation, text, "", []string{material.ID}, ref, model)
	if err != nil || run.TaskID != "" || run.Status != "completed" {
		t.Fatalf("referenced preview: %+v %v", run, err)
	}
	again, err := s.SubmitBenchmarkCommandPreview(ctx, "ref-preview", "user:session", conversation, text, "", []string{material.ID}, ref, model)
	if err != nil || again.ID != run.ID || calls != 1 {
		t.Fatalf("same request did not replay: %+v, calls=%d, %v", again, calls, err)
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "ref-preview", "user:session", conversation, text, "", []string{material.ID}, &MessageTaskRef{TaskID: other.TaskID}, model); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key accepted a different task: %v", err)
	}
	if _, err := s.SubmitBenchmarkCommandPreview(ctx, "ref-preview", "user:session", conversation, text, "", []string{material.ID}, nil, model); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key accepted a removed reference: %v", err)
	}
	page, err := s.MessageHistory(ctx, conversation, 0, 0, 0)
	if err != nil || len(page.Items) != 2 || page.Items[0].Text != text || page.Items[0].TaskRef == nil ||
		page.Items[0].TaskRef.TaskID != task || len(page.Items[0].Materials) != 1 || page.Items[1].TaskRef != nil {
		t.Fatalf("reference or material not saved on this message: %+v %v", page, err)
	}
	var afterRevision, tasks, operations int
	var afterStatus, afterPhase, afterTargets string
	if err := s.db.QueryRow(`SELECT t.revision,t.status,c.phase,c.frozen_targets,
(SELECT count(*) FROM tasks),(SELECT count(*) FROM operations) FROM tasks t
JOIN benchmark_cases c ON c.task_id=t.id WHERE t.id=?`, task).
		Scan(&afterRevision, &afterStatus, &afterPhase, &afterTargets, &tasks, &operations); err != nil ||
		afterRevision != beforeRevision || afterStatus != beforeStatus || afterPhase != beforePhase ||
		afterTargets != beforeTargets || tasks != 2 || operations != 0 {
		t.Fatalf("preview mutated tasks or dispatched work: rev=%d status=%s phase=%s targets=%s tasks=%d operations=%d %v",
			afterRevision, afterStatus, afterPhase, afterTargets, tasks, operations, err)
	}
}

func TestBenchmarkCommandPreviewReferenceMustBeVisibleBeforeModel(t *testing.T) {
	s := benchmarkCommandPreviewFixture(t)
	ctx := t.Context()
	task, conversation := benchmarkCommandReferencedTaskFixture(t, s)
	other, err := s.Submit(ctx, "other-conversation", "其他任务", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE runs SET status='completed' WHERE id=?", other.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	model := func(context.Context, BenchmarkCommandPreviewModelInput, int) (string, error) {
		calls++
		return `{"checks":[]}`, nil
	}
	for _, tc := range []struct{ key, actor, conversation, task string }{
		{"no-conversation", "user:session", "", task},
		{"cross-conversation", "user:session", other.ConversationID, task},
		{"foreign-actor", "user:other", conversation, task},
	} {
		_, err := s.SubmitBenchmarkCommandPreview(ctx, tc.key, tc.actor, tc.conversation, "/benchmark plan@4 核对任务", "", nil, &MessageTaskRef{TaskID: tc.task}, model)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s was not rejected: %v", tc.key, err)
		}
	}
	if calls != 0 {
		t.Fatalf("unauthorized reference reached model %d times", calls)
	}
}

func TestBenchmarkCommandPreviewRechecksReferencedTaskAfterModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *Store, string, string)
		want error
	}{
		{"task_content", func(t *testing.T, s *Store, task, _ string) {
			if _, err := s.db.Exec("UPDATE tasks SET goal='新要求',revision=revision+1 WHERE id=?", task); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskRevision},
		{"frozen_scope", func(t *testing.T, s *Store, task, _ string) {
			if _, err := s.db.Exec(`UPDATE benchmark_cases SET frozen_targets='[{"ref":"node-new","revision":1,"epoch":1}]' WHERE task_id=?`, task); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskRevision},
		{"revoked_visibility", func(t *testing.T, s *Store, task, _ string) {
			if _, err := s.db.Exec("UPDATE benchmark_runs SET actor='user:other' WHERE task_id=?", task); err != nil {
				t.Fatal(err)
			}
		}, sql.ErrNoRows},
		{"archived_conversation", func(t *testing.T, s *Store, _, conversation string) {
			if _, err := s.db.Exec("UPDATE conversations SET archived=1 WHERE id=?", conversation); err != nil {
				t.Fatal(err)
			}
		}, sql.ErrNoRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := benchmarkCommandPreviewFixture(t)
			task, conversation := benchmarkCommandReferencedTaskFixture(t, s)
			ref := &MessageTaskRef{TaskID: task}
			calls := 0
			model := func(context.Context, BenchmarkCommandPreviewModelInput, int) (string, error) {
				calls++
				tc.edit(t, s, task, conversation)
				return `{"checks":["targets"]}`, nil
			}
			_, err := s.SubmitBenchmarkCommandPreview(t.Context(), "reference-stale", "user:session", conversation, "/benchmark plan@4 核对任务", "", nil, ref, model)
			if !errors.Is(err, tc.want) {
				t.Fatalf("changed reference accepted: %v", err)
			}
			var submissions, refs, operations int
			if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM submissions WHERE request_key='reference-stale'),
(SELECT count(*) FROM message_task_refs),(SELECT count(*) FROM operations)`).Scan(&submissions, &refs, &operations); err != nil || submissions != 0 || refs != 0 || operations != 0 {
				t.Fatalf("stale preview persisted or dispatched: %d/%d/%d %v", submissions, refs, operations, err)
			}
			if tc.want == ErrTaskRevision {
				_, err = s.SubmitBenchmarkCommandPreview(t.Context(), "reference-stale", "user:session", conversation, "/benchmark plan@4 核对任务", "", nil, ref, model)
				if !errors.Is(err, ErrTaskRevision) || calls != 1 {
					t.Fatalf("stale result was reused or model repeated: calls=%d %v", calls, err)
				}
			}
		})
	}
}
