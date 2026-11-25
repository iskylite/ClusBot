package work

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var rsiFixtureModel = ExperienceEvaluationConfig{Model: "fixture", MaxOutput: 256, RequestTimeout: time.Second}

func rsiFixture(t *testing.T, scope, goal, baseline string) (*Store, SkillVersion, ExperienceCandidate, ExperienceEvaluationRun, ExperienceEvaluationSuite) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	var base SkillVersion
	if baseline != "" {
		base, err = s.DraftSkill(ctx, scope, baseline, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ActivateSkill(ctx, base.SkillID, base.Version, base.Revision, base.Digest); err != nil {
			t.Fatal(err)
		}
	}
	task, err := s.Submit(ctx, "rsi-source", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.delta(ctx, run.ID, "# 结论\n可核实的正文"); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(run, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(task.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ctx, task.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	cand, err := s.CaptureAcceptedExperience(ctx, task.TaskID, d.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	cand, err = s.ReviewExperience(ctx, cand.ID, cand.Revision, "approved")
	if err != nil {
		t.Fatal(err)
	}
	suite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "training", Scope: scope, Goal: goal, Prompt: "开发任务：使用第十批材料", Required: []string{"给出结论"}, Forbidden: []string{"OLD-88"}},
	}}
	runEval, err := s.RunExperienceEvaluation(ctx, suite, rsiFixtureModel, testProvider(func(_ context.Context, messages []Message, task bool, limit int, emit func(string) error) (*TokenUsage, error) {
		if !task || len(messages) != 1 || limit != rsiFixtureModel.MaxOutput {
			t.Fatalf("invalid isolated evaluation: %+v", messages)
		}
		if strings.Contains(messages[0].Text, "经审阅") {
			return nil, emit("给出结论")
		}
		return nil, emit("没有完成要求")
	}))
	if err != nil {
		t.Fatal(err)
	}
	return s, base, cand, runEval, suite
}

func rsiFixtureHoldout(scope, goal string) ExperienceEvaluationSuite {
	return ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "holdout-a", Scope: scope, Goal: goal, Prompt: "第十六批独立材料只包含热身测试", Required: []string{"热身已测"}, Forbidden: []string{"OLD-88"}},
		{ID: "holdout-b", Scope: scope, Goal: goal, Prompt: "提供第十八批中的冷启动验收证据", RequiredHeadings: []string{"冷启动验收"}, ForbiddenRegex: []string{`OLD-\d+`}},
	}}
}

func proposeFixtureSkill(t *testing.T, s *Store, scope, goal string, eval ExperienceEvaluationRun, opts SkillProposalOptions) SkillProposal {
	t.Helper()
	opts.EvaluationRunID = eval.ID
	p, err := s.ProposeSkillWithOptions(t.Context(), scope, goal, ExperienceEvaluationReport{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func replayFixtureProvider(t *testing.T, suite ExperienceEvaluationSuite, base, candidate string, variant string, count *int) Provider {
	t.Helper()
	return testProvider(func(_ context.Context, messages []Message, task bool, limit int, emit func(string) error) (*TokenUsage, error) {
		*count += 1
		if !task || limit != rsiFixtureModel.MaxOutput || len(messages) != 1 {
			t.Fatalf("unexpected isolated replay: %+v", messages)
		}
		m := messages[0]
		var found *ExperienceEvaluationCase
		for _, c := range suite.Cases {
			if m.Text == c.Prompt+"\n\n本次任务范围：\n"+c.Scope+"\n\n本次任务目标：\n"+c.Goal {
				found = &c
			}
		}
		if found == nil || m.Experience != nil || len(m.Memories)+len(m.Materials)+len(m.Reads) != 0 || m.Read != nil {
			t.Fatalf("replay did not use the frozen case or raised live data: %+v", m)
		}
		cand := false
		if m.Skill != nil {
			if m.Skill.Scope != found.Scope || m.Skill.Digest != digest(m.Skill.Instructions) {
				t.Fatalf("unfrozen case Skill: %+v", m.Skill)
			}
			switch m.Skill.Instructions {
			case candidate:
				cand = true
			case base:
			default:
				t.Fatalf("unexpected case Skill text: %+v", m.Skill)
			}
		}
		answer := "# 冷启动验收"
		if found.ID == "holdout-a" {
			if cand || variant == "equal" {
				answer = "热身已测"
			} else {
				answer = "没有给出测试事实"
			}
		}
		if cand && variant == "forbidden" && found.ID == "holdout-b" {
			answer += " OLD-88"
		}
		if cand && variant == "regression" && found.ID == "holdout-b" {
			answer = "不再给出可核验证据"
		}
		if cand && variant == "incomplete" && found.ID == "holdout-b" {
			return nil, errors.New("local provider fixture unavailable")
		}
		in, out := int64(42), int64(8)
		return &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit(answer)
	})
}

func TestSkillProposalEvidenceConfirmationAndRollback(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "项目 A", "整理本周周报"
	s, base, cand, eval, _ := rsiFixture(t, scope, goal, "保留依据。")
	if _, err := s.ProposeSkill(ctx, scope, goal, eval.Report); !errors.Is(err, ErrSkillProposal) {
		t.Fatalf("client-supplied report was treated as server evidence: %v", err)
	}
	proposal := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{})
	if proposal.Skill.Active || proposal.Skill.ApprovedAt != nil || proposal.BaseVersion == nil ||
		*proposal.BaseVersion != base.Version || proposal.Status != "pending" || len(proposal.Evidence.Gains) != 1 ||
		len(proposal.Evidence.Regressions) != 0 || !strings.Contains(proposal.Skill.Instructions, "结论") {
		t.Fatalf("invalid draft: %+v", proposal)
	}
	retry := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{})
	if retry.ID != proposal.ID || retry.ReportDigest != proposal.ReportDigest {
		t.Fatalf("server evaluation was duplicated: %+v", retry)
	}
	count, err := s.PendingSkillProposals(ctx)
	if err != nil || count != 1 {
		t.Fatalf("pending proposal not discoverable: %d, %v", count, err)
	}
	if err := s.ActivateSkill(ctx, base.SkillID, proposal.Skill.Version, proposal.Skill.Revision, proposal.Skill.Digest); !errors.Is(err, ErrSkillProposalConfirmation) {
		t.Fatalf("ordinary activation bypassed proposal confirmation: %v", err)
	}
	target := SkillProposalVersion{Version: proposal.Skill.Version, Revision: proposal.Skill.Revision, Digest: proposal.Skill.Digest}
	wrong := target
	wrong.Digest = "wrong"
	if err := s.ApproveSkillProposal(ctx, proposal.ID, wrong, ""); !errors.Is(err, ErrState) {
		t.Fatalf("wrong content digest confirmed: %v", err)
	}
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, ""); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("confirmation bypassed replay: %v", err)
	}

	suite := rsiFixtureHoldout(scope, goal)
	replayWith := func(variant string) SkillReplayReport {
		t.Helper()
		calls := 0
		report, err := s.ReplaySkillProposal(ctx, proposal.ID, target, suite, rsiFixtureModel, replayFixtureProvider(t, suite, base.Instructions, proposal.Skill.Instructions, variant, &calls))
		if err != nil || calls != 6 || report.TrainingDigest != eval.SuiteDigest || report.SuiteDigest == eval.SuiteDigest || report.Contract != skillReplayContract ||
			report.Metrics[skillReplayArmPlain].InputTokensKnown != 84 || len(report.Results) != 6 {
			t.Fatalf("invalid replay: %+v, calls=%d, %v", report, calls, err)
		}
		for _, item := range report.Results {
			if item.Answer != "" || len(item.MissingRequired)+len(item.ForbiddenHits) > 0 {
				t.Fatal("replay persisted source/assertion text")
			}
		}
		return report
	}
	identical := replayWith("equal")
	firstPass := replayWith("improved")
	failed := replayWith("forbidden")
	missing := replayWith("incomplete")
	regression := replayWith("regression")
	if identical.Passed || !firstPass.Passed || failed.Passed || missing.Passed || regression.Passed ||
		len(firstPass.Gains) != 1 || failed.Metrics[skillReplayArmProposed].WrongReuseCount != 1 ||
		len(missing.Incomplete) != 1 || len(regression.Regressions) != 1 {
		t.Fatalf("gate accepted no improvement, forbidden, missing, or regressions: %+v %+v %+v", failed, missing, regression)
	}
	for _, replayID := range []string{identical.ID, firstPass.ID, failed.ID, missing.ID, regression.ID} {
		if err := s.ApproveSkillProposal(ctx, proposal.ID, target, replayID); !errors.Is(err, ErrSkillReplayRequired) {
			t.Fatalf("superseded or failed replay enabled confirmation: %v", err)
		}
	}
	passed := replayWith("improved")
	persisted, err := s.SkillProposals(ctx)
	if err != nil || len(persisted) != 1 || persisted[0].Replay == nil || persisted[0].Replay.ID != passed.ID ||
		!persisted[0].Replay.Passed || persisted[0].Skill.Active {
		t.Fatalf("passing replay was not saved as a draft: %+v, %v", persisted, err)
	}
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, ""); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("passing replay bypassed explicit ID confirmation: %v", err)
	}
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, passed.ID); err != nil {
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
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, passed.ID); err != nil {
		t.Fatalf("original confirmation retry not idempotent: %v", err)
	}
	if err := s.ActivateSkill(ctx, base.SkillID, base.Version, proposal.Skill.Revision+1, base.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveSkillProposal(ctx, proposal.ID, target, passed.ID); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale approval reactivated a rolled-back version: %v", err)
	}
	if err := s.ActivateSkill(ctx, base.SkillID, proposal.Skill.Version, proposal.Skill.Revision+2, proposal.Skill.Digest); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(cand.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.ChangeConversation(ctx, d.Conversation.ID, d.Conversation.Revision, nil, &archived); err != nil {
		t.Fatal(err)
	}
	if _, err := readForGoal(goal); !errors.Is(err, ErrExperienceSource) {
		t.Fatalf("archived source remained in generated skill: %v", err)
	}
	items, err := s.SkillProposals(ctx)
	if err != nil || items[0].Status != "unavailable" || items[0].Skill.Instructions != "" {
		t.Fatalf("unavailable proposal exposed derived text: %+v, %v", items, err)
	}
	if err := s.ActivateSkill(ctx, base.SkillID, base.Version, items[0].Skill.Revision, base.Digest); err != nil {
		t.Fatal(err)
	}
	versions, err := s.Skills(ctx)
	if err != nil || len(versions) != 2 || !versions[1].Active || versions[0].Instructions != "" {
		t.Fatalf("rollback or source redaction failed: %+v, %v", versions, err)
	}
}

func TestSkillProposalMatchModesHoldoutAndSynthesis(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "集群巡检", "检查 POD7 节点健康"
	s, base, candidate, eval, dev := rsiFixture(t, scope, goal, "先查只读指标再给结论。")
	for _, mode := range []string{SkillMatchScopeAll, SkillMatchScopeKeywords} {
		if _, err := s.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{
			EvaluationRunID: eval.ID, MatchMode: mode, MatchKeywords: []string{"POD7"},
		}); !errors.Is(err, ErrSkillProposal) {
			t.Fatalf("unproven scope widening admitted: %q %v", mode, err)
		}
	}
	holdout := rsiFixtureHoldout(scope, goal)
	holdout.Cases[1].Goal = "检查 pod7 节点健康"
	p := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{
		MatchMode: SkillMatchGoalExact, Synthesize: true, ModelConfig: rsiFixtureModel,
		Provider: testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			if len(input) != 1 || strings.Contains(input[0].Text, holdout.Cases[0].Prompt) || !strings.Contains(input[0].Text, "仅输出增量") {
				t.Fatalf("synthesis saw holdout or tried rewriting baseline: %+v", input)
			}
			return nil, emit("- 必须包含巡检结论")
		}),
	})
	if p.DraftMode != "synthesized" || p.MatchMode != SkillMatchGoalExact || p.SuiteDigest != eval.SuiteDigest ||
		!strings.HasPrefix(p.Skill.Instructions, base.Instructions) || !strings.Contains(p.Skill.Instructions, "必须包含巡检结论") {
		t.Fatalf("bad synthesized overlay: %+v", p)
	}
	target := SkillProposalVersion{Version: p.Skill.Version, Revision: p.Skill.Revision, Digest: p.Skill.Digest}
	never := testProvider(func(_ context.Context, _ []Message, _ bool, _ int, _ func(string) error) (*TokenUsage, error) {
		t.Fatal("invalid evidence invoked the model")
		return nil, nil
	})
	for _, scenario := range []struct {
		name   string
		mutate func(*ExperienceEvaluationSuite)
	}{
		{"no holdout", func(s *ExperienceEvaluationSuite) { s.Cases = nil }},
		{"copy of development prompt with renamed case", func(s *ExperienceEvaluationSuite) { s.Cases[1].Prompt = "  开发任务！使用第十批材料  " }},
		{"wrong scope", func(s *ExperienceEvaluationSuite) { s.Cases[1].Scope = "另一项目" }},
		{"different task goal", func(s *ExperienceEvaluationSuite) { s.Cases[1].Goal = "回收 POD7 资源" }},
		{"leak of example task", func(s *ExperienceEvaluationSuite) { s.Cases[1].TaskID = candidate.TaskID }},
		{"unowned task ID", func(s *ExperienceEvaluationSuite) { s.Cases[1].TaskID = "not-present" }},
		{"duplicate real prompt", func(s *ExperienceEvaluationSuite) { s.Cases[1].Prompt = s.Cases[0].Prompt }},
		{"forbidden not measured for each case", func(s *ExperienceEvaluationSuite) { s.Cases[1].ForbiddenRegex = nil }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := rsiFixtureHoldout(scope, goal)
			scenario.mutate(&h)
			if _, err := s.ReplaySkillProposal(ctx, p.ID, target, h, rsiFixtureModel, never); !errors.Is(err, ErrSkillReplayInput) {
				t.Fatalf("unsafe suite passed: %v", err)
			}
		})
	}
	if _, err := s.ReplaySkillProposal(ctx, p.ID, target, dev, rsiFixtureModel, never); !errors.Is(err, ErrSkillReplayInput) {
		t.Fatalf("training suite was accepted as holdout: %v", err)
	}
	calls := 0
	replay, err := s.ReplaySkillProposal(ctx, p.ID, target, holdout, rsiFixtureModel, replayFixtureProvider(t, holdout, base.Instructions, p.Skill.Instructions, "improved", &calls))
	if err != nil || !replay.Passed || calls != 6 {
		t.Fatalf("independent different-case goals failed: %+v calls=%d %v", replay, calls, err)
	}
	var stored string
	if err := s.db.QueryRow("SELECT report FROM skill_proposal_replays WHERE proposal_id=?", p.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `"contract":"isolated_holdout_paired"`) {
		t.Fatal("the client cannot identify validated replay results")
	}
	for _, secret := range []string{holdout.Cases[0].Prompt, holdout.Cases[1].Prompt, "热身已测", "OLD-88"} {
		if strings.Contains(stored, secret) {
			t.Fatalf("unbounded or sensitive replay contents retained: %q", secret)
		}
	}
	holdout.Cases[1].Prompt = "切换至新的套件"
	if _, err := s.ReplaySkillProposal(ctx, p.ID, target, holdout, rsiFixtureModel, never); !errors.Is(err, ErrSkillReplayInput) {
		t.Fatalf("frozen holdout was switched after observing a result: %v", err)
	}
	if err := s.ApproveSkillProposal(ctx, p.ID, target, replay.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSkillSynthesisDoesNotHoldWriterAndRechecksAuthority(t *testing.T) {
	const scope, goal = "项目 B", "整理月报"
	for _, change := range []string{"base", "revoke"} {
		t.Run(change, func(t *testing.T) {
			s, base, candidate, eval, _ := rsiFixture(t, scope, goal, "原方法不变。")
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			began, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				_, err := s.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{
					EvaluationRunID: eval.ID, Synthesize: true, ModelConfig: rsiFixtureModel,
					Provider: testProvider(func(providerCtx context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
						close(began)
						select {
						case <-proceed:
							return nil, emit("只增加验收证据")
						case <-providerCtx.Done():
							return nil, providerCtx.Err()
						}
					}),
				})
				done <- err
			}()
			select {
			case <-began:
			case err := <-done:
				t.Fatalf("failed before synthesis: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, time.Second)
			defer writeCancel()
			var want error
			switch change {
			case "base":
				next, err := s.DraftSkill(writeCtx, scope, "新的基线内容。", base.Revision+1)
				if err != nil {
					t.Fatalf("network call held the writer: %v", err)
				}
				if err := s.ActivateSkill(writeCtx, base.SkillID, next.Version, next.Revision, next.Digest); err != nil {
					t.Fatal(err)
				}
				want = ErrTaskRevision
			case "revoke":
				d, err := s.Detail(candidate.TaskID, true)
				if err != nil {
					t.Fatal(err)
				}
				archived := true
				if _, err := s.ChangeConversation(writeCtx, d.Conversation.ID, d.Conversation.Revision, nil, &archived); err != nil {
					t.Fatalf("network call held the writer: %v", err)
				}
				want = ErrExperienceSource
			}
			close(proceed)
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("stale synthesis was published: %v (want %v)", err, want)
			}
			var n int
			if err := s.db.QueryRow("SELECT count(*) FROM skill_proposals").Scan(&n); err != nil || n != 0 {
				t.Fatalf("stale draft was persisted: %d %v", n, err)
			}
		})
	}
}

func TestReplayDoesNotTrustStoredPassBitOrUnprovenHistory(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "project-q", "summarize POD7"
	s, base, _, eval, _ := rsiFixture(t, scope, goal, "keep evidence")
	p := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{})
	target := SkillProposalVersion{Version: p.Skill.Version, Revision: p.Skill.Revision, Digest: p.Skill.Digest}
	legacy, _ := json.Marshal(SkillReplayReport{ID: "old-pass", Target: target, Passed: true})
	if _, err := s.db.Exec("INSERT INTO skill_proposal_replays(proposal_id,attempt_id,report) VALUES(?,?,?)", p.ID, "old-pass", string(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveSkillProposal(ctx, p.ID, target, "old-pass"); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("old or injected pass flag enabled approval: %v", err)
	}
	if _, err := s.db.Exec("UPDATE personal_skill_versions SET approved_at=? WHERE skill_id=? AND version=?", now(), p.Skill.SkillID, p.Skill.Version); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateSkill(ctx, p.Skill.SkillID, target.Version, target.Revision, target.Digest); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("historical weak evidence reactivated: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	text, err := applicableSkillInstructions(tx, p.Skill.SkillID, p.Skill.Version, goal)
	if err != nil || text != base.Instructions {
		t.Fatalf("old unproven body used in runtime: %q, %v", text, err)
	}
}

func TestSkillProposalRejectsCorruptServiceRecord(t *testing.T) {
	const scope, goal = "project-q", "summarize POD7"
	s, _, _, eval, _ := rsiFixture(t, scope, goal, "")
	if _, err := s.db.Exec("UPDATE skill_evaluation_runs SET report_json=report_json||' ' WHERE id=?", eval.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProposeSkillWithOptions(t.Context(), scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{EvaluationRunID: eval.ID}); !errors.Is(err, ErrSkillProposal) {
		t.Fatalf("tampered service evidence not rejected: %v", err)
	}
}

func TestSkillReplayPendingAttemptSupersedesEarlierEvidence(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "project-private", "summarize station"
	s, base, _, eval, _ := rsiFixture(t, scope, goal, "read source")
	p := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{})
	target := SkillProposalVersion{Version: p.Skill.Version, Revision: p.Skill.Revision, Digest: p.Skill.Digest}
	suite := rsiFixtureHoldout(scope, goal)
	count := 0
	old, err := s.ReplaySkillProposal(ctx, p.ID, target, suite, rsiFixtureModel, replayFixtureProvider(t, suite, base.Instructions, p.Skill.Instructions, "improved", &count))
	if err != nil || !old.Passed {
		t.Fatalf("test needs earlier passing attempt: %v", err)
	}
	newAttempt, _, err := s.beginSkillReplay(ctx, p.ID, target, suite, rsiFixtureModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveSkillReplay(ctx, p.ID, old); !errors.Is(err, ErrSkillReplayRequired) {
		t.Fatalf("late completion overwrote a new attempt: %v", err)
	}
	for _, replayID := range []string{old.ID, newAttempt.ID} {
		if err := s.ApproveSkillProposal(ctx, p.ID, target, replayID); !errors.Is(err, ErrSkillReplayRequired) {
			t.Fatalf("unfinished/old attempt approved (%s): %v", replayID, err)
		}
	}
}

func TestServerEvaluationGatesEveryTrainingCase(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "训练范围", "总结长期巡检报告"
	s, _, _, _, _ := rsiFixture(t, scope, goal, "")
	for _, scenario := range []string{"regression", "forbidden", "not_measured"} {
		t.Run(scenario, func(t *testing.T) {
			training := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
				{ID: "improvement", Scope: scope, Goal: goal, Prompt: "一条真实改善样例", Required: []string{"正确"}, Forbidden: []string{"误复用"}},
				{ID: "protected", Scope: scope, Goal: goal, Prompt: "一条单独的退步检查", Required: []string{"正确"}, Forbidden: []string{"误复用"}},
			}}
			if scenario == "not_measured" {
				training.Cases[1].Forbidden = nil
			}
			p := testProvider(func(_ context.Context, in []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
				gotCandidate := strings.Contains(in[0].Text, "经审阅")
				if strings.Contains(in[0].Text, "改善样例") {
					if gotCandidate {
						return nil, emit("正确")
					}
					return nil, emit("没有达到要求")
				}
				if gotCandidate && scenario == "regression" {
					return nil, emit("没有达到要求")
				}
				if scenario == "forbidden" {
					// The bad behavior is already present in the baselines;
					// a candidate cannot inherit it even with no aggregate rise.
					return nil, emit("正确 误复用")
				}
				return nil, emit("正确")
			})
			run, err := s.RunExperienceEvaluation(ctx, training, rsiFixtureModel, p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{EvaluationRunID: run.ID}); !errors.Is(err, ErrSkillProposal) {
				t.Fatalf("one gain masked an unsafe or unmeasured case: %v", err)
			}
		})
	}
}

func TestSkillReplayWithoutCurrentSkillUsesSinglePlainControl(t *testing.T) {
	ctx := t.Context()
	const scope, goal = "新范围", "总结多节点配置收集"
	s, _, _, eval, _ := rsiFixture(t, scope, goal, "")
	p := proposeFixtureSkill(t, s, scope, goal, eval, SkillProposalOptions{})
	target := SkillProposalVersion{Version: p.Skill.Version, Revision: p.Skill.Revision, Digest: p.Skill.Digest}
	h := rsiFixtureHoldout(scope, goal)
	calls := 0
	r, err := s.ReplaySkillProposal(ctx, p.ID, target, h, rsiFixtureModel, replayFixtureProvider(t, h, "", p.Skill.Instructions, "improved", &calls))
	if err != nil || !r.Passed || calls != 4 || r.Metrics[skillReplayArmBase].Successful != r.Metrics[skillReplayArmPlain].Successful {
		t.Fatalf("a missing current Skill became an extra sample: %+v, %d, %v", r, calls, err)
	}
	for _, c := range r.Cases {
		if c.BaseHasSkill || c.BaseDigest != digest("") {
			t.Fatalf("alias provenance missing: %+v", c)
		}
	}
	if err := s.ApproveSkillProposal(ctx, p.ID, target, r.ID); err != nil {
		t.Fatal(err)
	}
}
