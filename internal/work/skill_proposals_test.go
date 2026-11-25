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
	badReport := ExperienceEvaluationReport{Version: 1, Snapshot: strings.Repeat("a", 64), Tools: "disabled",
		Scoring: scoringLiteralV1, Model: ExperienceEvaluationConfig{Model: "fixture"},
		PolicyVersions: map[string]string{approvedCandidateExperiencePolicy: "approved_exact_goal/v1"},
		CaseContexts:   []ExperienceEvaluationCaseContext{{ID: "gain", Scope: scope, Goal: goal}, {ID: "regression", Scope: scope, Goal: goal}},
		Results: []ExperienceEvaluationResult{
			{CaseID: "gain", Policy: "none", Status: "completed"},
			{CaseID: "gain", Policy: acceptedExperiencePolicy, Status: "completed", WrongReuse: &yes},
			{CaseID: "gain", Policy: approvedCandidateExperiencePolicy, Status: "completed", Success: true, WrongReuse: &no,
				Sources: []ExperienceEvaluationSource{{CandidateID: candidate.ID, CandidateRevision: candidate.Revision,
					ArtifactID: candidate.ArtifactID, AcceptanceRevision: candidate.AcceptanceRevision, Digest: candidate.Digest}}},
			{CaseID: "regression", Policy: "none", Status: "completed"},
			{CaseID: "regression", Policy: acceptedExperiencePolicy, Status: "completed", Success: true},
			{CaseID: "regression", Policy: approvedCandidateExperiencePolicy, Status: "completed"},
		},
	}
	if _, err := s.ProposeSkill(ctx, scope, goal, badReport); !errors.Is(err, ErrSkillProposal) {
		t.Fatalf("regression report was not rejected: %v", err)
	}
	report := ExperienceEvaluationReport{Version: 1, Snapshot: strings.Repeat("a", 64), Tools: "disabled",
		Scoring: scoringLiteralV1, Model: ExperienceEvaluationConfig{Model: "fixture"},
		PolicyVersions: map[string]string{approvedCandidateExperiencePolicy: "approved_exact_goal/v1"},
		CaseContexts:   []ExperienceEvaluationCaseContext{{ID: "gain", Scope: scope, Goal: goal}},
		Results: []ExperienceEvaluationResult{
			{CaseID: "gain", Policy: "none", Status: "completed"},
			{CaseID: "gain", Policy: acceptedExperiencePolicy, Status: "completed", WrongReuse: &yes},
			{CaseID: "gain", Policy: approvedCandidateExperiencePolicy, Status: "completed", Success: true, WrongReuse: &no,
				Sources: []ExperienceEvaluationSource{{CandidateID: candidate.ID, CandidateRevision: candidate.Revision,
					ArtifactID: candidate.ArtifactID, AcceptanceRevision: candidate.AcceptanceRevision, Digest: candidate.Digest}}},
		},
	}
	proposal, err := s.ProposeSkill(ctx, scope, goal, report)
	if err != nil || proposal.Skill.Active || proposal.Skill.ApprovedAt != nil || proposal.BaseVersion == nil ||
		*proposal.BaseVersion != base.Version || proposal.Status != "pending" || len(proposal.Evidence.Gains) != 1 ||
		len(proposal.Evidence.Regressions) != 0 || !strings.Contains(proposal.Skill.Instructions, "本周结论") {
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
		if err != nil || calls != 2 || replay.Metrics[skillReplayArmBase].InputTokensKnown != 42 ||
			replay.Metrics[skillReplayArmProposed].OutputTokensKnown != 8 || replay.Results[1].Answer != "" {
			t.Fatalf("invalid replay: %+v, calls=%d, %v", replay, calls, err)
		}
		return replay
	}
	firstPass := replayWith("完成测试")
	failed := replayWith("完成测试 OLD-88")
	if !firstPass.Passed || failed.Passed || failed.Metrics[skillReplayArmProposed].WrongReuseCount != 1 {
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

func TestSkillProposalMatchModesBoundSuiteAndSynthesis(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "rsi-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	const scope, goal = "集群巡检", "检查 POD7 节点健康"
	base, err := s.DraftSkill(ctx, scope, "先查只读指标再给结论。", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSkill(ctx, base.SkillID, base.Version, base.Revision, base.Digest); err != nil {
		t.Fatal(err)
	}
	task, err := s.Submit(ctx, "rsi-source", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.delta(ctx, run.ID, "# 巡检结论\nOK"); err != nil {
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
	suite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "c1", Scope: scope, Goal: goal, Prompt: "请巡检 POD7", Required: []string{"巡检结论"}, Forbidden: []string{"OLD-BAD"}},
	}}
	cfg := ExperienceEvaluationConfig{Model: "fixture", MaxOutput: 256, RequestTimeout: time.Second}
	evalProvider := testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		in, out := int64(10), int64(5)
		if strings.Contains(input[0].Text, "经审阅") {
			return &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit("巡检结论")
		}
		return &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit("未按要求输出")
	})
	evalRun, err := s.RunExperienceEvaluation(ctx, suite, cfg, evalProvider)
	if err != nil {
		t.Fatal(err)
	}
	synthProvider := testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		return nil, emit("先查只读指标再给结论。\n- 必须包含巡检结论")
	})
	proposal, err := s.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{
		EvaluationRunID: evalRun.ID,
		MatchMode:       SkillMatchScopeKeywords,
		MatchKeywords:   []string{"POD7", "健康"},
		Synthesize:      true,
		Provider:        synthProvider,
		ModelConfig:     cfg,
	})
	if err != nil {
		t.Fatalf("ProposeSkillWithOptions: %v", err)
	}
	if proposal.DraftMode != "synthesized" || proposal.MatchMode != SkillMatchScopeKeywords || proposal.SuiteDigest != evalRun.SuiteDigest {
		t.Fatalf("unexpected proposal metadata: %+v", proposal)
	}
	target := SkillProposalVersion{Version: proposal.Skill.Version, Revision: proposal.Skill.Revision, Digest: proposal.Skill.Digest}
	// Mismatched suite must be rejected when SuiteDigest is bound.
	wrongSuite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "other", Scope: scope, Goal: goal, Prompt: "不同用例", Required: []string{"巡检结论"}, Forbidden: []string{"OLD-BAD"}},
	}}
	if _, err := s.ReplaySkillProposal(ctx, proposal.ID, target, wrongSuite, cfg, evalProvider); !errors.Is(err, ErrSkillReplayInput) {
		t.Fatalf("mismatched suite was not rejected: %v", err)
	}
	// Empty suite automatically loads the bound suite from skill_evaluation_suites.
	replayProvider := testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		return nil, emit("巡检结论")
	})
	replay, err := s.ReplaySkillProposal(ctx, proposal.ID, target, ExperienceEvaluationSuite{}, cfg, replayProvider)
	if err != nil || !replay.Passed || replay.SuiteDigest != evalRun.SuiteDigest {
		t.Fatalf("bound suite auto-replay failed: %+v, %v", replay, err)
	}
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, replay.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Keyword match succeeds for similar goal in the same scope, falls back to base for unrelated goal.
	matched, err := applicableSkillInstructions(tx, base.SkillID, proposal.Skill.Version, "排查 pod7 异常")
	if err != nil || !strings.Contains(matched, "必须包含巡检结论") {
		t.Fatalf("keyword match failed: %q, %v", matched, err)
	}
	unmatched, err := applicableSkillInstructions(tx, base.SkillID, proposal.Skill.Version, "整理财务报表")
	if err != nil || unmatched != base.Instructions {
		t.Fatalf("unrelated goal should keep base instructions: %q, %v", unmatched, err)
	}
}
