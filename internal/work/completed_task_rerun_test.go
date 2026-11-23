package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func completedRerunFixture(t *testing.T) (*Store, Run, Detail, Material) {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspace := filepath.Join(root, "workspace")
	if err = store.OpenWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspace, "materials", "资料.md"), []byte("已提交的资料"), 0600); err != nil {
		t.Fatal(err)
	}
	material, err := store.CaptureMaterial(ctx, "materials/资料.md")
	if err != nil {
		t.Fatal(err)
	}
	criteria := "列出关键结论"
	run, err := store.SubmitWithDelivery(ctx, "original-completed", "根据资料整理报告", "工作资料", "", true, Delivery{Criteria: &criteria}, material.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim original: %+v, %v", claimed, err)
	}
	if err = store.delta(ctx, run.ID, "原成果"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(claimed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || detail.Artifact == nil {
		t.Fatalf("original result: %+v, %v", detail, err)
	}
	if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil || detail.Acceptance == nil {
		t.Fatalf("original acceptance: %+v, %v", detail, err)
	}
	return store, run, detail, material
}

func TestCompletedTaskRerunPreservesIdentityInputAndAcceptance(t *testing.T) {
	ctx := t.Context()
	store, original, accepted, material := completedRerunFixture(t)
	if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); err != nil {
		t.Fatalf("ordinary completed task should be eligible: %v", err)
	}
	if receipt, err := store.ReadCompletedTaskRerun(ctx, original.TaskID, "repeat-request", accepted.Task.Revision); err != nil || receipt.ID != "" {
		t.Fatalf("unsubmitted request returned a receipt: %+v %v", receipt, err)
	}
	if _, err := store.RerunCompletedTask(ctx, original.TaskID, "stale-rerun", accepted.Task.Revision-1); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale revision: %v", err)
	}
	run, err := store.RerunCompletedTask(ctx, original.TaskID, "repeat-request", accepted.Task.Revision)
	if err != nil || run.ID == original.ID || run.TaskID != original.TaskID || run.ConversationID != original.ConversationID || run.Status != "queued" {
		t.Fatalf("queue rerun: %+v, %v", run, err)
	}
	replayed, err := store.RerunCompletedTask(ctx, original.TaskID, "repeat-request", accepted.Task.Revision)
	if err != nil || replayed.ID != run.ID {
		t.Fatalf("same request did not replay: %+v, %v", replayed, err)
	}
	if receipt, err := store.ReadCompletedTaskRerun(ctx, original.TaskID, "repeat-request", accepted.Task.Revision); err != nil || receipt.ID != run.ID {
		t.Fatalf("read submitted receipt: %+v %v", receipt, err)
	}
	if _, err := store.ReadCompletedTaskRerun(ctx, original.TaskID, "repeat-request", accepted.Task.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("read receipt with changed revision: %v", err)
	}
	if _, err = store.RerunCompletedTask(ctx, original.TaskID, "repeat-request", accepted.Task.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key and different revision: %v", err)
	}
	if _, err = store.RerunCompletedTask(ctx, original.TaskID, "second-request", accepted.Task.Revision); !errors.Is(err, ErrState) {
		t.Fatalf("parallel rerun accepted: %v", err)
	}
	detail, err := store.Detail(original.TaskID, true)
	if err != nil || detail.Task.Status != "active" || detail.Task.StatusReason != "queued" || detail.Task.CompletedAt != nil || detail.Acceptance != nil {
		t.Fatalf("queued lifecycle: %+v, %v", detail, err)
	}
	last := detail.Messages[len(detail.Messages)-1]
	if last.Text != accepted.Task.Goal || len(last.Materials) != 1 || last.Materials[0].ID != material.ID {
		t.Fatalf("rerun lost its original input: %+v", last)
	}
	oldArtifact, err := store.Artifact(accepted.Artifact.ID)
	if err != nil || oldArtifact.Acceptance == nil || oldArtifact.Acceptance.Digest != accepted.Acceptance.Digest {
		t.Fatalf("old acceptance lost: %+v, %v", oldArtifact, err)
	}
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim rerun: %+v, %v", claimed, err)
	}
	modelCalled := false
	store.execute(ctx, testProvider(func(_ context.Context, messages []Message, task bool, _ int, emit func(string) error) (*TokenUsage, error) {
		modelCalled = true
		last := messages[len(messages)-1]
		if !task || !strings.Contains(last.Text, accepted.Task.Goal) || !strings.Contains(last.Text, accepted.Task.Criteria) {
			t.Errorf("wrong task input: %+v", last)
		}
		return nil, emit("重新生成的成果")
	}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	newDetail, err := store.Detail(original.TaskID, true)
	if err != nil || !modelCalled || newDetail.Task.Status != "waiting_user" || newDetail.Artifact == nil || newDetail.Artifact.ID == accepted.Artifact.ID || newDetail.Acceptance != nil {
		t.Fatalf("new result should wait for acceptance: %+v, %v", newDetail, err)
	}
	if err = store.Accept(ctx, original.TaskID, accepted.Artifact.ID, newDetail.Task.Revision); !errors.Is(err, ErrState) {
		t.Fatalf("old result accepted for new run: %v", err)
	}
	if err = store.Accept(ctx, original.TaskID, newDetail.Artifact.ID, newDetail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	final, err := store.Detail(original.TaskID, true)
	if err != nil || final.Task.Status != "completed" || final.Acceptance == nil || final.Acceptance.ArtifactID != newDetail.Artifact.ID {
		t.Fatalf("new acceptance: %+v, %v", final, err)
	}
	var runs, acceptances int
	if err = store.db.QueryRow("SELECT (SELECT count(*) FROM runs WHERE task_id=?),(SELECT count(*) FROM task_acceptances WHERE task_id=?)", original.TaskID, original.TaskID).Scan(&runs, &acceptances); err != nil || runs != 2 || acceptances != 2 {
		t.Fatalf("history counts: runs=%d acceptances=%d err=%v", runs, acceptances, err)
	}
}

func TestCompletedTaskRerunConcurrentRequests(t *testing.T) {
	store, original, accepted, _ := completedRerunFixture(t)
	type outcome struct {
		run Run
		err error
	}
	out := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"concurrent-one", "concurrent-two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			run, err := store.RerunCompletedTask(context.Background(), original.TaskID, key, accepted.Task.Revision)
			out <- outcome{run, err}
		}(key)
	}
	wg.Wait()
	close(out)
	successes, conflicts := 0, 0
	for result := range out {
		switch {
		case result.err == nil:
			successes++
		case errors.Is(result.err, ErrState), errors.Is(result.err, ErrTaskRevision), errors.Is(result.err, ErrActive):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: %+v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent submissions: successes=%d conflicts=%d", successes, conflicts)
	}
}

type rerunToolTrapProvider struct {
	plain, automatic int
	check            func([]Message)
}

func (p *rerunToolTrapProvider) Stream(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
	p.plain++
	if p.check != nil {
		p.check(input)
	}
	return nil, emit("安全的重新生成")
}
func (p *rerunToolTrapProvider) TextRunProvider(context.Context, bool) (Provider, string, error) {
	return p, "rerun-no-auto-tool-fixture", nil
}
func (p *rerunToolTrapProvider) TaskToolTurn(context.Context, []Message, []TaskToolDefinition, []TaskReadRound, int, func(string) error) (TaskToolTurn, error) {
	p.automatic++
	return TaskToolTurn{}, errors.New("rerun must not offer tools")
}

func TestCompletedTaskRerunDoesNotOfferAutomaticToolsAfterContinue(t *testing.T) {
	ctx := t.Context()
	store, original, accepted, sourceMaterial := completedRerunFixture(t)
	run, err := store.RerunCompletedTask(ctx, original.TaskID, "rerun-then-continue", accepted.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim rerun: %+v %v", claimed, err)
	}
	store.execute(ctx, testProvider(func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
		return nil, errors.New("fixture interruption")
	}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	detail, err := store.Detail(original.TaskID, true)
	if err != nil || detail.Task.Status != "interrupted" {
		t.Fatalf("interrupted rerun: %+v %v", detail, err)
	}
	independent, err := saveMaterial(ctx, store.db, "materials/later.md", []byte("继续前的独立材料"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitChat(ctx, "continue-separate-chat", "user:session", original.ConversationID, "继续前的独立内容", "", false,
		Delivery{TaskRef: &MessageTaskRef{TaskID: original.TaskID}}, []string{independent.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err = store.ProcessChatAdmissions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	standalone, err := store.claim(ctx)
	if err != nil || standalone.TaskID != "" {
		t.Fatalf("claim independent chat: %+v %v", standalone, err)
	}
	if err = store.delta(ctx, standalone.ID, "独立回复"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(standalone, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	continued, err := store.Resume(ctx, original.TaskID, "continue-rerun", detail.Task.Revision)
	if err != nil || continued.ID == run.ID {
		t.Fatalf("fresh continuation: %+v %v", continued, err)
	}
	claimed, err = store.claim(ctx)
	if err != nil || claimed.ID != continued.ID {
		t.Fatalf("claim continuation: %+v %v", claimed, err)
	}
	provider := &rerunToolTrapProvider{check: func(input []Message) {
		hasSource := false
		for _, m := range input {
			if strings.Contains(m.Text, "继续前的独立内容") || strings.Contains(m.Text, "独立回复") {
				t.Errorf("independent message entered continuation: %+v", m)
			}
			for _, material := range m.Materials {
				if material.ID == independent.ID && material.Content != "" {
					t.Errorf("independent material entered continuation: %+v", material)
				}
				if material.ID == sourceMaterial.ID && material.Content == "已提交的资料" {
					hasSource = true
				}
			}
		}
		if !hasSource {
			t.Errorf("source material missing after continuation: %+v", input)
		}
	}}
	store.execute(ctx, provider, claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	detail, err = store.Detail(original.TaskID, true)
	if err != nil || detail.Task.Status != "waiting_user" || provider.plain != 1 || provider.automatic != 0 {
		t.Fatalf("automatic tool offer on continuation: plain=%d automatic=%d detail=%+v err=%v", provider.plain, provider.automatic, detail, err)
	}
}

type completedAutomaticReadProvider struct{ calls int }

func (*completedAutomaticReadProvider) Stream(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
	return nil, errors.New("expected automatic provider")
}
func (p *completedAutomaticReadProvider) TextRunProvider(context.Context, bool) (Provider, string, error) {
	return p, "completed-automatic-read-fixture", nil
}
func (p *completedAutomaticReadProvider) TaskToolTurn(_ context.Context, _ []Message, _ []TaskToolDefinition, _ []TaskReadRound, _ int, emit func(string) error) (TaskToolTurn, error) {
	p.calls++
	if p.calls == 1 {
		used := int64(1)
		return TaskToolTurn{TaskAnswer: TaskAnswer{Usage: &TokenUsage{OutputTokens: &used}}, Output: json.RawMessage(`[]`), Calls: []TaskToolCall{{Name: "workspace_list", ID: "list-1", Arguments: map[string]any{"query": ""}}}}, nil
	}
	return TaskToolTurn{}, emit("已读取并整理")
}

func TestCompletedTaskRerunRejectsAutomaticReadHistory(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.OpenWorkspace(filepath.Join(root, "workspace")); err != nil {
		t.Fatal(err)
	}
	run, err := store.Submit(ctx, "auto-read-original", "整理工作区", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	provider := &completedAutomaticReadProvider{}
	store.execute(ctx, provider, claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || detail.Task.Status != "waiting_user" || provider.calls != 2 || detail.Artifact == nil {
		t.Fatalf("automatic read fixture: %+v calls=%d err=%v", detail, provider.calls, err)
	}
	if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CheckCompletedTaskRerun(ctx, run.TaskID); !errors.Is(err, ErrCompletedTaskRerunUnsupported) {
		t.Fatalf("automatic read exposed as rerunnable: %v", err)
	}
	if _, err = store.RerunCompletedTask(ctx, run.TaskID, "auto-read-rerun", detail.Task.Revision); !errors.Is(err, ErrCompletedTaskRerunUnsupported) {
		t.Fatalf("automatic read was rerun: %v", err)
	}
}

func completedManualReadFixture(t *testing.T) (*Store, TaskReadRequest, Run, Detail) {
	t.Helper()
	ctx := t.Context()
	store, read := taskReadFixture(t)
	run, err := store.SubmitWithDelivery(ctx, "completed-manual-read", "整理样例状态", "", "", true, Delivery{Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.runTaskRead(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, run.ID, "已完成读取"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(claimed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || detail.Artifact == nil {
		t.Fatalf("manual read result: %+v %v", detail, err)
	}
	if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	return store, read, run, detail
}

func TestCompletedTaskRerunReusesManualReadAndRechecksAuthority(t *testing.T) {
	t.Run("reuses without replay", func(t *testing.T) {
		ctx := t.Context()
		store, _, original, accepted := completedManualReadFixture(t)
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); err != nil {
			t.Fatal(err)
		}
		run, err := store.RerunCompletedTask(ctx, original.TaskID, "manual-read-rerun", accepted.Task.Revision)
		if err != nil {
			t.Fatal(err)
		}
		var status, result string
		if err = store.db.QueryRow("SELECT status,result FROM run_reads WHERE run_id=?", run.ID).Scan(&status, &result); err != nil || status != "reused" || result == "" {
			t.Fatalf("saved evidence not reused: %q %q %v", status, result, err)
		}
		claimed, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			called = true
			if last := input[len(input)-1]; last.Read == nil || last.Read.Result == nil || last.Read.Result.Text != "样例服务状态：可用" {
				t.Errorf("reused source missing from input: %+v", last.Read)
			}
			return nil, emit("复用读取结果生成")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		var calls int
		if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || !called || calls != 1 {
			t.Fatalf("manual source replayed: called=%v calls=%d err=%v", called, calls, err)
		}
	})
	t.Run("revoked before admission", func(t *testing.T) {
		ctx := t.Context()
		store, read, original, accepted := completedManualReadFixture(t)
		if err := store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); !errors.Is(err, ErrReadNeedsSelection) {
			t.Fatalf("revoked source projected eligible: %v", err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "revoked-before-rerun", accepted.Task.Revision); !errors.Is(err, ErrReadNeedsSelection) {
			t.Fatalf("revoked source admitted: %v", err)
		}
	})
	t.Run("revoked after admission", func(t *testing.T) {
		ctx := t.Context()
		store, read, original, accepted := completedManualReadFixture(t)
		run, err := store.RerunCompletedTask(ctx, original.TaskID, "revoked-after-rerun", accepted.Task.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.SetToolPermission(ctx, read.ConnectionID, read.ToolName, read.ToolVersion, read.Revision, false); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil || claimed.ID != run.ID {
			t.Fatalf("claim: %+v %v", claimed, err)
		}
		called := false
		store.execute(ctx, testProvider(func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
			called = true
			return nil, errors.New("revoked evidence reached model")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		var status, code string
		var calls int
		if err = store.db.QueryRow("SELECT status,error_code FROM runs WHERE id=?", run.ID).Scan(&status, &code); err != nil {
			t.Fatal(err)
		}
		if err = store.db.QueryRow("SELECT count(*) FROM connection_calls").Scan(&calls); err != nil || called || calls != 1 || status != "interrupted" || code != "tool_read_reselection_required" {
			t.Fatalf("revoked evidence sent: called=%v calls=%d status=%s code=%s err=%v", called, calls, status, code, err)
		}
	})
}

func TestCompletedTaskRerunRejectsUnsafeInputsAndRollsBack(t *testing.T) {
	t.Run("unknown or failed read", func(t *testing.T) {
		for _, example := range []struct {
			status string
			want   error
		}{{"unknown", ErrReadUnconfirmed}, {"failed", ErrReadNeedsSelection}} {
			t.Run(example.status, func(t *testing.T) {
				ctx := t.Context()
				store, _, run, accepted := completedManualReadFixture(t)
				if _, err := store.db.Exec("UPDATE run_reads SET status=? WHERE run_id=?", example.status, run.ID); err != nil {
					t.Fatal(err)
				}
				if err := store.CheckCompletedTaskRerun(ctx, run.TaskID); !errors.Is(err, example.want) {
					t.Fatalf("unsafe read eligible: %v", err)
				}
				if _, err := store.RerunCompletedTask(ctx, run.TaskID, "unsafe-read-request", accepted.Task.Revision); !errors.Is(err, example.want) {
					t.Fatalf("unsafe read admitted: %v", err)
				}
			})
		}
	})
	t.Run("external operation", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, _ := completedRerunFixture(t)
		if _, err := store.db.Exec(`INSERT INTO operations(operation_id,task_id,capability_key,target_refs,params_digest,idempotency_key,state,registered_at,updated_at)
VALUES('previous-operation',?,'test','[]','digest','operation-key','succeeded',1,1)`, original.TaskID); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); !errors.Is(err, ErrCompletedTaskRerunUnsupported) {
			t.Fatalf("external operation eligible: %v", err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "external-rerun", accepted.Task.Revision); !errors.Is(err, ErrCompletedTaskRerunUnsupported) {
			t.Fatalf("external operation rerun: %v", err)
		}
	})
	t.Run("corrupt material", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, material := completedRerunFixture(t)
		if _, err := store.db.Exec("UPDATE materials SET content='改变的内容' WHERE id=?", material.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); err != nil {
			t.Fatalf("metadata-only eligibility: %v", err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "corrupt-material", accepted.Task.Revision); err == nil {
			t.Fatal("invalid material admitted")
		}
	})
	t.Run("failed insertion", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, _ := completedRerunFixture(t)
		if _, err := store.db.Exec(`CREATE TRIGGER reject_rerun_message BEFORE INSERT ON messages
WHEN NEW.role='user' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "failed-rerun", accepted.Task.Revision); err == nil {
			t.Fatal("injected write failure succeeded")
		}
		var runs, submissions int
		if err := store.db.QueryRow("SELECT (SELECT count(*) FROM runs WHERE task_id=?),(SELECT count(*) FROM submissions WHERE request_key='failed-rerun')", original.TaskID).Scan(&runs, &submissions); err != nil || runs != 1 || submissions != 0 {
			t.Fatalf("partial rerun persisted: runs=%d submissions=%d err=%v", runs, submissions, err)
		}
		detail, err := store.Detail(original.TaskID, true)
		if err != nil || detail.Task.Status != "completed" || detail.Acceptance == nil {
			t.Fatalf("failure changed completed task: %+v %v", detail, err)
		}
		if _, err = store.db.Exec("DROP TRIGGER reject_rerun_message"); err != nil {
			t.Fatal(err)
		}
		if _, err = store.RerunCompletedTask(ctx, original.TaskID, "failed-rerun", accepted.Task.Revision); err != nil {
			t.Fatalf("retry after rolled-back failure: %v", err)
		}
	})
}

func TestCompletedTaskRerunKeepsSourceContextAndMaterials(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.OpenWorkspace(filepath.Join(root, "workspace")); err != nil {
		t.Fatal(err)
	}
	material, err := saveMaterial(ctx, store.db, "materials/source.md", []byte("原始任务材料"), nil)
	if err != nil {
		t.Fatal(err)
	}
	unused, err := saveMaterial(ctx, store.db, "materials/omitted.md", []byte("源模型未使用的材料"), nil)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := saveMaterial(ctx, store.db, "materials/independent.md", []byte("独立交流的材料"), nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := func(key, text, conversation, reply string) Run {
		t.Helper()
		run, err := store.Submit(ctx, key, text, "", conversation, false)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil || claimed.ID != run.ID {
			t.Fatalf("claim chat: %+v %v", claimed, err)
		}
		if err = store.delta(ctx, run.ID, reply); err != nil {
			t.Fatal(err)
		}
		if err = store.finish(claimed, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := chat("old-chat", "旧的无关输入", "", "旧的无关回复")
	chat("useful-chat", "原任务需要的背景", first.ConversationID, "原任务需要的答复")
	criteria := "列出结论"
	original, err := store.SubmitWithDelivery(ctx, "context-task", "依据背景和材料写报告", "", first.ConversationID, true, Delivery{Criteria: &criteria}, material.ID, unused.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.execute(ctx, testProvider(func(modelCtx context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		var used []int64
		for _, m := range input {
			if m.Text == "原任务需要的背景" || m.Text == "原任务需要的答复" || strings.Contains(m.Text, "依据背景和材料写报告") {
				used = append(used, m.ID)
			}
		}
		if len(used) != 3 {
			t.Errorf("fixture could not find source inputs: %+v", input)
		}
		if err := RecordModelManifest(modelCtx, ManifestRequest{Messages: used, Materials: map[string]string{material.ID: material.Digest}}); err != nil {
			return nil, err
		}
		return nil, emit("原报告：已处理的内容")
	}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	completed, err := store.Detail(original.TaskID, true)
	if err != nil || completed.Artifact == nil {
		t.Fatalf("source task: %+v %v", completed, err)
	}
	if err = store.Accept(ctx, original.TaskID, completed.Artifact.ID, completed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	completed, err = store.Detail(original.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitChat(ctx, "separate-chat", "user:session", first.ConversationID, "之后的独立交流", "", false,
		Delivery{TaskRef: &MessageTaskRef{TaskID: original.TaskID}}, []string{independent.ID}, false); err != nil {
		t.Fatalf("admit standalone: %v", err)
	}
	if err = store.ProcessChatAdmissions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.ReadAdmission(ctx, "separate-chat")
	if err != nil || receipt.TaskID != "" || receipt.RunID == "" {
		t.Fatalf("standalone: %+v %v", receipt, err)
	}
	claimed, err = store.claim(ctx)
	if err != nil || claimed.ID != receipt.RunID {
		t.Fatalf("claim standalone: %+v %v", claimed, err)
	}
	if err = store.delta(ctx, claimed.ID, "独立交流的回复"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(claimed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	rerun, err := store.RerunCompletedTask(ctx, original.TaskID, "bounded-context-rerun", completed.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var replace bool
	var materialCount, allMaterials int
	if err = store.db.QueryRow(`SELECT replace_materials,(SELECT count(*) FROM message_materials mm WHERE mm.message_seq=m.seq AND mm.material_id=?),
(SELECT count(*) FROM message_materials mm WHERE mm.message_seq=m.seq)
FROM messages m WHERE m.run_id=? AND m.role='user'`, material.ID, rerun.ID).Scan(&replace, &materialCount, &allMaterials); err != nil || !replace || materialCount != 1 || allMaterials != 1 {
		t.Fatalf("source material not pinned: replace=%v count=%d total=%d err=%v", replace, materialCount, allMaterials, err)
	}
	claimed, err = store.claim(ctx)
	if err != nil || claimed.ID != rerun.ID {
		t.Fatalf("claim rerun: %+v %v", claimed, err)
	}
	called := false
	store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		called = true
		var hasContext, hasSource bool
		for _, m := range input {
			if strings.Contains(m.Text, "旧的无关") || strings.Contains(m.Text, "之后的独立交流") || strings.Contains(m.Text, "独立交流的回复") || strings.Contains(m.Text, "原报告：已处理的内容") {
				t.Errorf("unrelated or completed output entered rerun context: %+v", m)
			}
			if m.Text == "原任务需要的背景" {
				hasContext = true
			}
			for _, item := range m.Materials {
				if item.ID == independent.ID && item.Content != "" {
					t.Errorf("independent material entered rerun: %+v", item)
				}
				if item.ID == material.ID && item.Content == "原始任务材料" {
					hasSource = true
				}
			}
		}
		if !hasContext || !hasSource {
			t.Errorf("source context or material missing: %+v", input)
		}
		return nil, emit("重跑后的报告")
	}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	if !called {
		t.Fatal("rerun model was not called")
	}
}

func TestCompletedTaskRerunPublicationBoundary(t *testing.T) {
	ctx := t.Context()
	store, original, accepted, _ := completedRerunFixture(t)
	if _, err := store.db.Exec(`INSERT INTO publications(id,request_key,task_id,artifact_id,run_id,task_revision,workspace_id,filename,sha256,status,expires_at,updated_at)
VALUES('publication-fixture','publication-request',?,?,?,?,?,'report.md',?,'pending',1,1)`,
		original.TaskID, accepted.Artifact.ID, original.ID, accepted.Task.Revision, "workspace", accepted.Artifact.Digest); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "dispatched", "unknown"} {
		if _, err := store.db.Exec("UPDATE publications SET status=? WHERE id='publication-fixture'", status); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); !errors.Is(err, ErrPublicationOpen) {
			t.Fatalf("%s publication eligible: %v", status, err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "pending-publication-rerun", accepted.Task.Revision); !errors.Is(err, ErrPublicationOpen) {
			t.Fatalf("%s publication allowed rerun: %v", status, err)
		}
	}
	if _, err := store.db.Exec("UPDATE publications SET status='succeeded' WHERE id='publication-fixture'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RerunCompletedTask(ctx, original.TaskID, "succeeded-publication-rerun", accepted.Task.Revision); err != nil {
		t.Fatalf("previous succeeded publication blocked rerun: %v", err)
	}
	var status string
	if err := store.db.QueryRow("SELECT status FROM publications WHERE id='publication-fixture'").Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("previous publication changed: %q %v", status, err)
	}
}

func TestCompletedTaskRerunPreservesSkillBindingAndRevocation(t *testing.T) {
	completedWithSkill := func(t *testing.T) (*Store, Run, Detail, SkillVersion) {
		t.Helper()
		ctx := t.Context()
		store, err := Open(filepath.Join(t.TempDir(), "work.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		v1, err := store.DraftSkill(ctx, "reports", "原有方法", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ActivateSkill(ctx, v1.SkillID, v1.Version, 0, v1.Digest); err != nil {
			t.Fatal(err)
		}
		run, err := store.Submit(ctx, "skill-original", "整理报告", "reports", "", true)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			if skill := input[len(input)-1].Skill; skill == nil || skill.Version != v1.Version {
				t.Errorf("source skill was not bound: %+v", skill)
			}
			return nil, emit("原成果")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		detail, err := store.Detail(run.TaskID, true)
		if err != nil || detail.Artifact == nil {
			t.Fatalf("source result: %+v %v", detail, err)
		}
		if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
			t.Fatal(err)
		}
		detail, err = store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		return store, run, detail, v1
	}
	t.Run("new default does not replace source", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, v1 := completedWithSkill(t)
		v2, err := store.DraftSkill(ctx, "reports", "更新的方法", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ActivateSkill(ctx, v2.SkillID, v2.Version, 1, v2.Digest); err != nil {
			t.Fatal(err)
		}
		rerun, err := store.RerunCompletedTask(ctx, original.TaskID, "pinned-skill-rerun", accepted.Task.Revision)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil || claimed.ID != rerun.ID {
			t.Fatalf("claim rerun: %+v %v", claimed, err)
		}
		called := false
		store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			called = true
			if skill := input[len(input)-1].Skill; skill == nil || skill.Version != v1.Version || skill.Instructions != "原有方法" {
				t.Errorf("source skill changed: %+v", skill)
			}
			return nil, emit("重跑成果")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		if !called {
			t.Fatal("model was not called")
		}
	})
	t.Run("absent skill remains absent", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, _ := completedRerunFixture(t)
		skill, err := store.DraftSkill(ctx, "工作资料", "事后新增的方法", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ActivateSkill(ctx, skill.SkillID, skill.Version, 0, skill.Digest); err != nil {
			t.Fatal(err)
		}
		if _, err = store.RerunCompletedTask(ctx, original.TaskID, "no-skill-rerun", accepted.Task.Revision); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		store.execute(ctx, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			called = true
			if skill := input[len(input)-1].Skill; skill != nil {
				t.Errorf("new skill applied to rerun: %+v", skill)
			}
			return nil, emit("重跑成果")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		if !called {
			t.Fatal("model was not called")
		}
	})
	t.Run("disabled skill blocks and is rechecked", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, v1 := completedWithSkill(t)
		rerun, err := store.RerunCompletedTask(ctx, original.TaskID, "revoked-skill-rerun", accepted.Task.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.DisableSkill(ctx, v1.SkillID, 1); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil || claimed.ID != rerun.ID {
			t.Fatalf("claim rerun: %+v %v", claimed, err)
		}
		called := false
		store.execute(ctx, testProvider(func(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
			called = true
			return nil, errors.New("revoked skill reached model")
		}), claimed, RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
		var status, code string
		if err = store.db.QueryRow("SELECT status,error_code FROM runs WHERE id=?", rerun.ID).Scan(&status, &code); err != nil || called || status != "interrupted" || code != "read_checkpoint_unavailable" {
			t.Fatalf("disabled skill not stopped: called=%v status=%s code=%s err=%v", called, status, code, err)
		}
	})
	t.Run("disabled source is not offered", func(t *testing.T) {
		ctx := t.Context()
		store, original, accepted, v1 := completedWithSkill(t)
		if err := store.DisableSkill(ctx, v1.SkillID, 1); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckCompletedTaskRerun(ctx, original.TaskID); !errors.Is(err, ErrReadCheckpoint) {
			t.Fatalf("disabled source exposed as eligible: %v", err)
		}
		if _, err := store.RerunCompletedTask(ctx, original.TaskID, "disabled-skill-rerun", accepted.Task.Revision); !errors.Is(err, ErrReadCheckpoint) {
			t.Fatalf("disabled source accepted: %v", err)
		}
	})
}
