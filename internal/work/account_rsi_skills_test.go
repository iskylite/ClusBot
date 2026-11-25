package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateRSISkillsStayWithAccountAndRevokeSharedBase(t *testing.T) {
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := Open(filepath.Join(root, "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	account := func(principal string) *Store {
		t.Helper()
		s, err := OpenAccountBusinessStore(filepath.Join(t.TempDir(), "account.db"), instance)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.BindAccountBusinessStore(ctx, principal); err != nil {
			t.Fatal(err)
		}
		return s
	}
	alice, bob := account("user:alice"), account("user:bob")
	const scope, goal = "集群巡检", "检查集群健康"
	const sharedText = "共享准则：使用只读能力。"
	shared, err := instance.DraftSkill(ctx, scope, sharedText, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.ActivateSkill(ctx, shared.SkillID, shared.Version, shared.Revision, shared.Digest); err != nil {
		t.Fatal(err)
	}
	newRun := func(s *Store, key, taskGoal string) (Run, *SkillVersion) {
		t.Helper()
		run, err := s.Submit(ctx, key, taskGoal, scope, "", true)
		if err != nil {
			t.Fatal(err)
		}
		skill, err := s.skillForRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		return run, skill
	}
	const privateCue = "独立核对历史版本来源"
	r, err := alice.Submit(ctx, "checked-source", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := alice.claim(ctx)
	if err != nil || claimed.ID != r.ID {
		t.Fatalf("claim source: %+v %v", claimed, err)
	}
	if err := alice.delta(ctx, claimed.ID, "# "+privateCue); err != nil {
		t.Fatal(err)
	}
	if err := alice.finish(claimed, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := alice.Detail(r.TaskID, true)
	if err != nil || detail.Artifact == nil {
		t.Fatalf("source artifact: %+v %v", detail, err)
	}
	if err := alice.Accept(ctx, r.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	candidates, err := alice.ExperienceCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates: %+v %v", candidates, err)
	}
	if _, err := alice.ReviewExperience(ctx, candidates[0].ID, candidates[0].Revision, "approved"); err != nil {
		t.Fatal(err)
	}
	cfg := ExperienceEvaluationConfig{Model: "fixture", ModelBinding: "model-binding", MaxOutput: 128, RequestTimeout: time.Second}
	evalSuite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "train-1", Goal: goal, Scope: scope, Prompt: "训练：报告不应抄写上一次的值。", Required: []string{"只按当次证据"}, Forbidden: []string{"旧值"}},
		{ID: "train-2", Goal: goal, Scope: scope, Prompt: "训练：复用上次的检查方法。", Required: []string{"只按当次证据"}, Forbidden: []string{"旧值"}},
	}}
	eval, err := alice.RunExperienceEvaluation(ctx, evalSuite, cfg, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		if strings.Contains(input[0].Text, "经审阅") {
			return nil, emit("只按当次证据")
		}
		return nil, emit("无法核对")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{EvaluationRunID: eval.ID}); !errors.Is(err, ErrSkillProposal) {
		t.Fatalf("other account could reference a private evaluation: %v", err)
	}
	proposed, err := alice.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{EvaluationRunID: eval.ID})
	if err != nil || proposed.Skill.Active || proposed.Skill.SkillID == shared.SkillID || proposed.Skill.Version != 1 || proposed.BaseVersion != nil {
		t.Fatalf("private draft changed or reused shared identity: %+v %v", proposed, err)
	}
	if _, got := newRun(alice, "before-review", goal); got == nil || got.SkillID != shared.SkillID {
		t.Fatalf("draft was used before human confirmation: %+v", got)
	}
	target := SkillProposalVersion{Version: proposed.Skill.Version, Revision: proposed.Skill.Revision, Digest: proposed.Skill.Digest}
	replaySuite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "holdout-1", Goal: goal, Scope: scope, Prompt: "全新材料场景：检查结果能否独立验证。", Required: []string{"独立核对"}, Forbidden: []string{"旧值"}},
		{ID: "holdout-2", Goal: goal, Scope: scope, Prompt: "另一全新材料场景：条件不足时如何说明。", Required: []string{"条件说明"}, Forbidden: []string{"旧值"}},
	}}
	replay, err := alice.ReplaySkillProposal(ctx, proposed.ID, target, replaySuite, cfg, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		skill := input[0].Skill
		if skill == nil {
			return nil, emit("条件说明")
		}
		if !strings.Contains(skill.Instructions, sharedText) {
			t.Fatalf("the approved shared baseline is missing from one arm: %+v", skill)
		}
		if strings.Contains(skill.Instructions, privateCue) {
			return nil, emit("独立核对，条件说明")
		}
		return nil, emit("条件说明")
	}))
	if err != nil || !replay.Passed {
		t.Fatalf("private proposal did not replay actual bodies: %+v %v", replay, err)
	}
	if got, err := bob.SkillProposals(ctx); err != nil || len(got) != 0 {
		t.Fatalf("private proposals leaked between accounts: %+v %v", got, err)
	}
	if err := bob.ApproveSkillProposal(ctx, proposed.ID, target, replay.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other account approved private proposal: %v", err)
	}
	if err := alice.ApproveSkillProposal(ctx, proposed.ID, target, replay.ID); err != nil {
		t.Fatal(err)
	}
	if err := bob.RollbackSkillProposal(ctx, proposed.ID, SkillProposalVersion{Version: 1, Revision: target.Revision + 1, Digest: target.Digest}, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other account rolled back private proposal: %v", err)
	}
	aliceRun, aliceSkill := newRun(alice, "approved-owner", goal)
	if aliceSkill == nil || aliceSkill.SkillID != proposed.Skill.SkillID || !strings.Contains(aliceSkill.Instructions, privateCue) {
		t.Fatalf("actual Worker did not select own approved private experience: %+v", aliceSkill)
	}
	for _, task := range []struct {
		s         *Store
		key, goal string
	}{
		{bob, "no-leak", goal},
		{alice, "different-goal", "核对库存状态"},
	} {
		if _, got := newRun(task.s, task.key, task.goal); got == nil || got.SkillID != shared.SkillID || got.Instructions != sharedText {
			t.Fatalf("private default crossed account or goal %s: %+v", task.key, got)
		}
	}
	if versions, err := instance.Skills(ctx); err != nil || len(versions) != 1 || versions[0].SkillID != shared.SkillID || !versions[0].Active {
		t.Fatalf("private version escaped to instance catalog: %+v %v", versions, err)
	}
	// A second improvement has its own evidence and holdout. Rollback
	// atomically restores the approved private predecessor, never the shared
	// row and never a draft. A stale repeat cannot revert another version.
	correction, err := alice.Submit(ctx, "new-correction", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := alice.db.QueryRowContext(ctx, "SELECT seq FROM messages WHERE run_id=? AND role='user'", correction.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	newCandidate, err := alice.CaptureExperienceCorrection(ctx, seq, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.ReviewExperience(ctx, newCandidate.ID, newCandidate.Revision, "approved"); err != nil {
		t.Fatal(err)
	}
	evalSuite.Cases[0].Prompt = "新的训练资料规则说明，与第一轮不复用"
	secondEval, err := alice.RunExperienceEvaluation(ctx, evalSuite, cfg, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		if strings.Contains(input[0].Text, "经审阅") {
			return nil, emit("只按当次证据")
		}
		return nil, emit("无法核对")
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := alice.ProposeSkillWithOptions(ctx, scope, goal, ExperienceEvaluationReport{}, SkillProposalOptions{EvaluationRunID: secondEval.ID})
	if err != nil || second.Skill.Version != 2 || second.BaseVersion == nil || *second.BaseVersion != 1 || second.Skill.SkillID != proposed.Skill.SkillID {
		t.Fatalf("second private proposal has wrong predecessor: %+v %v", second, err)
	}
	secondTarget := SkillProposalVersion{Version: second.Skill.Version, Revision: second.Skill.Revision, Digest: second.Skill.Digest}
	replaySuite.Cases[0].Prompt = "独立的第二次留出输入，不引用历史内容"
	replaySuite.Cases[1].Prompt = "独立的第三类留出输入，关注说明"
	secondReplay, err := alice.ReplaySkillProposal(ctx, second.ID, secondTarget, replaySuite, cfg, testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		if skill := input[0].Skill; skill != nil && strings.Contains(skill.Instructions, "用户纠正原文") {
			return nil, emit("独立核对，条件说明")
		}
		return nil, emit("条件说明")
	}))
	if err != nil || !secondReplay.Passed {
		t.Fatalf("second private replay: %+v %v", secondReplay, err)
	}
	if err := alice.ApproveSkillProposal(ctx, second.ID, secondTarget, secondReplay.ID); err != nil {
		t.Fatal(err)
	}
	secondTarget.Revision++
	if err := alice.RollbackSkillProposal(ctx, second.ID, secondTarget, false); err != nil {
		t.Fatal(err)
	}
	if _, got := newRun(alice, "after-rollback", goal); got == nil || got.SkillID != proposed.Skill.SkillID || got.Version != 1 {
		t.Fatalf("rollback did not select previously approved private method: %+v", got)
	}
	if err := alice.RollbackSkillProposal(ctx, second.ID, secondTarget, false); !errors.Is(err, ErrTaskRevision) {
		t.Fatalf("stale rollback unexpectedly authorized: %v", err)
	}
	if err := instance.DisableSkill(ctx, shared.SkillID, shared.Revision+1); err != nil {
		t.Fatal(err)
	}
	if versions, err := alice.SkillProposals(ctx); err != nil || len(versions) != 2 {
		t.Fatalf("account private proposals not readable: %+v %v", versions, err)
	} else {
		for _, v := range versions {
			if v.Status != "unavailable" || v.Skill.Instructions != "" {
				t.Fatalf("a revoked base remained readable: %+v", v)
			}
		}
	}
	if _, err := alice.skillForRun(ctx, aliceRun); !errors.Is(err, ErrReadCheckpoint) {
		t.Fatalf("in-flight private binding survived revoked shared base: %v", err)
	}
	if err := alice.RollbackSkillProposal(ctx, proposed.ID, SkillProposalVersion{Version: 1, Revision: target.Revision + 3, Digest: target.Digest}, true); err != nil {
		t.Fatalf("owner could not disable unavailable private method: %v", err)
	}
	if _, got := newRun(alice, "no-revoked-base", goal); got != nil {
		t.Fatalf("disabled private or revoked shared text loaded: %+v", got)
	}
}
