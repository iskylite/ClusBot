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
	"unicode/utf8"
)

const (
	SkillMatchGoalExact     = "goal_exact"
	SkillMatchScopeKeywords = "scope_keywords"
	SkillMatchScopeAll      = "scope_all"
)

type SkillProposalOptions struct {
	EvaluationRunID string
	MatchMode       string
	MatchKeywords   []string
	Synthesize      bool
	Provider        Provider
	ModelConfig     ExperienceEvaluationConfig
}

var ErrSkillProposalConfirmation = errors.New("请从对应 Skill 提案确认生效")

var ErrSkillProposal = errors.New("需要同范围、同目标且有改善的隔离评测报告")

type SkillProposalEvidence struct {
	Gains               []string `json:"gains"`
	Regressions         []string `json:"regressions"`
	IncompletePairs     int      `json:"incomplete_pairs"`
	PairedCases         int      `json:"paired_cases"`
	NoneSuccessful      int      `json:"none_successful"`
	CurrentSuccessful   int      `json:"current_successful"`
	CandidateSuccessful int      `json:"candidate_successful"`
	CurrentWrongReuse   int      `json:"current_wrong_reuse"`
	CandidateWrongReuse int      `json:"candidate_wrong_reuse"`
	ReuseChecked        int      `json:"reuse_checked"`
	TokenDelta          int64    `json:"candidate_token_delta"`
	TokenPairs          int      `json:"token_pairs"`
}

type SkillProposal struct {
	ID               string                       `json:"id"`
	Skill            SkillVersion                 `json:"skill"`
	BaseVersion      *int                         `json:"base_version"`
	BaseDigest       string                       `json:"base_sha256,omitempty"`
	BaseInstructions string                       `json:"base_instructions,omitempty"`
	Goal             string                       `json:"goal"`
	ReportDigest     string                       `json:"report_sha256"`
	SnapshotDigest   string                       `json:"snapshot_sha256"`
	SuiteDigest      string                       `json:"suite_sha256,omitempty"`
	EvaluationRunID  string                       `json:"evaluation_run_id,omitempty"`
	MatchMode        string                       `json:"match_mode,omitempty"`
	MatchKeywords    []string                     `json:"match_keywords,omitempty"`
	DraftMode        string                       `json:"draft_mode,omitempty"`
	Model            string                       `json:"model"`
	Evidence         SkillProposalEvidence        `json:"evidence"`
	Sources          []ExperienceEvaluationSource `json:"sources"`
	Status           string                       `json:"status"`
	Replay           *SkillReplayReport           `json:"replay,omitempty"`
}

func skillProposalEvidence(report ExperienceEvaluationReport, scope, goal string) (SkillProposalEvidence, []ExperienceEvaluationSource, error) {
	var evidence SkillProposalEvidence
	candidatePolicyVer := report.PolicyVersions[approvedCandidateExperiencePolicy]
	if candidatePolicyVer == "" {
		candidatePolicyVer = report.PolicyVersions[legacyCandidateExperiencePolicy]
	}
	scoringOK := report.Scoring == scoringLiteralV1 || report.Scoring == scoringStructuredV2
	if report.Version != 1 || report.Tools != "disabled" || !scoringOK ||
		candidatePolicyVer != "approved_exact_goal/v1" ||
		len(report.CaseContexts) == 0 || len(report.CaseContexts) > 32 || len(report.Results) != len(report.CaseContexts)*3 ||
		strings.TrimSpace(report.Model.Model) == "" || len(report.Model.Model) > 200 {
		return evidence, nil, ErrSkillProposal
	}
	if digestBytes, err := hex.DecodeString(report.Snapshot); err != nil || len(digestBytes) != 32 {
		return evidence, nil, ErrSkillProposal
	}
	cases := make(map[string]ExperienceEvaluationCaseContext)
	pairs := make(map[string]map[string]ExperienceEvaluationResult)
	for _, item := range report.CaseContexts {
		if item.ID == "" || len(item.ID) > 80 || cases[item.ID].ID != "" {
			return evidence, nil, ErrSkillProposal
		}
		cases[item.ID] = item
		pairs[item.ID] = make(map[string]ExperienceEvaluationResult)
	}
	for _, result := range report.Results {
		policy := normalizeExperiencePolicy(result.Policy)
		pair, exists := pairs[result.CaseID]
		if !exists || !slices.Contains(evaluationPolicies[:], policy) || pair[policy].Policy != "" ||
			(result.Status == "completed" && result.Success != (len(result.MissingRequired) == 0 && len(result.ForbiddenHits) == 0)) ||
			(result.WrongReuse != nil && *result.WrongReuse != (len(result.ForbiddenHits) != 0)) ||
			(result.Success && result.Status != "completed") || len(result.Sources) > 3 {
			return evidence, nil, ErrSkillProposal
		}
		result.Policy = policy
		pair[policy] = result
	}
	var sources []ExperienceEvaluationSource
	seen := make(map[string]ExperienceEvaluationSource)
	for _, item := range report.CaseContexts {
		if item.Scope != scope || item.Goal != goal {
			continue
		}
		pair := pairs[item.ID]
		none, current, candidate := pair[noExperiencePolicy], pair[acceptedExperiencePolicy], pair[approvedCandidateExperiencePolicy]
		if none.Status != "completed" || current.Status != "completed" || candidate.Status != "completed" ||
			none.WrongReuse == nil || current.WrongReuse == nil || candidate.WrongReuse == nil {
			evidence.IncompletePairs++
			continue
		}
		evidence.PairedCases++
		if none.Success {
			evidence.NoneSuccessful++
		}
		if current.Success {
			evidence.CurrentSuccessful++
		}
		if candidate.Success {
			evidence.CandidateSuccessful++
		}
		if current.WrongReuse != nil && candidate.WrongReuse != nil {
			evidence.ReuseChecked++
			if *current.WrongReuse {
				evidence.CurrentWrongReuse++
			}
			if *candidate.WrongReuse {
				evidence.CandidateWrongReuse++
			}
		}
		if (!candidate.Success && (current.Success || none.Success)) || *candidate.WrongReuse {
			evidence.Regressions = append(evidence.Regressions, item.ID)
		}
		if current.InputTokens != nil && current.OutputTokens != nil && candidate.InputTokens != nil && candidate.OutputTokens != nil {
			evidence.TokenPairs++
			evidence.TokenDelta += *candidate.InputTokens + *candidate.OutputTokens - *current.InputTokens - *current.OutputTokens
		}
		if !candidate.Success || current.Success || none.Success || len(candidate.Sources) == 0 {
			continue
		}
		evidence.Gains = append(evidence.Gains, item.ID)
		for _, source := range candidate.Sources {
			if source.CandidateID == "" {
				return evidence, nil, ErrSkillProposal
			}
			if old, exists := seen[source.CandidateID]; exists {
				if old != source {
					return evidence, nil, ErrSkillProposal
				}
				continue
			}
			seen[source.CandidateID] = source
			sources = append(sources, source)
		}
	}
	if evidence.IncompletePairs > 0 {
		return evidence, nil, fmt.Errorf("%w：存在 %d 项未完整配对用例", ErrSkillProposal, evidence.IncompletePairs)
	}
	if len(evidence.Regressions) > 0 {
		return evidence, nil, fmt.Errorf("%w：候选策略在 %s 上存在退步", ErrSkillProposal, strings.Join(evidence.Regressions, "、"))
	}
	if len(evidence.Gains) == 0 {
		return evidence, nil, fmt.Errorf("%w：候选策略未在同一用例同时超过无经验和当前经验", ErrSkillProposal)
	}
	return evidence, sources, nil
}

func proposalCandidate(tx *sql.Tx, source ExperienceEvaluationSource) (ExperienceCandidate, error) {
	item, err := scanExperienceCandidate(tx.QueryRow(candidateSelect+" WHERE e.id=?", source.CandidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrExperienceSource
	}
	if err != nil {
		return item, err
	}
	if item.Status != "approved" || item.Revision != source.CandidateRevision || item.Digest != source.Digest ||
		item.ArtifactID != source.ArtifactID || item.AcceptanceRevision != source.AcceptanceRevision ||
		item.MessageID != source.MessageID || (source.TaskID != "" && source.TaskID != item.TaskID) || item.CorrectionTruncated {
		return item, ErrExperienceSource
	}
	return item, nil
}

func normalizeSkillMatchOptions(mode string, keywords []string) (string, []string, error) {
	mode = strings.TrimSpace(mode)
	if (mode == "" || mode == SkillMatchGoalExact) && len(keywords) == 0 {
		return SkillMatchGoalExact, nil, nil
	}
	return "", nil, fmt.Errorf("%w：当前评测仅证明所填目标，不支持按关键词或整个范围自动扩大", ErrSkillProposal)
}

func skillProposalMatchesGoal(_ string, _ []string, proposalGoal, taskGoal string) bool {
	return strings.ToLower(strings.TrimSpace(taskGoal)) == strings.ToLower(strings.TrimSpace(proposalGoal))
}

func synthesizeSkillDraft(ctx context.Context, provider Provider, cfg ExperienceEvaluationConfig, scope, goal, base string, tips []string) (string, bool) {
	if provider == nil {
		return "", false
	}
	prompt := fmt.Sprintf("你是 Skill 方法整理助手。请从【新采纳经验】提炼可复用的方法增量。\n"+
		"仅输出增量正文，不重写现行 Skill。现行约束将由程序原样保留；不得放宽权限、跳过证据或复用历史事实。\n"+
		"总长度不超过 3800 字符。\n\n工作范围：%s\n适用目标：%s\n\n【现行 Skill】\n%s\n\n【新采纳经验】\n%s",
		scope, goal, base, strings.Join(tips, "\n"))
	timeout := cfg.RequestTimeout
	if timeout < 10*time.Second || timeout > 45*time.Second {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxOut := cfg.MaxOutput
	if maxOut < 256 || maxOut > 2048 {
		maxOut = 1024
	}
	var out strings.Builder
	_, err := provider.Stream(callCtx, []Message{{Role: "user", Text: prompt}}, false, maxOut, func(delta string) error {
		if out.Len()+len(delta) > 16*1024 {
			return ErrOutputBudget
		}
		out.WriteString(delta)
		return nil
	})
	if err != nil {
		return "", false
	}
	draft := strings.TrimSpace(out.String())
	if draft == "" || utf8.RuneCountInString(draft) > 4000 {
		return "", false
	}
	return draft, true
}

// A report supplied by a client (including an offline export) is not proof
// that this store executed the evaluation. Keep the old signature fail-closed.
func (s *Store) ProposeSkill(ctx context.Context, scope, goal string, report ExperienceEvaluationReport) (SkillProposal, error) {
	return s.ProposeSkillWithOptions(ctx, scope, goal, report, SkillProposalOptions{})
}

type trustedSkillEvaluation struct {
	Report       ExperienceEvaluationReport
	Suite        ExperienceEvaluationSuite
	ReportDigest string
	SuiteDigest  string
}

// Only the service-side writer may create this record. Verify the durable
// body hashes and association, not a claimed digest supplied by the caller.
func loadTrustedSkillEvaluation(ctx context.Context, tx *sql.Tx, runID string) (trustedSkillEvaluation, error) {
	var e trustedSkillEvaluation
	if runID == "" || len(runID) > 128 {
		return e, fmt.Errorf("%w：必须提供本账号服务端生成的评测编号", ErrSkillProposal)
	}
	var raw, snapshot, suiteJSON, suiteHash string
	err := tx.QueryRowContext(ctx, `SELECT r.report_json,r.report_digest,r.snapshot_digest,r.suite_digest,s.suite_json,s.suite_digest
FROM skill_evaluation_runs r JOIN skill_evaluation_suites s ON s.id=r.suite_id WHERE r.id=?`, runID).
		Scan(&raw, &e.ReportDigest, &snapshot, &e.SuiteDigest, &suiteJSON, &suiteHash)
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("%w：未找到对应的服务端评测记录", ErrSkillProposal)
	}
	if err != nil {
		return e, err
	}
	if len(raw) > 8<<20 || len(suiteJSON) > 8<<20 || digest(raw) != e.ReportDigest || digest(suiteJSON) != e.SuiteDigest ||
		e.SuiteDigest != suiteHash || json.Unmarshal([]byte(raw), &e.Report) != nil || json.Unmarshal([]byte(suiteJSON), &e.Suite) != nil ||
		e.Report.SuiteDigest != e.SuiteDigest || e.Report.Snapshot != snapshot {
		return e, fmt.Errorf("%w：服务端评测记录或基准集无法核实", ErrSkillProposal)
	}
	return e, nil
}

type frozenSkillProposal struct {
	evalRunID    string
	reportDigest string
	snapshot     string
	suiteDigest  string
	model        string
	evidence     SkillProposalEvidence
	sources      []ExperienceEvaluationSource
	tips         []string
	base         rsiProposalBase
}

func existingSkillProposal(tx *sql.Tx, scope, goal, reportDigest string) (SkillProposal, bool, error) {
	var previous string
	err := tx.QueryRow(`SELECT p.id FROM skill_proposals p JOIN personal_skills s ON s.id=p.skill_id
WHERE s.scope_label=? AND p.goal=? AND p.report_digest=?`, scope, goal, reportDigest).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillProposal{}, false, nil
	}
	if err != nil {
		return SkillProposal{}, false, err
	}
	p, err := readSkillProposal(tx, previous)
	return p, true, err
}

func (s *Store) freezeSkillProposal(ctx context.Context, scope, goal, evalRunID string) (frozenSkillProposal, *SkillProposal, error) {
	f := frozenSkillProposal{evalRunID: evalRunID}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return f, nil, err
	}
	defer tx.Rollback()
	loaded, err := loadTrustedSkillEvaluation(ctx, tx, evalRunID)
	if err != nil {
		return f, nil, err
	}
	f.reportDigest, f.snapshot, f.suiteDigest, f.model = loaded.ReportDigest, loaded.Report.Snapshot, loaded.SuiteDigest, loaded.Report.Model.Model
	if p, exists, err := existingSkillProposal(tx, scope, goal, f.reportDigest); err != nil || exists {
		return f, &p, err
	}
	f.evidence, f.sources, err = skillProposalEvidence(loaded.Report, scope, goal)
	if err != nil {
		return f, nil, err
	}
	f.base, err = readRSIProposalBase(tx, scope, goal)
	if err != nil {
		return f, nil, err
	}
	if f.base.Version.Valid {
		if err := verifySkillProposalSources(tx, f.base.SkillID, int(f.base.Version.Int64)); err != nil {
			return f, nil, err
		}
	}
	for _, source := range f.sources {
		if source.TaskID == "" {
			return f, nil, fmt.Errorf("%w：评测经验缺少可核验的来源任务", ErrSkillProposal)
		}
	}
	f.tips, err = skillProposalTips(tx, f.sources, scope, goal)
	if err != nil {
		return f, nil, err
	}
	if len(f.tips) == 0 {
		return f, nil, ErrSkillProposal
	}
	return f, nil, tx.Commit()
}

func skillProposalTips(tx *sql.Tx, sources []ExperienceEvaluationSource, scope, goal string) ([]string, error) {
	var tips []string
	seen := make(map[string]bool)
	add := func(tip string) {
		if !seen[tip] {
			seen[tip] = true
			tips = append(tips, tip)
		}
	}
	for _, source := range sources {
		item, err := proposalCandidate(tx, source)
		if err != nil {
			return nil, err
		}
		if item.Scope != scope || item.Goal != goal || item.TaskID != source.TaskID {
			return nil, ErrExperienceSource
		}
		if item.Correction != "" {
			add("- 用户纠正原文：" + item.Correction)
		}
		if item.Criteria != "" {
			add("- 交付检查：" + item.Criteria)
		}
		if len(item.FormatCues) > 0 {
			add("- 参考文档结构：" + strings.Join(item.FormatCues, "、"))
		}
	}
	// Replay and synthesis have their own explicit, bounded inputs.
	if len(strings.Join(tips, "\n")) > 32<<10 {
		return nil, fmt.Errorf("%w：经验材料超过单次可核验预算，请拆分评测", ErrSkillProposal)
	}
	return tips, nil
}

func (s *Store) ProposeSkillWithOptions(ctx context.Context, scope, goal string, _ ExperienceEvaluationReport, opts SkillProposalOptions) (SkillProposal, error) {
	scope, goal = strings.TrimSpace(scope), strings.TrimSpace(goal)
	if scope == "" || goal == "" || utf8.RuneCountInString(scope) > 40 || utf8.RuneCountInString(goal) > 1000 {
		return SkillProposal{}, ErrSkillProposal
	}
	if _, _, err := normalizeSkillMatchOptions(opts.MatchMode, opts.MatchKeywords); err != nil {
		return SkillProposal{}, err
	}
	frozen, existing, err := s.freezeSkillProposal(ctx, scope, goal, strings.TrimSpace(opts.EvaluationRunID))
	if err != nil {
		return SkillProposal{}, err
	}
	if existing != nil {
		return *existing, nil
	}

	base := frozen.base.Instructions
	content := strings.Join(frozen.tips, "\n")
	draftMode := "template"
	if opts.Synthesize && opts.Provider != nil {
		// The entire network wait is outside either SQL transaction. Limit it
		// with the shared model slot and section-specific request budget.
		synthCtx, cancel := context.WithTimeout(ctx, time.Minute)
		release, err := s.acquireModelSlot(synthCtx)
		if err == nil {
			if synthesized, ok := synthesizeSkillDraft(synthCtx, opts.Provider, opts.ModelConfig, scope, goal, base, frozen.tips); ok {
				content, draftMode = synthesized, "synthesized"
			}
			release()
		}
		cancel()
		if err := ctx.Err(); err != nil {
			return SkillProposal{}, err
		}
	}
	guidance := "仅当任务目标为 " + fmt.Sprintf("%q", goal) + " 时参考以下要求。以本次要求和证据为准，旧经验不改变事实或工具权限。\n" + content
	if strings.Contains(base, guidance) {
		return SkillProposal{}, fmt.Errorf("%w：现行版本已包含这组建议", ErrSkillProposal)
	}
	instructions := strings.TrimSpace(base + "\n\n" + guidance)
	if utf8.RuneCountInString(instructions) > 4000 {
		return SkillProposal{}, ErrSkill
	}
	return s.saveFrozenSkillProposal(ctx, scope, goal, frozen, guidance, instructions, draftMode)
}

func (s *Store) saveFrozenSkillProposal(ctx context.Context, scope, goal string, f frozenSkillProposal, guidance, instructions, draftMode string) (SkillProposal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillProposal{}, err
	}
	defer tx.Rollback()
	if p, exists, err := existingSkillProposal(tx, scope, goal, f.reportDigest); err != nil || exists {
		return p, err
	}
	current, err := readRSIProposalBase(tx, scope, goal)
	if err != nil {
		return SkillProposal{}, err
	}
	if current != f.base {
		return SkillProposal{}, ErrTaskRevision
	}
	if current.Version.Valid {
		if err := verifySkillProposalSources(tx, current.SkillID, int(current.Version.Int64)); err != nil {
			return SkillProposal{}, err
		}
	}
	tips, err := skillProposalTips(tx, f.sources, scope, goal)
	if err != nil {
		return SkillProposal{}, err
	}
	if !slices.Equal(tips, f.tips) {
		return SkillProposal{}, ErrExperienceSource
	}
	var reportHash, snapshotHash, suiteHash string
	err = tx.QueryRowContext(ctx, `SELECT report_digest,snapshot_digest,suite_digest FROM skill_evaluation_runs WHERE id=?`, f.evalRunID).
		Scan(&reportHash, &snapshotHash, &suiteHash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (reportHash != f.reportDigest || snapshotHash != f.snapshot || suiteHash != f.suiteDigest)) {
		return SkillProposal{}, fmt.Errorf("%w：生成期间评测依据已变更，请重新评测", ErrSkillProposal)
	}
	if err != nil {
		return SkillProposal{}, err
	}

	if err := prepareRSIProposalSkill(tx, scope, f.base); err != nil {
		return SkillProposal{}, err
	}
	skill, err := draftSkillTx(tx, scope, instructions, f.base.Revision, false)
	if err != nil {
		return SkillProposal{}, err
	}
	evidenceJSON, err := json.Marshal(f.evidence)
	if err != nil {
		return SkillProposal{}, err
	}
	sourcesJSON, err := json.Marshal(f.sources)
	if err != nil {
		return SkillProposal{}, err
	}
	proposalID := id()
	if _, err = tx.Exec(`INSERT INTO skill_proposals
(id,skill_id,version,base_version,goal,guidance,report_digest,snapshot_digest,model,evidence,sources,created_at,suite_digest,evaluation_run_id,match_mode,match_keywords,draft_mode)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		proposalID, skill.SkillID, skill.Version, f.base.Version, goal, guidance, f.reportDigest, f.snapshot,
		f.model, string(evidenceJSON), string(sourcesJSON), now(), f.suiteDigest, f.evalRunID, SkillMatchGoalExact, "[]", draftMode); err != nil {
		return SkillProposal{}, err
	}
	result, err := readSkillProposal(tx, proposalID)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func verifySkillProposalSources(tx *sql.Tx, skill string, version int) error {
	var raw string
	var base sql.NullInt64
	err := tx.QueryRow("SELECT sources,base_version FROM skill_proposals WHERE skill_id=? AND version=?", skill, version).Scan(&raw, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkProposalSourceJSON(tx, raw); err != nil {
		return err
	}
	if base.Valid {
		if base.Int64 >= int64(version) {
			return ErrState
		}
		return verifySkillProposalSources(tx, skill, int(base.Int64))
	}
	_, err = rsiPrivateBaseInstructions(tx, skill)
	return err
}

func checkProposalSourceJSON(tx *sql.Tx, raw string) error {
	var sources []ExperienceEvaluationSource
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := proposalCandidate(tx, source); err != nil {
			return err
		}
	}
	return nil
}

// Keep every addition within its exact evaluated goal. Applicable text is an
// overlay on the authorized effective base, not an arbitrary rewrite of it.
func applicableSkillInstructions(tx *sql.Tx, skill string, version int, taskGoal string) (string, error) {
	var instructions, hash, goal, guidance, sources, matchMode, proposalID string
	var base, approved sql.NullInt64
	var baseRevision int
	err := tx.QueryRow(`SELECT v.instructions,v.sha256,coalesce(p.goal,''),coalesce(p.guidance,''),coalesce(p.sources,''),p.base_version,
coalesce(p.match_mode,'goal_exact'),coalesce(p.id,''),v.approved_at,v.base_revision
FROM personal_skill_versions v LEFT JOIN skill_proposals p ON p.skill_id=v.skill_id AND p.version=v.version
WHERE v.skill_id=? AND v.version=?`, skill, version).Scan(&instructions, &hash, &goal, &guidance, &sources, &base, &matchMode, &proposalID, &approved, &baseRevision)
	if err != nil {
		return "", err
	}
	if digest(instructions) != hash {
		return "", ErrReadCheckpoint
	}
	if proposalID == "" {
		return instructions, nil
	}
	var current string
	if base.Valid {
		if base.Int64 >= int64(version) {
			return "", ErrState
		}
		current, err = applicableSkillInstructions(tx, skill, int(base.Int64), taskGoal)
	} else {
		current, err = rsiPrivateBaseInstructions(tx, skill)
	}
	if err != nil {
		return "", err
	}
	if matchMode != SkillMatchGoalExact || !skillProposalMatchesGoal(matchMode, nil, goal, taskGoal) {
		return current, nil
	}
	if err := checkProposalSourceJSON(tx, sources); err != nil {
		return "", err
	}
	if approved.Valid {
		// Previously published versions that relied on the old comparison
		// cannot silently become automatic RSI instructions. Keep only the
		// independently authorized, tested predecessor.
		if _, err := storedPassedSkillReplay(tx, proposalID, "", SkillProposalVersion{Version: version, Revision: baseRevision, Digest: hash}); err != nil {
			if errors.Is(err, ErrSkillReplayRequired) {
				return current, nil
			}
			return "", err
		}
	}
	effective := strings.TrimSpace(current + "\n\n" + guidance)
	if digest(effective) != hash {
		return "", ErrReadCheckpoint
	}
	return effective, nil
}

func (s *Store) PendingSkillProposals(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM skill_proposals p
JOIN personal_skills s ON s.id=p.skill_id
JOIN personal_skill_versions v ON v.skill_id=p.skill_id AND v.version=p.version
WHERE v.approved_at IS NULL AND v.base_revision=s.revision`).Scan(&count)
	return count, err
}

func readSkillProposal(tx *sql.Tx, proposal string) (SkillProposal, error) {
	p := SkillProposal{ID: proposal}
	var skill, evidence, sources, replay, keywordsRaw string
	var version int
	err := tx.QueryRow(`SELECT p.skill_id,p.version,p.base_version,p.goal,p.report_digest,p.snapshot_digest,
coalesce(p.suite_digest,''),coalesce(p.evaluation_run_id,''),coalesce(p.match_mode,'goal_exact'),coalesce(p.match_keywords,'[]'),coalesce(p.draft_mode,'template'),
p.model,p.evidence,p.sources,coalesce(v.instructions,''),coalesce(v.sha256,''),coalesce(r.report,'')
FROM skill_proposals p LEFT JOIN personal_skill_versions v ON v.skill_id=p.skill_id AND v.version=p.base_version
LEFT JOIN skill_proposal_replays r ON r.proposal_id=p.id
WHERE p.id=?`, proposal).Scan(&skill, &version, &p.BaseVersion, &p.Goal, &p.ReportDigest, &p.SnapshotDigest,
		&p.SuiteDigest, &p.EvaluationRunID, &p.MatchMode, &keywordsRaw, &p.DraftMode,
		&p.Model, &evidence, &sources, &p.BaseInstructions, &p.BaseDigest, &replay)
	if err != nil {
		return p, err
	}
	if keywordsRaw != "" && keywordsRaw != "[]" {
		if err := json.Unmarshal([]byte(keywordsRaw), &p.MatchKeywords); err != nil {
			return p, err
		}
	}
	p.Skill, err = scanSkill(tx.QueryRow(skillSelect+" WHERE s.id=? AND v.version=?", skill, version))
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(evidence), &p.Evidence); err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(sources), &p.Sources); err != nil {
		return p, err
	}
	p.Status = "pending"
	switch {
	case p.Skill.Active:
		p.Status = "active"
	case p.Skill.ApprovedAt != nil:
		p.Status = "historical"
	case p.Skill.Revision != p.Skill.BaseRevision:
		p.Status = "stale"
	}
	if err = verifySkillProposalSources(tx, skill, version); errors.Is(err, ErrExperienceSource) {
		p.Status, p.Skill.Instructions, p.BaseInstructions, p.BaseDigest = "unavailable", "", "", ""
	} else if err != nil {
		return p, err
	}
	if p.Status != "unavailable" {
		if p.BaseVersion != nil {
			p.BaseInstructions, err = applicableSkillInstructions(tx, p.Skill.SkillID, *p.BaseVersion, p.Goal)
		} else {
			p.BaseInstructions, err = rsiPrivateBaseInstructions(tx, p.Skill.SkillID)
		}
		if err != nil {
			return p, err
		}
		p.BaseDigest = ""
		if p.BaseInstructions != "" {
			p.BaseDigest = digest(p.BaseInstructions)
		}
		if replay != "" && len(replay) <= maxSkillReplayBytes {
			p.Replay = new(SkillReplayReport)
			if err := json.Unmarshal([]byte(replay), p.Replay); err != nil {
				return p, err
			}
			p.Replay.Passed = passesSkillReplayEvidence(*p.Replay)
		}
	}
	return p, nil
}

func (s *Store) SkillProposals(ctx context.Context) ([]SkillProposal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id FROM skill_proposals ORDER BY rowid DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	items := make([]SkillProposal, 0, len(ids))
	for _, id := range ids {
		item, err := readSkillProposal(tx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, tx.Commit()
}

func (s *Store) ApproveSkillProposal(ctx context.Context, id string, target SkillProposalVersion, replayID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var skill string
	var proposedVersion int
	if err := tx.QueryRow("SELECT skill_id,version FROM skill_proposals WHERE id=?", id).Scan(&skill, &proposedVersion); err != nil {
		return err
	}
	if proposedVersion != target.Version {
		return ErrSkillProposal
	}
	if err := activateSkillTx(tx, skill, target.Version, target.Revision, target.Digest, id, replayID); err != nil {
		return err
	}
	return tx.Commit()
}
