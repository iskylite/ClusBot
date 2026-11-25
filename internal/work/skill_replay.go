package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	skillReplayArmBase     = "base_skill"
	skillReplayArmProposed = "proposed_skill"
)

var ErrSkillReplayRequired = errors.New("请先通过 Skill 正文重放，并确认该次评测结果")
var ErrSkillReplayInput = errors.New("重放用例须属于提案范围和目标，并包含错误复用断言")

type SkillProposalVersion struct {
	Version  int    `json:"version"`
	Revision int    `json:"revision"`
	Digest   string `json:"sha256"`
}

type SkillReplayReport struct {
	ID              string                                 `json:"id"`
	Target          SkillProposalVersion                   `json:"target"`
	BaseDigest      string                                 `json:"base_sha256"`
	CandidateDigest string                                 `json:"effective_candidate_sha256"`
	SuiteDigest     string                                 `json:"suite_sha256"`
	Model           ExperienceEvaluationConfig             `json:"model_config"`
	TimeoutMS       int64                                  `json:"request_timeout_ms"`
	Metrics         map[string]ExperienceEvaluationMetrics `json:"metrics"`
	Results         []ExperienceEvaluationResult           `json:"results"`
	Passed          bool                                   `json:"passed"`
	CompletedAt     int64                                  `json:"completed_at"`
}

// ReplaySkillProposal compares the same task inputs with the two effective
// Skill bodies. Only the replay report is written to the store.
func (s *Store) ReplaySkillProposal(ctx context.Context, proposalID string, target SkillProposalVersion, suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig, provider Provider) (SkillReplayReport, error) {
	if len(suite.Cases) == 0 {
		loaded, ok, err := s.loadProposalBoundSuite(ctx, proposalID)
		if err != nil {
			return SkillReplayReport{}, err
		}
		if ok {
			suite = loaded
		}
	}
	if err := validateEvaluation(suite, cfg); err != nil {
		return SkillReplayReport{}, fmt.Errorf("%w：%v", ErrSkillReplayInput, err)
	}
	report, versions, err := s.beginSkillReplay(ctx, proposalID, target, suite, cfg)
	if err != nil {
		return report, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	report.Passed = true
	for index, c := range suite.Cases {
		pair := make([]ExperienceEvaluationResult, len(versions))
		for offset := range versions {
			arm := (index + offset) % len(versions)
			policy := skillReplayArmBase
			if arm == 1 {
				policy = skillReplayArmProposed
			}
			input := Message{Role: "user", Text: c.Prompt + "\n\n本次任务目标：\n" + strings.TrimSpace(c.Goal), Skill: versions[arm]}
			result := ExperienceEvaluationResult{CaseID: c.ID, Policy: policy, Status: "not_run"}
			release, err := s.acquireModelSlot(runCtx)
			if err == nil {
				result.Status = "completed"
				result = evaluateTaskOutput(runCtx, provider, cfg, c, input, result)
				release()
			} else if !errors.Is(err, context.DeadlineExceeded) {
				return SkillReplayReport{}, err
			}
			if err := ctx.Err(); err != nil {
				return SkillReplayReport{}, err
			}
			result.Answer, result.MissingRequired, result.ForbiddenHits = "", nil, nil
			pair[arm] = result
			report.Results = append(report.Results, result)
			report.Metrics[policy] = addEvaluationMetrics(report.Metrics[policy], result)
		}
		baseRes, proposedRes := pair[0], pair[1]
		newWrongReuse := baseRes.WrongReuse != nil && proposedRes.WrongReuse != nil && !*baseRes.WrongReuse && *proposedRes.WrongReuse
		if baseRes.Status != "completed" || proposedRes.Status != "completed" || (baseRes.Success && !proposedRes.Success) || newWrongReuse {
			report.Passed = false
		}
	}
	report.Passed = report.Passed && report.Metrics[skillReplayArmProposed].Successful > 0
	report.CompletedAt = now()
	return report, s.saveSkillReplay(ctx, proposalID, report)
}

func (s *Store) loadProposalBoundSuite(ctx context.Context, proposalID string) (ExperienceEvaluationSuite, bool, error) {
	var suiteJSON string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(s.suite_json,'') FROM skill_proposals p
LEFT JOIN skill_evaluation_suites s ON s.suite_digest=p.suite_digest
WHERE p.id=? AND p.suite_digest<>''`, proposalID).Scan(&suiteJSON)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && suiteJSON == "") {
		return ExperienceEvaluationSuite{}, false, nil
	}
	if err != nil {
		return ExperienceEvaluationSuite{}, false, err
	}
	var suite ExperienceEvaluationSuite
	if err := json.Unmarshal([]byte(suiteJSON), &suite); err != nil {
		return ExperienceEvaluationSuite{}, false, err
	}
	return suite, true, nil
}

func (s *Store) beginSkillReplay(ctx context.Context, proposalID string, target SkillProposalVersion, suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig) (SkillReplayReport, [2]*SkillVersion, error) {
	var versions [2]*SkillVersion
	report := SkillReplayReport{
		ID: id(), Target: target, Model: cfg, TimeoutMS: cfg.RequestTimeout.Milliseconds(),
		Metrics: make(map[string]ExperienceEvaluationMetrics),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, versions, err
	}
	defer tx.Rollback()
	p, err := readSkillProposal(tx, proposalID)
	if err != nil {
		return report, versions, err
	}
	if err := checkSkillReplayTarget(p, target); err != nil {
		return report, versions, err
	}
	heldOut := make(map[string]bool)
	for _, taskID := range suite.HoldoutTasks {
		heldOut[taskID] = true
	}
	reuseChecked := false
	for _, c := range suite.Cases {
		if strings.TrimSpace(c.Scope) != p.Skill.Scope || !skillProposalMatchesGoal(p.MatchMode, p.MatchKeywords, p.Goal, c.Goal) {
			return report, versions, ErrSkillReplayInput
		}
		if c.TaskID != "" {
			heldOut[c.TaskID] = true
		}
		reuseChecked = reuseChecked || len(c.Forbidden) > 0 || len(c.ForbiddenRegex) > 0
	}
	if !reuseChecked {
		return report, versions, ErrSkillReplayInput
	}
	for _, source := range p.Sources {
		if heldOut[source.TaskID] {
			return report, versions, fmt.Errorf("%w：留出任务被用作提案经验来源", ErrSkillReplayInput)
		}
	}
	_, suiteHash, err := computeSuiteDigest(suite)
	if err != nil {
		return report, versions, err
	}
	if p.SuiteDigest != "" && p.SuiteDigest != suiteHash {
		return report, versions, fmt.Errorf("%w：重放用例须与评测阶段绑定的基准集一致", ErrSkillReplayInput)
	}
	report.SuiteDigest = suiteHash
	var base string
	if p.BaseVersion != nil {
		base, err = applicableSkillInstructions(tx, p.Skill.SkillID, *p.BaseVersion, p.Goal)
		if err != nil {
			return report, versions, err
		}
	}
	candidate, err := applicableSkillInstructions(tx, p.Skill.SkillID, p.Skill.Version, p.Goal)
	if err != nil {
		return report, versions, err
	}
	report.BaseDigest, report.CandidateDigest = digest(base), digest(candidate)
	if base != "" {
		versions[0] = &SkillVersion{SkillID: p.Skill.SkillID, Version: *p.BaseVersion, Instructions: base, Digest: report.BaseDigest}
	}
	versions[1] = &SkillVersion{SkillID: p.Skill.SkillID, Version: p.Skill.Version, Instructions: candidate, Digest: report.CandidateDigest}
	// A retry supersedes the previous report before any model calls.
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_proposal_replays(proposal_id,attempt_id) VALUES(?,?)
ON CONFLICT(proposal_id) DO UPDATE SET attempt_id=excluded.attempt_id,report=''`, proposalID, report.ID); err != nil {
		return report, versions, err
	}
	return report, versions, tx.Commit()
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
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE skill_proposal_replays SET report=? WHERE proposal_id=? AND attempt_id=?", string(encoded), proposalID, report.ID)
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

func requirePassedSkillReplay(tx *sql.Tx, proposalID, replayID string, target SkillProposalVersion) error {
	var raw string
	err := tx.QueryRow("SELECT report FROM skill_proposal_replays WHERE proposal_id=? AND attempt_id=?", proposalID, replayID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return ErrSkillReplayRequired
	}
	if err != nil {
		return err
	}
	var report SkillReplayReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return err
	}
	if !report.Passed || report.ID != replayID || report.Target != target {
		return ErrSkillReplayRequired
	}
	return nil
}
