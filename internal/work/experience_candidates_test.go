package work

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExperienceCandidatesUseVerifiableSources(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	initial, err := s.Submit(ctx, "experience-candidate-task", "编写周报", "项目 A", "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body := "# 结论\n~~~text\n# 代码不是文档标题\n~~~\n## 下周计划"
	if err = s.delta(ctx, run.ID, body); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(initial.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureAcceptedExperience(ctx, initial.TaskID, d.Artifact.ID); !errors.Is(err, ErrExperienceSource) {
		t.Fatalf("unaccepted artifact became a candidate: %v", err)
	}
	if err = s.Accept(ctx, initial.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	items, err := s.ExperienceCandidates(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("automatic candidate: %+v, %v", items, err)
	}
	first := items[0]
	if first.Status != "pending" || first.Kind != "accepted_result" || first.Scope != "项目 A" ||
		first.Digest != d.Artifact.Digest || first.AcceptanceRevision != d.Task.Revision+1 ||
		!reflect.DeepEqual(first.FormatCues, []string{"结论", "下周计划"}) {
		t.Fatalf("wrong accepted evidence: %+v", first)
	}
	retry, err := s.CaptureAcceptedExperience(ctx, initial.TaskID, d.Artifact.ID)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("same source created another candidate: %+v, %v", retry, err)
	}
	approved, err := s.ReviewExperience(ctx, first.ID, first.Revision, "approved")
	if err != nil || approved.Revision != first.Revision+1 || approved.Status != "approved" {
		t.Fatalf("review failed: %+v, %v", approved, err)
	}
	if _, err = s.ReviewExperience(ctx, first.ID, first.Revision, "rejected"); !errors.Is(err, ErrExperienceRevision) {
		t.Fatalf("stale review replaced a decision: %v", err)
	}

	correction := "上版缺少未完成事项。下次把它单独列出。"
	next, err := s.Submit(ctx, "experience-candidate-correction", correction, "", initial.ConversationID, false)
	if err != nil {
		t.Fatal(err)
	}
	var seq, assistant int64
	if err = s.db.QueryRow("SELECT seq FROM messages WHERE run_id=? AND role='user'", next.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT seq FROM messages WHERE run_id=? AND role='assistant'", run.ID).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureExperienceCorrection(ctx, assistant, ""); !errors.Is(err, ErrExperienceSource) {
		t.Fatalf("assistant text accepted as user correction: %v", err)
	}
	candidate, err := s.CaptureExperienceCorrection(ctx, seq, d.Artifact.ID)
	if err != nil || candidate.Correction != correction || candidate.CounterDigest != d.Artifact.Digest ||
		candidate.MessageID != seq || candidate.Scope != "项目 A" || candidate.Status != "pending" {
		t.Fatalf("wrong correction evidence: %+v, %v", candidate, err)
	}
	retry, err = s.CaptureExperienceCorrection(ctx, seq, d.Artifact.ID)
	if err != nil || retry.ID != candidate.ID {
		t.Fatalf("correction was duplicated: %+v, %v", retry, err)
	}
	claimed, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finish(claimed, "interrupted", "测试结束", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err = s.Detail(initial.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	archive := true
	if _, err = s.ChangeConversation(ctx, initial.ConversationID, d.Conversation.Revision, nil, &archive); err != nil {
		t.Fatal(err)
	}
	items, err = s.ExperienceCandidates(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("archived candidates: %+v, %v", items, err)
	}
	for _, item := range items {
		if item.Status != "unavailable" || item.Correction != "" || item.Goal != "" || len(item.FormatCues) != 0 {
			t.Fatalf("archived source leaked a derived preview: %+v", item)
		}
	}
	if _, err = s.ReviewExperience(ctx, candidate.ID, candidate.Revision, "approved"); !errors.Is(err, ErrExperienceSource) {
		t.Fatalf("archived source became usable: %v", err)
	}
}
