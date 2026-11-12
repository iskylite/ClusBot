package work

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillProposalEvidenceConfirmationAndRollback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	const scope, goal = "项目 A", "整理本周周报"
	base, err := s.DraftSkill(ctx, scope, "保留依据。", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSkill(ctx, base.SkillID, base.Version, base.Revision, base.Digest); err != nil {
		t.Fatal(err)
	}
	task, err := s.Submit(ctx, "proposal-source-task", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.delta(ctx, run.ID, "# 本周结论\n正文"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(task.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Accept(ctx, task.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CaptureAcceptedExperience(ctx, task.TaskID, d.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = s.ReviewExperience(ctx, candidate.ID, candidate.Revision, "approved")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	report := ExperienceEvaluationReport{Version: 1, Snapshot: strings.Repeat("a", 64), Tools: "disabled",
		Scoring: "literal_required_and_forbidden/v1", Model: ExperienceEvaluationConfig{Model: "fixture"},
		PolicyVersions: map[string]string{"candidate": "approved_exact_goal/v1"},
		CaseContexts:   []ExperienceEvaluationCaseContext{{ID: "gain", Scope: scope, Goal: goal}, {ID: "regression", Scope: scope, Goal: goal}},
		Results: []ExperienceEvaluationResult{
			{CaseID: "gain", Policy: "none", Status: "completed"},
			{CaseID: "gain", Policy: "current", Status: "completed", WrongReuse: &yes},
			{CaseID: "gain", Policy: "candidate", Status: "completed", Success: true, WrongReuse: &no,
				Sources: []ExperienceEvaluationSource{{CandidateID: candidate.ID, CandidateRevision: candidate.Revision,
					ArtifactID: candidate.ArtifactID, AcceptanceRevision: candidate.AcceptanceRevision, Digest: candidate.Digest}}},
			{CaseID: "regression", Policy: "none", Status: "completed"},
			{CaseID: "regression", Policy: "current", Status: "completed", Success: true},
			{CaseID: "regression", Policy: "candidate", Status: "completed"},
		},
	}
	proposal, err := s.ProposeSkill(ctx, scope, goal, report)
	if err != nil || proposal.Skill.Active || proposal.Skill.ApprovedAt != nil || proposal.BaseVersion == nil ||
		*proposal.BaseVersion != base.Version || proposal.Status != "pending" || len(proposal.Evidence.Gains) != 1 ||
		len(proposal.Evidence.Regressions) != 1 || !strings.Contains(proposal.Skill.Instructions, "本周结论") {
		t.Fatalf("invalid draft: %+v, %v", proposal, err)
	}
	retry, err := s.ProposeSkill(ctx, scope, goal, report)
	if err != nil || retry.ID != proposal.ID {
		t.Fatalf("report was duplicated: %+v, %v", retry, err)
	}
	count, err := s.PendingSkillProposals(ctx)
	if err != nil || count != 1 {
		t.Fatalf("pending proposal not discoverable: %d, %v", count, err)
	}
	if err = s.ActivateSkill(ctx, base.SkillID, proposal.Skill.Version, proposal.Skill.Revision, proposal.Skill.Digest); !errors.Is(err, ErrSkillProposalConfirmation) {
		t.Fatalf("ordinary activation bypassed proposal confirmation: %v", err)
	}
	target := SkillProposalVersion{Version: proposal.Skill.Version, Revision: proposal.Skill.Revision, Digest: proposal.Skill.Digest}
	wrong := target
	wrong.Digest = "wrong"
	if err = s.ApproveSkillProposal(ctx, proposal.ID, wrong, ""); !errors.Is(err, ErrState) {
		t.Fatalf("wrong content digest confirmed: %v", err)
	}
	if err = s.ApproveSkillProposal(ctx, proposal.ID, target, ""); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("confirmation bypassed replay: %v", err)
	}
	suite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "holdout", Goal: goal, Scope: scope, Prompt: "本周完成测试。", Required: []string{"完成测试"}, Forbidden: []string{"OLD-88"}},
	}}
	cfg := ExperienceEvaluationConfig{Model: "fixture", MaxOutput: 256, RequestTimeout: time.Second}
	replayWith := func(candidateAnswer string) SkillReplayReport {
		t.Helper()
		calls := 0
		provider := testProvider(func(_ context.Context, messages []Message, task bool, limit int, emit func(string) error) (*TokenUsage, error) {
			index := calls
			calls++
			expected := []SkillVersion{base, proposal.Skill}[index]
			if !task || limit != cfg.MaxOutput || len(messages) != 1 || messages[0].Text != "本周完成测试。\n\n本次任务目标：\n"+goal {
				t.Fatalf("replay changed the task input or budget: %+v", messages)
			}
			input := messages[0]
			if input.Skill == nil || input.Skill.Version != expected.Version || input.Skill.Instructions != expected.Instructions ||
				input.Experience != nil || len(input.Memories)+len(input.Materials)+len(input.Reads) != 0 || input.Read != nil {
				t.Fatalf("replay used the wrong Skill or loaded live context: %+v", input)
			}
			usage := &TokenUsage{InputTokens: new(int64(42)), OutputTokens: new(int64(8))}
			if index == 0 {
				return usage, emit("完成测试")
			}
			return usage, emit(candidateAnswer)
		})
		replay, err := s.ReplaySkillProposal(ctx, proposal.ID, target, suite, cfg, provider)
		if err != nil || calls != 2 || replay.Metrics["current"].InputTokensKnown != 42 ||
			replay.Metrics["candidate"].OutputTokensKnown != 8 || replay.Results[1].Answer != "" {
			t.Fatalf("invalid replay: %+v, calls=%d, %v", replay, calls, err)
		}
		return replay
	}
	firstPass := replayWith("完成测试")
	failed := replayWith("完成测试 OLD-88")
	if !firstPass.Passed || failed.Passed || failed.Metrics["candidate"].WrongReuseCount != 1 {
		t.Fatalf("replay gate accepted a regression: %+v", failed)
	}
	for _, replayID := range []string{firstPass.ID, failed.ID} {
		if err = s.ApproveSkillProposal(ctx, proposal.ID, target, replayID); !errors.Is(err, ErrSkillReplayRequired) {
			t.Fatalf("superseded or failed replay enabled confirmation: %v", err)
		}
	}
	passed := replayWith("完成测试")
	persisted, err := s.SkillProposals(ctx)
	if err != nil || len(persisted) != 1 || persisted[0].Replay == nil || persisted[0].Replay.ID != passed.ID ||
		!persisted[0].Replay.Passed || persisted[0].Skill.Active {
		t.Fatalf("passing replay was not saved as a draft: %+v, %v", persisted, err)
	}
	if err = s.ApproveSkillProposal(ctx, proposal.ID, target, passed.ID); err != nil {
		t.Fatal(err)
	}
	readForGoal := func(taskGoal string) (string, error) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return applicableSkillInstructions(tx, base.SkillID, proposal.Skill.Version, taskGoal)
	}
	if text, err := readForGoal("整理月报"); err != nil || text != base.Instructions {
		t.Fatalf("guidance escaped evaluated goal: %q, %v", text, err)
	}
	if text, err := readForGoal(goal); err != nil || text != proposal.Skill.Instructions {
		t.Fatalf("confirmed guidance did not apply: %q, %v", text, err)
	}
	if err = s.ApproveSkillProposal(ctx, proposal.ID, target, passed.ID); err != nil {
		t.Fatalf("retry of the original confirmation was not idempotent: %v", err)
	}
	if err = s.ActivateSkill(ctx, base.SkillID, base.Version, proposal.Skill.Revision+1, base.Digest); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []int{proposal.Skill.Revision, proposal.Skill.Revision + 2} {
		stale := target
		stale.Revision = revision
		if err = s.ApproveSkillProposal(ctx, proposal.ID, stale, passed.ID); !errors.Is(err, ErrTaskRevision) {
			t.Fatalf("proposal confirmation reactivated a rolled-back version at revision %d: %v", revision, err)
		}
	}
	if err = s.ActivateSkill(ctx, base.SkillID, proposal.Skill.Version, proposal.Skill.Revision+2, proposal.Skill.Digest); err != nil {
		t.Fatal(err)
	}
	d, err = s.Detail(task.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.ChangeConversation(ctx, task.ConversationID, d.Conversation.Revision, nil, &archived); err != nil {
		t.Fatal(err)
	}
	if _, err := readForGoal(goal); !errors.Is(err, ErrExperienceSource) {
		t.Fatalf("archived source remained in generated skill: %v", err)
	}
	items, err := s.SkillProposals(ctx)
	if err != nil || items[0].Status != "unavailable" || items[0].Skill.Instructions != "" {
		t.Fatalf("unavailable proposal exposed derived text: %+v, %v", items, err)
	}
	if err = s.ActivateSkill(ctx, base.SkillID, base.Version, items[0].Skill.Revision, base.Digest); err != nil {
		t.Fatal(err)
	}
	versions, err := s.Skills(ctx)
	if err != nil || len(versions) != 2 || !versions[1].Active || versions[0].Instructions != "" {
		t.Fatalf("rollback or source redaction failed: %+v, %v", versions, err)
	}
}
