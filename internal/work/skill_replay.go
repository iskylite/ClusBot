package work

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	skillReplayArmPlain    = "none"
	skillReplayArmBase     = "base_skill"
	skillReplayArmProposed = "proposed_skill"
	skillReplayContract    = "isolated_holdout_paired"
	maxSkillReplayCases    = 12
	maxSkillReplayBytes    = 96 << 10
)

var skillReplayArms = [...]string{skillReplayArmPlain, skillReplayArmBase, skillReplayArmProposed}

var ErrSkillReplayRequired = errors.New("请先通过独立留出用例的 Skill 正文重放，并确认该次结果")
var ErrSkillReplayInput = errors.New("重放需要与三臂开发评测独立、与提案范围和目标一致的留出用例")

type SkillProposalVersion struct {
	Version  int    `json:"version"`
	Revision int    `json:"revision"`
	Digest   string `json:"sha256"`
}

// Persist bounded metadata and verdicts, not raw prompts, assertion text,
// model replies or copied historical task material.
type SkillReplayCaseProof struct {
	ID               string `json:"id"`
	Scope            string `json:"scope_label"`
	GoalDigest       string `json:"goal_sha256"`
	PromptDigest     string `json:"prompt_sha256"`
	AssertionsDigest string `json:"assertions_sha256"`
	TaskID           string `json:"source_task_id,omitempty"`
	BaseDigest       string `json:"base_sha256"`
	BaseHasSkill     bool   `json:"base_has_skill"`
	CandidateDigest  string `json:"effective_candidate_sha256"`
}

type SkillReplayReport struct {
	ID              string                                 `json:"id"`
	Contract        string                                 `json:"contract,omitempty"`
	Target          SkillProposalVersion                   `json:"target"`
	TrainingRunID   string                                 `json:"training_evaluation_run_id,omitempty"`
	TrainingDigest  string                                 `json:"training_suite_sha256,omitempty"`
	SnapshotDigest  string                                 `json:"replay_snapshot_sha256,omitempty"`
	BaseDigest      string                                 `json:"base_sha256"`
	CandidateDigest string                                 `json:"effective_candidate_sha256"`
	SuiteDigest     string                                 `json:"suite_sha256"`
	Scoring         string                                 `json:"scoring,omitempty"`
	Tools           string                                 `json:"tool_environment,omitempty"`
	Model           ExperienceEvaluationConfig             `json:"model_config"`
	TimeoutMS       int64                                  `json:"request_timeout_ms"`
	Cases           []SkillReplayCaseProof                 `json:"case_proofs,omitempty"`
	Metrics         map[string]ExperienceEvaluationMetrics `json:"metrics"`
	Results         []ExperienceEvaluationResult           `json:"results"`
	Gains           []string                               `json:"gains,omitempty"`
	Regressions     []string                               `json:"regressions,omitempty"`
	Incomplete      []string                               `json:"incomplete,omitempty"`
	Passed          bool                                   `json:"passed"`
	CompletedAt     int64                                  `json:"completed_at"`
}

type frozenSkillReplayCase struct {
	Case     ExperienceEvaluationCase
	Versions [len(skillReplayArms)]*SkillVersion
}

// ReplaySkillProposal freezes a disjoint holdout and its case-specific
// effective Skill bodies before the first model call. A retry may rerun the
// same holdout only; it cannot select friendlier cases after seeing a result.
func (s *Store) ReplaySkillProposal(ctx context.Context, proposalID string, target SkillProposalVersion, suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig, provider Provider) (SkillReplayReport, error) {
	if len(suite.Cases) < 2 || len(suite.Cases) > maxSkillReplayCases || provider == nil || len(cfg.Model) > 200 || len(cfg.ModelBinding) > 128 {
		return SkillReplayReport{}, fmt.Errorf("%w：请另行提供 2 至 %d 条用例及可用模型", ErrSkillReplayInput, maxSkillReplayCases)
	}
	if err := validateEvaluation(suite, cfg); err != nil {
		return SkillReplayReport{}, fmt.Errorf("%w：%v", ErrSkillReplayInput, err)
	}
	report, cases, err := s.beginSkillReplay(ctx, proposalID, target, suite, cfg)
	if err != nil {
		return SkillReplayReport{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	for index, frozen := range cases {
		c := frozen.Case
		pair := make([]ExperienceEvaluationResult, len(skillReplayArms))
		for offset := range skillReplayArms {
			arm := (index + offset) % len(skillReplayArms)
			if err := ctx.Err(); err != nil {
				return SkillReplayReport{}, err
			}
			// With no applicable baseline Skill, reuse the identical plain
			// control. Independent samples of identical inputs are not evidence
			// that the "current" method was better or worse.
			if arm == 1 && frozen.Versions[1] == nil {
				continue
			}
			input := Message{Role: "user",
				Text:  c.Prompt + "\n\n本次任务范围：\n" + c.Scope + "\n\n本次任务目标：\n" + c.Goal,
				Skill: frozen.Versions[arm]}
			result := ExperienceEvaluationResult{CaseID: c.ID, Policy: skillReplayArms[arm], Status: "not_run"}
			release, err := s.acquireModelSlot(runCtx)
			if err == nil {
				result.Status = "completed"
				result = evaluateTaskOutput(runCtx, provider, cfg, c, input, result)
				release()
			} else if !errors.Is(err, context.DeadlineExceeded) {
				return SkillReplayReport{}, err
			}
			if ctx.Err() != nil {
				return SkillReplayReport{}, ctx.Err()
			}
			result.RequiredMisses = len(result.MissingRequired)
			result.ForbiddenMatches = len(result.ForbiddenHits)
			result.Answer, result.MissingRequired, result.ForbiddenHits = "", nil, nil
			result.InputTokens = replayUsageCount(result.InputTokens)
			result.OutputTokens = replayUsageCount(result.OutputTokens)
			pair[arm] = result
		}
		if frozen.Versions[1] == nil {
			pair[1] = pair[0]
			pair[1].Policy = skillReplayArmBase
		}
		report.Results = append(report.Results, pair...)
	}
	report.Metrics, report.Gains, report.Regressions, report.Incomplete, report.Passed = scoreSkillReplay(report)
	report.CompletedAt = now()
	if err := s.saveSkillReplay(ctx, proposalID, report); err != nil {
		return SkillReplayReport{}, err
	}
	return report, nil
}

func replayUsageCount(n *int64) *int64 {
	if n != nil && (*n < 0 || *n > 1_000_000_000) {
		return nil
	}
	return n
}

func checkSkillReplayTarget(p SkillProposal, target SkillProposalVersion) error {
	if p.Status == "unavailable" {
		return ErrExperienceSource
	}
	if p.Status != "pending" || p.Skill.Version != target.Version || p.Skill.Revision != target.Revision {
		return ErrTaskRevision
	}
	if p.Skill.Digest != target.Digest || digest(p.Skill.Instructions) != target.Digest {
		return ErrState
	}
	return nil
}

func (s *Store) beginSkillReplay(ctx context.Context, proposalID string, target SkillProposalVersion, suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig) (SkillReplayReport, []frozenSkillReplayCase, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	defer tx.Rollback()
	p, err := readSkillProposal(tx, proposalID)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	if err := checkSkillReplayTarget(p, target); err != nil {
		return SkillReplayReport{}, nil, err
	}
	if p.MatchMode != SkillMatchGoalExact || len(p.MatchKeywords) != 0 || p.EvaluationRunID == "" {
		return SkillReplayReport{}, nil, fmt.Errorf("%w：旧提案缺少可信开发证据或适用范围过宽，请重新评测和起草", ErrSkillReplayInput)
	}
	training, err := loadTrustedSkillEvaluation(ctx, tx, p.EvaluationRunID)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	if training.ReportDigest != p.ReportDigest || training.SuiteDigest != p.SuiteDigest || training.Report.Snapshot != p.SnapshotDigest {
		return SkillReplayReport{}, nil, ErrSkillReplayInput
	}
	if _, sources, err := skillProposalEvidence(training.Report, p.Skill.Scope, p.Goal); err != nil || !slices.Equal(sources, p.Sources) {
		return SkillReplayReport{}, nil, fmt.Errorf("%w：开发评测未通过当前的配对及来源规则，请重新生成提案", ErrSkillReplayInput)
	}
	_, suiteHash, err := computeSuiteDigest(suite)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	if p.Replay != nil && p.Replay.Contract == skillReplayContract && p.Replay.SuiteDigest != suiteHash {
		return SkillReplayReport{}, nil, fmt.Errorf("%w：本提案已冻结独立留出集，重试必须使用同一套用例", ErrSkillReplayInput)
	}
	used, err := replayExcludedTasks(tx, p, training)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	for _, taskID := range suite.HoldoutTasks {
		if taskID == "" || len(taskID) > 128 || strings.TrimSpace(taskID) != taskID || used[taskID] {
			return SkillReplayReport{}, nil, fmt.Errorf("%w：留出任务编号无效或已用于开发", ErrSkillReplayInput)
		}
	}
	inputs := make(map[string]bool, len(training.Suite.Cases)+len(suite.Cases))
	for _, c := range training.Suite.Cases {
		inputs[digest(normalizedReplayPrompt(c.Prompt))] = true
	}
	report := SkillReplayReport{
		ID: id(), Contract: skillReplayContract, Target: target, TrainingRunID: p.EvaluationRunID,
		TrainingDigest: training.SuiteDigest, SuiteDigest: suiteHash, Tools: "disabled",
		Scoring: evaluationScoringVersion(suite), Model: cfg, TimeoutMS: cfg.RequestTimeout.Milliseconds(),
		Metrics: make(map[string]ExperienceEvaluationMetrics), Results: []ExperienceEvaluationResult{},
	}
	cases := make([]frozenSkillReplayCase, 0, len(suite.Cases))
	usedCaseTasks := make(map[string]bool)
	for _, c := range suite.Cases {
		c.Scope, c.Goal = strings.TrimSpace(c.Scope), strings.TrimSpace(c.Goal)
		if c.Scope != p.Skill.Scope || !skillProposalMatchesGoal(p.MatchMode, p.MatchKeywords, p.Goal, c.Goal) {
			return SkillReplayReport{}, nil, fmt.Errorf("%w：用例 %s 的实际范围或目标不同", ErrSkillReplayInput, c.ID)
		}
		if len(c.Forbidden) == 0 && len(c.ForbiddenRegex) == 0 {
			return SkillReplayReport{}, nil, fmt.Errorf("%w：用例 %s 缺少误复用断言", ErrSkillReplayInput, c.ID)
		}
		key := digest(normalizedReplayPrompt(c.Prompt))
		if inputs[key] {
			return SkillReplayReport{}, nil, fmt.Errorf("%w：用例 %s 复用了开发提示或本留出集中的提示", ErrSkillReplayInput, c.ID)
		}
		inputs[key] = true
		if c.TaskID != "" {
			if used[c.TaskID] || usedCaseTasks[c.TaskID] || !replayTaskMatches(tx, c.TaskID, c.Scope, c.Goal) {
				return SkillReplayReport{}, nil, fmt.Errorf("%w：用例 %s 的来源任务不可用、被重复引用或已用于开发", ErrSkillReplayInput, c.ID)
			}
			usedCaseTasks[c.TaskID] = true
		}
		base, candidate, err := skillReplayBodies(tx, p, c.Goal)
		if err != nil {
			return SkillReplayReport{}, nil, err
		}
		proof := SkillReplayCaseProof{
			ID: c.ID, Scope: c.Scope, GoalDigest: replayGoalDigest(c.Goal),
			PromptDigest: key, AssertionsDigest: replayAssertionsDigest(c), TaskID: c.TaskID,
			BaseDigest: digest(base), BaseHasSkill: base != "", CandidateDigest: digest(candidate),
		}
		report.Cases = append(report.Cases, proof)
		frozen := frozenSkillReplayCase{Case: c}
		if base != "" {
			baseVersion := 0 // first account-private proposal may use an ordinary shared base
			if p.BaseVersion != nil {
				baseVersion = *p.BaseVersion
			}
			frozen.Versions[1] = &SkillVersion{SkillID: p.Skill.SkillID, Scope: c.Scope, Version: baseVersion, Instructions: base, Digest: proof.BaseDigest}
		}
		frozen.Versions[2] = &SkillVersion{SkillID: p.Skill.SkillID, Scope: c.Scope, Version: p.Skill.Version, Instructions: candidate, Digest: proof.CandidateDigest}
		cases = append(cases, frozen)
	}
	report.BaseDigest, report.CandidateDigest = replayArmDigests(report.Cases)
	report.SnapshotDigest = replayFrozenDigest(report)
	encoded, err := encodeSkillReplay(report)
	if err != nil {
		return SkillReplayReport{}, nil, err
	}
	// Supersede the previous result before calling any model. A durable
	// incomplete receipt is never a pass, including across process restart.
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_proposal_replays(proposal_id,attempt_id,report) VALUES(?,?,?)
ON CONFLICT(proposal_id) DO UPDATE SET attempt_id=excluded.attempt_id,report=excluded.report`, proposalID, report.ID, encoded); err != nil {
		return SkillReplayReport{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return SkillReplayReport{}, nil, err
	}
	return report, cases, nil
}

func normalizedReplayPrompt(text string) string {
	// Case, punctuation, and whitespace edits must not turn a development
	// fixture into a holdout. This detects recognizable copies, not semantic
	// duplicates or a user's manually paraphrased task.
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			return ' '
		}
		return unicode.ToLower(r)
	}, text)), " ")
}

func replayGoalDigest(goal string) string { return digest(strings.ToLower(strings.TrimSpace(goal))) }

func replayAssertionsDigest(c ExperienceEvaluationCase) string {
	c.ID, c.Prompt, c.Scope, c.Goal, c.TaskID = "", "", "", "", ""
	return digest(toJSON(c))
}

func replayTaskMatches(tx *sql.Tx, taskID, scope, goal string) bool {
	if len(taskID) > 128 || strings.TrimSpace(taskID) != taskID {
		return false
	}
	var gotScope, gotGoal string
	err := tx.QueryRow(`SELECT t.scope_label,t.goal FROM tasks t
JOIN conversations c ON c.id=t.conversation_id WHERE t.id=? AND c.archived=0`, taskID).
		Scan(&gotScope, &gotGoal)
	return err == nil && strings.TrimSpace(gotScope) == scope && replayGoalDigest(gotGoal) == replayGoalDigest(goal)
}

// Development includes all training fixtures, all picked current and candidate
// examples, and ancestors whose guidance is possibly present in either arm.
func replayExcludedTasks(tx *sql.Tx, p SkillProposal, training trustedSkillEvaluation) (map[string]bool, error) {
	used := make(map[string]bool)
	for _, task := range training.Suite.HoldoutTasks {
		used[task] = true
	}
	for _, c := range training.Suite.Cases {
		if c.TaskID != "" {
			used[c.TaskID] = true
		}
	}
	add := func(sources []ExperienceEvaluationSource) error {
		for _, src := range sources {
			task := src.TaskID
			if task == "" && src.ArtifactID != "" {
				if err := tx.QueryRow("SELECT task_id FROM artifacts WHERE id=?", src.ArtifactID).Scan(&task); err != nil {
					return ErrExperienceSource
				}
			}
			if task == "" {
				return ErrExperienceSource
			}
			used[task] = true
		}
		return nil
	}
	for _, result := range training.Report.Results {
		if err := add(result.Sources); err != nil {
			return nil, err
		}
	}
	if err := add(p.Sources); err != nil {
		return nil, err
	}
	for v, depth := p.BaseVersion, 0; v != nil; depth++ {
		if depth >= 64 {
			return nil, ErrState
		}
		var raw string
		var parent *int
		err := tx.QueryRow("SELECT sources,base_version FROM skill_proposals WHERE skill_id=? AND version=?", p.Skill.SkillID, *v).Scan(&raw, &parent)
		if errors.Is(err, sql.ErrNoRows) {
			break // ordinary base without RSI sources
		}
		if err != nil {
			return nil, err
		}
		var sources []ExperienceEvaluationSource
		if json.Unmarshal([]byte(raw), &sources) != nil || parent != nil && *parent >= *v {
			return nil, ErrState
		}
		if err := add(sources); err != nil {
			return nil, err
		}
		v = parent
	}
	return used, nil
}

func skillReplayBodies(tx *sql.Tx, p SkillProposal, goal string) (base string, candidate string, err error) {
	if p.BaseVersion != nil {
		base, err = applicableSkillInstructions(tx, p.Skill.SkillID, *p.BaseVersion, goal)
	} else {
		base, err = rsiPrivateBaseInstructions(tx, p.Skill.SkillID)
	}
	if err != nil {
		return "", "", err
	}
	candidate, err = applicableSkillInstructions(tx, p.Skill.SkillID, p.Skill.Version, goal)
	if err != nil {
		return "", "", err
	}
	if candidate == "" || candidate == base || digest(candidate) != p.Skill.Digest {
		return "", "", fmt.Errorf("%w：正文在目标范围内无可核实的增量", ErrSkillReplayInput)
	}
	return base, candidate, nil
}

func replayArmDigests(cases []SkillReplayCaseProof) (base, candidate string) {
	baseList, candidateList := make([]string, 0, len(cases)), make([]string, 0, len(cases))
	for _, c := range cases {
		baseList, candidateList = append(baseList, c.BaseDigest), append(candidateList, c.CandidateDigest)
	}
	return digest(toJSON(baseList)), digest(toJSON(candidateList))
}

func replayFrozenDigest(r SkillReplayReport) string {
	return digest(toJSON([]any{r.Contract, r.Target, r.TrainingRunID, r.TrainingDigest, r.SuiteDigest, r.Scoring, r.Tools,
		r.Model, r.TimeoutMS, r.Cases, r.BaseDigest, r.CandidateDigest}))
}

func encodeSkillReplay(report SkillReplayReport) (string, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	if len(raw) > maxSkillReplayBytes {
		return "", fmt.Errorf("%w：重放回执超过预算", ErrSkillReplayInput)
	}
	return string(raw), nil
}

func (s *Store) saveSkillReplay(ctx context.Context, proposalID string, report SkillReplayReport) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readSkillProposal(tx, proposalID)
	if err != nil {
		return err
	}
	if err := checkSkillReplayTarget(p, report.Target); err != nil {
		return err
	}
	if p.Replay == nil || p.Replay.ID != report.ID || p.Replay.SnapshotDigest != report.SnapshotDigest || replayFrozenDigest(report) != report.SnapshotDigest {
		return ErrSkillReplayRequired
	}
	if err := checkReplayLiveDependencies(tx, p, report); err != nil {
		return err
	}
	encoded, err := encodeSkillReplay(report)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE skill_proposal_replays SET report=? WHERE proposal_id=? AND attempt_id=?", encoded, proposalID, report.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrSkillReplayRequired
	}
	return tx.Commit()
}

func checkReplayLiveDependencies(tx *sql.Tx, p SkillProposal, report SkillReplayReport) error {
	var rawDigest, suite, snapshot string
	err := tx.QueryRow("SELECT report_digest,suite_digest,snapshot_digest FROM skill_evaluation_runs WHERE id=?", p.EvaluationRunID).
		Scan(&rawDigest, &suite, &snapshot)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (rawDigest != p.ReportDigest || suite != report.TrainingDigest || snapshot != p.SnapshotDigest)) {
		return ErrSkillReplayRequired
	}
	if err != nil {
		return err
	}
	var base string
	if p.BaseVersion != nil {
		base, err = applicableSkillInstructions(tx, p.Skill.SkillID, *p.BaseVersion, p.Goal)
	} else {
		base, err = rsiPrivateBaseInstructions(tx, p.Skill.SkillID)
	}
	if err != nil {
		return err
	}
	for _, proof := range report.Cases {
		if proof.GoalDigest != replayGoalDigest(p.Goal) || proof.Scope != p.Skill.Scope ||
			proof.BaseDigest != digest(base) || proof.CandidateDigest != p.Skill.Digest {
			return ErrSkillReplayRequired
		}
		if proof.TaskID != "" && !replayTaskMatches(tx, proof.TaskID, p.Skill.Scope, p.Goal) {
			return ErrExperienceSource
		}
	}
	return nil
}

func replayDigestOK(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32
}

func scoreSkillReplay(report SkillReplayReport) (metrics map[string]ExperienceEvaluationMetrics, gains, regressions, incomplete []string, passed bool) {
	metrics = make(map[string]ExperienceEvaluationMetrics)
	pairs := make(map[string]map[string]ExperienceEvaluationResult, len(report.Cases))
	for _, c := range report.Cases {
		if c.ID == "" || pairs[c.ID] != nil {
			return metrics, nil, nil, []string{"invalid_cases"}, false
		}
		pairs[c.ID] = make(map[string]ExperienceEvaluationResult)
	}
	for _, result := range report.Results {
		pair := pairs[result.CaseID]
		if pair == nil || !slices.Contains(skillReplayArms[:], result.Policy) || pair[result.Policy].Policy != "" {
			return metrics, nil, nil, []string{"invalid_results"}, false
		}
		pair[result.Policy] = result
		metrics[result.Policy] = addEvaluationMetrics(metrics[result.Policy], result)
	}
	for _, c := range report.Cases {
		pair := pairs[c.ID]
		complete := true
		for _, arm := range skillReplayArms {
			r := pair[arm]
			if r.Status != "completed" || r.WrongReuse == nil ||
				*r.WrongReuse != (r.ForbiddenMatches > 0) || r.Success != (r.RequiredMisses == 0 && r.ForbiddenMatches == 0) ||
				r.RequiredMisses < 0 || r.ForbiddenMatches < 0 || r.Answer != "" || len(r.Sources)+len(r.MissingRequired)+len(r.ForbiddenHits) != 0 {
				complete = false
			}
		}
		if !complete {
			incomplete = append(incomplete, c.ID)
			continue
		}
		plain, base, proposed := pair[skillReplayArmPlain], pair[skillReplayArmBase], pair[skillReplayArmProposed]
		if !c.BaseHasSkill {
			plainControl := plain
			plainControl.Policy = skillReplayArmBase
			if toJSON(plainControl) != toJSON(base) {
				incomplete = append(incomplete, c.ID)
				continue
			}
		}
		if !proposed.Success || *proposed.WrongReuse {
			regressions = append(regressions, c.ID)
		}
		if proposed.Success && !plain.Success && !base.Success {
			gains = append(gains, c.ID)
		}
	}
	passed = len(report.Cases) >= 2 && len(report.Cases) <= maxSkillReplayCases &&
		len(report.Results) == len(report.Cases)*len(skillReplayArms) &&
		len(gains) > 0 && len(regressions) == 0 && len(incomplete) == 0
	return metrics, gains, regressions, incomplete, passed
}

func passesSkillReplayEvidence(r SkillReplayReport) bool {
	if r.Contract != skillReplayContract || r.Tools != "disabled" ||
		(r.Scoring != scoringLiteralV1 && r.Scoring != scoringStructuredV2) ||
		r.TrainingRunID == "" || r.CompletedAt <= 0 || r.ID == "" || !r.Passed ||
		!replayDigestOK(r.TrainingDigest) || !replayDigestOK(r.SuiteDigest) || r.SuiteDigest == r.TrainingDigest ||
		r.SnapshotDigest != replayFrozenDigest(r) || r.Model.MaxOutput < 16 || r.Model.MaxOutput > 4096 ||
		strings.TrimSpace(r.Model.Model) == "" || r.TimeoutMS < 1000 || r.TimeoutMS > 120000 {
		return false
	}
	base, proposed := replayArmDigests(r.Cases)
	if base != r.BaseDigest || proposed != r.CandidateDigest {
		return false
	}
	seen := map[string]bool{}
	for _, c := range r.Cases {
		if c.Scope == "" || !replayDigestOK(c.GoalDigest) || !replayDigestOK(c.PromptDigest) ||
			!replayDigestOK(c.AssertionsDigest) || !replayDigestOK(c.BaseDigest) ||
			c.CandidateDigest != r.Target.Digest || c.BaseDigest == c.CandidateDigest || seen[c.PromptDigest] ||
			c.BaseHasSkill != (c.BaseDigest != digest("")) {
			return false
		}
		seen[c.PromptDigest] = true
	}
	metrics, gains, regressions, incomplete, passed := scoreSkillReplay(r)
	return passed && toJSON(metrics) == toJSON(r.Metrics) && slices.Equal(gains, r.Gains) &&
		slices.Equal(regressions, r.Regressions) && slices.Equal(incomplete, r.Incomplete)
}

func storedPassedSkillReplay(tx *sql.Tx, proposalID, replayID string, target SkillProposalVersion) (SkillReplayReport, error) {
	var raw, attempt string
	err := tx.QueryRow("SELECT report,attempt_id FROM skill_proposal_replays WHERE proposal_id=?", proposalID).Scan(&raw, &attempt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (raw == "" || len(raw) > maxSkillReplayBytes || (replayID != "" && attempt != replayID))) {
		return SkillReplayReport{}, ErrSkillReplayRequired
	}
	if err != nil {
		return SkillReplayReport{}, err
	}
	var report SkillReplayReport
	if json.Unmarshal([]byte(raw), &report) != nil || report.ID != attempt || report.Target != target || !passesSkillReplayEvidence(report) {
		return SkillReplayReport{}, ErrSkillReplayRequired
	}
	return report, nil
}

func requirePassedSkillReplay(tx *sql.Tx, proposalID, replayID string, target SkillProposalVersion) error {
	if replayID == "" {
		return ErrSkillReplayRequired
	}
	return verifyPassedSkillReplay(tx, proposalID, replayID, target)
}

func verifyPassedSkillReplay(tx *sql.Tx, proposalID, replayID string, target SkillProposalVersion) error {
	report, err := storedPassedSkillReplay(tx, proposalID, replayID, target)
	if err != nil {
		return err
	}
	p, err := readSkillProposal(tx, proposalID)
	if err != nil {
		return err
	}
	if p.Status == "unavailable" {
		return ErrExperienceSource
	}
	if p.MatchMode != SkillMatchGoalExact || len(p.MatchKeywords) != 0 || p.EvaluationRunID != report.TrainingRunID || p.SuiteDigest != report.TrainingDigest {
		return ErrSkillReplayRequired
	}
	return checkReplayLiveDependencies(tx, p, report)
}
