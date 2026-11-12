package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskEditPreservesAcceptanceAndQueuesOnlyOnExplicitContinue(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace := filepath.Join(root, "workspace")
	if err = store.OpenWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspace, "materials", "议程.md"), []byte("初始议程"), 0600); err != nil {
		t.Fatal(err)
	}
	material, err := store.CaptureMaterial(ctx, "materials/议程.md")
	if err != nil {
		t.Fatal(err)
	}
	criteria := "包含会议议程"
	run, err := store.SubmitWithDelivery(ctx, "original-edit-task", "整理议程", "工作资料", "", true, Delivery{Criteria: &criteria}, material.ID)
	if err != nil {
		t.Fatal(err)
	}
	load := func() Detail {
		t.Helper()
		detail, err := store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	title, scope := "议程摘要", "会议记录"
	if _, err = store.EditTask(ctx, run.TaskID, TaskEdit{Revision: load().Task.Revision, Title: &title}); !errors.Is(err, ErrActive) {
		t.Fatalf("queued Run allowed an edit: %v", err)
	}
	if _, err = store.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.delta(ctx, run.ID, "# 初稿\n\n初始议程"); err != nil {
		t.Fatal(err)
	}
	if err = store.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	approval := requestWrite(t, store, load(), "议程.md", "pending-edit-approval")
	if _, err = store.EditTask(ctx, run.TaskID, TaskEdit{Revision: load().Task.Revision, Title: &title}); !errors.Is(err, ErrPublicationOpen) {
		t.Fatalf("pending approval allowed an edit: %v", err)
	}
	if _, err = store.DecidePublication(ctx, approval.ID, "deny", approval.taskRevision); err != nil {
		t.Fatal(err)
	}
	detail := load()
	if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	accepted := load()
	edited, err := store.EditTask(ctx, run.TaskID, TaskEdit{Revision: accepted.Task.Revision, Title: &title, Scope: &scope})
	if err != nil || edited.Title != title || edited.Scope != scope || edited.Status != "completed" || edited.RequirementsChanged {
		t.Fatalf("metadata edit: %+v, %v", edited, err)
	}
	if current := load(); current.Acceptance == nil || *current.Acceptance != *accepted.Acceptance {
		t.Fatalf("metadata edit hid the original acceptance: %+v", current.Acceptance)
	}
	if _, err = store.EditTask(ctx, run.TaskID, TaskEdit{Revision: accepted.Task.Revision, Title: &title}); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale revision was accepted: %v", err)
	}
	goal, nextCriteria, filename := "按议题整理新的会议简报", "补充每项议题的负责人", "简报.md"
	edited, err = store.EditTask(ctx, run.TaskID, TaskEdit{Revision: edited.Revision, Goal: &goal, Criteria: &nextCriteria, RequiredFile: &filename})
	if err != nil || edited.Status != "interrupted" || !edited.RequirementsChanged || edited.Goal != goal || edited.Criteria != nextCriteria || edited.RequiredFile != filename {
		t.Fatalf("requirements edit: %+v, %v", edited, err)
	}
	detail = load()
	if detail.Run.ID != run.ID || detail.Acceptance != nil || detail.Artifact.ID != accepted.Artifact.ID {
		t.Fatalf("requirements edit replaced history or started work: %+v", detail)
	}
	snapshot, err := store.Overview(ctx, OverviewFilter{Scope: scope, Query: title})
	if err != nil || len(snapshot.Groups["waiting"]) != 1 || snapshot.Groups["waiting"][0].ID != run.TaskID || len(snapshot.Groups["active"])+len(snapshot.Groups["completed"]) != 0 {
		t.Fatalf("edited Task should await the user's decision: %+v, %v", snapshot.Groups, err)
	}
	if err = store.Accept(ctx, run.TaskID, detail.Artifact.ID, edited.Revision); !errors.Is(err, ErrState) {
		t.Fatalf("old artifact accepted against new requirements: %v", err)
	}
	old, err := store.Artifact(accepted.Artifact.ID)
	if err != nil || old.Acceptance == nil || old.Acceptance.Criteria != criteria || old.Digest != accepted.Artifact.Digest {
		t.Fatalf("old acceptance was lost: %+v, %v", old, err)
	}
	continued, err := store.Resume(ctx, run.TaskID, "continue-edited-task", edited.Revision)
	if err != nil || continued.ID == run.ID || continued.TaskID != run.TaskID || continued.ConversationID != run.ConversationID {
		t.Fatalf("explicit continue did not create a new Run in the same Task: %+v, %v", continued, err)
	}
	if repeated, err := store.Resume(ctx, run.TaskID, "continue-edited-task", edited.Revision); err != nil || repeated.ID != continued.ID {
		t.Fatalf("repeated continue changed the Run: %+v, %v", repeated, err)
	}
	detail = load()
	last := detail.Messages[len(detail.Messages)-1]
	if detail.Task.RequirementsChanged || last.Role != "user" || last.Text != goal || len(last.Materials) != 1 || last.Materials[0].ID != material.ID {
		t.Fatalf("new Run lost its revised input or attached material: %+v", last)
	}
}
