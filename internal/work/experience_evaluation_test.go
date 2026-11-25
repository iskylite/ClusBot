package work

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExperienceEvaluationFreezesSourcesAndSeparatesPolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusbot.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	const goal, scope = "整理 Project A 本周周报", "项目 A"
	accept := func(key, body string) (Run, ExperienceCandidate) {
		t.Helper()
		r, err := s.Submit(ctx, key, goal, scope, "", true)
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.claim(ctx)
		if err != nil || run.ID != r.ID {
			t.Fatalf("claim: %+v, %v", run, err)
		}
		if err = s.delta(ctx, run.ID, body); err != nil {
			t.Fatal(err)
		}
		if err = s.finish(run, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		d, err := s.Detail(run.TaskID, true)
		if err != nil || d.Artifact == nil {
			t.Fatalf("result: %+v, %v", d, err)
		}
		if err = s.Accept(ctx, run.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
			t.Fatal(err)
		}
		candidate, err := s.CaptureAcceptedExperience(ctx, run.TaskID, d.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err = s.ReviewExperience(ctx, candidate.ID, candidate.Revision, "approved")
		if err != nil {
			t.Fatal(err)
		}
		return run, candidate
	}
	_, reference := accept("evaluation-source", "# 摘要\n历史工单 OLD-88")
	holdout, forbiddenReference := accept("evaluation-holdout", "# 保留样本\n不可读取 HOLDOUT-99")
	counts := func() [4]int {
		t.Helper()
		var counts [4]int
		if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM runs),
(SELECT count(*) FROM experience_candidates),(SELECT count(*) FROM experience_routes)`).
			Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
			t.Fatal(err)
		}
		return counts
	}
	before := counts()
	suite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "weekly", Prompt: "用本周材料生成周报，项目进展是完成测试。", Scope: scope, Goal: goal,
			TaskID: holdout.TaskID, Required: []string{"完成测试"}, Forbidden: []string{"OLD-88", "HOLDOUT-99"}},
		{ID: "other", Prompt: "整理另一范围的数据。", Scope: "项目 B", Goal: goal,
			Required: []string{"空缺"}, Forbidden: []string{"OLD-88"}},
	}}
	calls := 0
	provider := testProvider(func(_ context.Context, input []Message, task bool, limit int, emit func(string) error) (*TokenUsage, error) {
		calls++
		if len(input) != 1 || !task || limit != 256 || len(input[0].Memories) != 0 || input[0].Skill != nil ||
			input[0].Read != nil || len(input[0].Reads) != 0 || len(input[0].Materials) != 0 {
			t.Fatalf("evaluation included live state: %+v", input)
		}
		m := input[0]
		if strings.Contains(m.Text, "另一范围") {
			if m.Experience != nil || strings.Contains(m.Text, "经审阅") {
				t.Fatal("experience crossed scope")
			}
			return nil, errors.New("fixture: provider unavailable")
		}
		if strings.Contains(m.Text, "HOLDOUT-99") || (m.Experience != nil && m.Experience.ArtifactID == forbiddenReference.ArtifactID) {
			t.Fatal("held-out task entered a prompt")
		}
		answer := "完成测试"
		switch {
		case m.Experience != nil:
			if m.Experience.ArtifactID != reference.ArtifactID || !strings.Contains(m.Experience.Example, "摘要") || strings.Contains(m.Experience.Example, "OLD-88") {
				t.Fatalf("current policy leaked facts or chose the wrong source: %+v", m.Experience)
			}
			answer += " OLD-88"
		case strings.Contains(m.Text, "经审阅"):
			if !strings.Contains(m.Text, "摘要") || strings.Contains(m.Text, "OLD-88") {
				t.Fatalf("candidate policy did not isolate structure from facts: %s", m.Text)
			}
		}
		in, out := int64(20), int64(7)
		return &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit(answer)
	})
	cfg := ExperienceEvaluationConfig{Model: "fixture", MaxOutput: 256, RequestTimeout: time.Second}
	report, err := EvaluateExperience(ctx, path, suite, cfg, provider)
	if err != nil || calls != 6 || len(report.Results) != 6 || len(report.Snapshot) != 64 || report.Tools != "disabled" {
		t.Fatalf("incomplete evaluation: %+v, calls=%d, %v", report, calls, err)
	}
	if after := counts(); before != after {
		t.Fatalf("evaluation modified production state: %v -> %v", before, after)
	}
	for _, policy := range evaluationPolicies {
		m := report.Metrics[policy]
		want := 0.5
		if policy == acceptedExperiencePolicy {
			want = 0
		}
		if m.Cases != 2 || m.Completed != 1 || m.Incomplete != 1 || m.SuccessRate != want || m.WrongReuseChecked != 1 ||
			m.InputTokensKnown != 20 || m.OutputTokensKnown != 7 || m.InputUsageReported != 1 || m.OutputUsageReported != 1 {
			t.Fatalf("wrong denominators or usage for %s: %+v", policy, m)
		}
	}
	var actual []string
	for _, r := range report.Results {
		actual = append(actual, r.Policy)
		if r.CaseID == "weekly" && r.Policy == approvedCandidateExperiencePolicy &&
			(len(r.Sources) != 1 || r.Sources[0].CandidateID != reference.ID || r.Sources[0].CandidateRevision != reference.Revision) {
			t.Fatalf("missing candidate provenance: %+v", r)
		}
	}
	if !reflect.DeepEqual(actual, []string{"none", "accepted_experience", "approved_candidates", "accepted_experience", "approved_candidates", "none"}) ||
		report.Metrics["accepted_experience"].WrongReuseCount != 1 {
		t.Fatalf("unexpected order or wrong-reuse result: %v %+v", actual, report.Metrics)
	}
	if len(report.SuiteDigest) != 64 {
		t.Fatalf("missing suite_sha256 in report: %+v", report)
	}
	proposal, err := s.ProposeSkill(ctx, scope, goal, report)
	if err != nil || proposal.Status != "pending" || proposal.Skill.Active ||
		!reflect.DeepEqual(proposal.Evidence.Gains, []string{"weekly"}) ||
		len(proposal.Sources) != 1 || proposal.Sources[0].CandidateID != reference.ID {
		t.Fatalf("evaluation report did not yield a reviewable proposal: %+v, %v", proposal, err)
	}
	variant, err := snapshotEvaluationCases(ctx, path, ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{ID: "case-variant", Goal: strings.ToLower(goal), Scope: scope},
	}})
	if err != nil || len(variant) != 1 || len(variant[0].CandidateSources) != 0 {
		t.Fatalf("candidate evaluation crossed the Skill goal boundary: %+v, %v", variant, err)
	}
}

func TestStructuredEvaluationAndServerEvaluationRun(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "eval-v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	const goal, scope = "检查 POD7 节点状态", "集群巡检"
	suite := ExperienceEvaluationSuite{Cases: []ExperienceEvaluationCase{
		{
			ID:               "v2-case",
			Prompt:           "请输出 POD7 检查结论",
			Goal:             goal,
			Scope:            scope,
			Required:         []string{"结论"},
			RequiredHeadings: []string{"检查结论"},
			RequiredRegex:    []string{`POD7:\s+OK`},
			ForbiddenRegex:   []string{`OLD-\d+`},
			MaxChars:         200,
		},
	}}
	provider := testProvider(func(_ context.Context, input []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		in, out := int64(10), int64(5)
		return &TokenUsage{InputTokens: &in, OutputTokens: &out}, emit("# 检查结论\nPOD7: OK")
	})
	cfg := ExperienceEvaluationConfig{Model: "fixture", MaxOutput: 256, RequestTimeout: time.Second}
	run, err := s.RunExperienceEvaluation(ctx, suite, cfg, provider)
	if err != nil {
		t.Fatalf("RunExperienceEvaluation: %v", err)
	}
	if run.Report.Scoring != scoringStructuredV2 || len(run.SuiteDigest) != 64 || run.Report.Metrics["none"].Successful != 1 {
		t.Fatalf("unexpected v2 evaluation run: %+v", run)
	}
	runs, err := s.ExperienceEvaluations(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("ExperienceEvaluations: %+v, %v", runs, err)
	}
	// Verify regex forbidden failure.
	bad := evaluateTaskOutput(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		return nil, emit("# 检查结论\nPOD7: OK OLD-42")
	}), cfg, suite.Cases[0], Message{Role: "user", Text: "x"}, ExperienceEvaluationResult{CaseID: "v2-case", Status: "completed"})
	if bad.Success || bad.WrongReuse == nil || !*bad.WrongReuse {
		t.Fatalf("forbidden_regex did not mark wrong_reuse: %+v", bad)
	}
}
