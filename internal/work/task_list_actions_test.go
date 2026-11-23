package work

import (
	"path/filepath"
	"testing"
)

func TestTaskListActionsFollowTaskState(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	run, err := store.Submit(ctx, "list-actions-original", "任务操作验证", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string, rename bool) Task {
		t.Helper()
		task, err := store.ReadTaskSummary(ctx, run.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		actions, err := store.TaskListActions(ctx, task)
		if err != nil || actions.StatusAction != want || actions.CanRename != rename {
			t.Fatalf("state %s/%s: actions=%+v err=%v", task.Status, task.StatusReason, actions, err)
		}
		return task
	}
	queued := check("pause", false)
	if err := store.Control(ctx, run.TaskID, "pause", queued.Revision); err != nil {
		t.Fatal(err)
	}
	paused := check("resume", true)
	resumed, err := store.Resume(ctx, run.TaskID, "list-actions-resume", paused.Revision)
	if err != nil {
		t.Fatal(err)
	}
	check("pause", false)
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != resumed.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := store.delta(ctx, claimed.ID, "测试成果"); err != nil {
		t.Fatal(err)
	}
	if err := store.finish(claimed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	waiting := check("accept", true)
	detail, err := store.Detail(run.TaskID, true)
	if err != nil || detail.Artifact == nil {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	if err := store.Accept(ctx, run.TaskID, detail.Artifact.ID, waiting.Revision); err != nil {
		t.Fatal(err)
	}
	check("rerun", true)
}

func TestTaskListActionsDistinguishRetryAndUnavailableCheckpoint(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	run, err := store.Submit(ctx, "list-actions-retry", "重试测试", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err = store.db.ExecContext(ctx, `INSERT INTO run_read_checkpoints
(run_id,version,provider_binding,context_digest,output_budget,active_budget_ms)
VALUES (?,?,'fixture','fixture',128,60000)`, run.ID, readCheckpointVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(claimed, "interrupted", "", runAccounting{errorCode: "output_budget"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.ReadTaskSummary(ctx, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := store.TaskListActions(ctx, task)
	if err != nil || actions.StatusAction != "retry" {
		t.Fatalf("retry checkpoint: %+v %v", actions, err)
	}
	if _, err = store.db.ExecContext(ctx, "UPDATE run_read_checkpoints SET version=99 WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	actions, err = store.TaskListActions(ctx, task)
	if err != nil || actions.StatusAction != "" {
		t.Fatalf("unavailable checkpoint offered recovery: %+v %v", actions, err)
	}
}
