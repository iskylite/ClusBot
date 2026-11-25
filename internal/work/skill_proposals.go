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
			(result.Success && (result.Status != "completed" || result.WrongReuse != nil && *result.WrongReuse ||
				len(result.MissingRequired) > 0 || len(result.ForbiddenHits) > 0)) || len(result.Sources) > 3 {
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
		if none.Status != "completed" || current.Status != "completed" || candidate.Status != "completed" {
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
		if !candidate.Success && current.Success {
			evidence.Regressions = append(evidence.Regressions, item.ID)
		}
		if current.InputTokens != nil && current.OutputTokens != nil && candidate.InputTokens != nil && candidate.OutputTokens != nil {
			evidence.TokenPairs++
			evidence.TokenDelta += *candidate.InputTokens + *candidate.OutputTokens - *current.InputTokens - *current.OutputTokens
		}
		if !candidate.Success || current.Success || len(candidate.Sources) == 0 {
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
	if evidence.CandidateWrongReuse > evidence.CurrentWrongReuse {
		return evidence, nil, fmt.Errorf("%w：候选策略新增了错误复用", ErrSkillProposal)
	}
	if len(evidence.Gains) == 0 {
		return evidence, nil, ErrSkillProposal
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
	if mode == "" {
		mode = SkillMatchGoalExact
	}
	switch mode {
	case SkillMatchGoalExact, SkillMatchScopeAll:
		return mode, nil, nil
	case SkillMatchScopeKeywords:
		seen := make(map[string]bool)
		var out []string
		for _, kw := range keywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			if utf8.RuneCountInString(kw) > 40 {
				return "", nil, fmt.Errorf("%w：匹配关键词过长", ErrSkillProposal)
			}
			lower := strings.ToLower(kw)
			if !seen[lower] {
				seen[lower] = true
				out = append(out, kw)
			}
		}
		if len(out) == 0 || len(out) > 10 {
			return "", nil, fmt.Errorf("%w：关键词匹配模式需要 1 至 10 个关键词", ErrSkillProposal)
		}
		return mode, out, nil
	default:
		return "", nil, fmt.Errorf("%w：不支持的适用范围模式 %q", ErrSkillProposal, mode)
	}
}

func skillProposalMatchesGoal(mode string, keywords []string, proposalGoal, taskGoal string) bool {
	taskGoal = strings.TrimSpace(taskGoal)
	proposalGoal = strings.TrimSpace(proposalGoal)
	switch mode {
	case SkillMatchScopeAll:
		return true
	case SkillMatchScopeKeywords:
		if strings.EqualFold(taskGoal, proposalGoal) {
			return true
		}
		lowerTask := strings.ToLower(taskGoal)
		for _, kw := range keywords {
			if kw != "" && strings.Contains(lowerTask, strings.ToLower(kw)) {
				return true
			}
		}
		return false
	default:
		return strings.EqualFold(taskGoal, proposalGoal)
	}
}

func synthesizeSkillDraft(ctx context.Context, provider Provider, cfg ExperienceEvaluationConfig, scope, goal, base string, tips []string) (string, bool) {
	if provider == nil {
		return "", false
	}
	prompt := fmt.Sprintf("你是 Skill 规范整理助手。请把【现行 Skill】与【新采纳经验】综合整理为一版去重、精炼、结构化的 Skill 指示。\n"+
		"要求：\n1. 仅输出整理后的 Skill 正文，不要解释或寒暄；\n2. 总长度严格不超过 3800 字符；\n3. 保留原有核心约束，新经验仅补充方法与检查要求，不得放宽权限或跳过证据。\n\n"+
		"工作范围：%s\n适用目标：%s\n\n【现行 Skill】\n%s\n\n【新采纳经验】\n%s",
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

func (s *Store) ProposeSkill(ctx context.Context, scope, goal string, report ExperienceEvaluationReport) (SkillProposal, error) {
	return s.ProposeSkillWithOptions(ctx, scope, goal, report, SkillProposalOptions{})
}

func (s *Store) ProposeSkillWithOptions(ctx context.Context, scope, goal string, report ExperienceEvaluationReport, opts SkillProposalOptions) (SkillProposal, error) {
	scope, goal = strings.TrimSpace(scope), strings.TrimSpace(goal)
	if scope == "" || goal == "" || utf8.RuneCountInString(scope) > 40 || utf8.RuneCountInString(goal) > 1000 {
		return SkillProposal{}, ErrSkillProposal
	}
	matchMode, matchKeywords, err := normalizeSkillMatchOptions(opts.MatchMode, opts.MatchKeywords)
	if err != nil {
		return SkillProposal{}, err
	}
	evalRunID := strings.TrimSpace(opts.EvaluationRunID)
	if evalRunID != "" {
		var rawReport, runSuiteDigest string
		err := s.db.QueryRowContext(ctx, `SELECT report_json,suite_digest FROM skill_evaluation_runs WHERE id=?`, evalRunID).Scan(&rawReport, &runSuiteDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return SkillProposal{}, fmt.Errorf("%w：未找到对应的服务端评测记录", ErrSkillProposal)
		}
		if err != nil {
			return SkillProposal{}, err
		}
		if err := json.Unmarshal([]byte(rawReport), &report); err != nil {
			return SkillProposal{}, err
		}
		if report.SuiteDigest == "" {
			report.SuiteDigest = runSuiteDigest
		}
	}
	evidence, sources, err := skillProposalEvidence(report, scope, goal)
	if err != nil {
		return SkillProposal{}, err
	}
	// Retain the evidence and source references, without retaining model answers.
	report.Results = slices.Clone(report.Results)
	for i := range report.Results {
		report.Results[i].Answer = ""
	}
	encodedReport, err := json.Marshal(report)
	if err != nil {
		return SkillProposal{}, err
	}
	reportDigest := digest(string(encodedReport))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillProposal{}, err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRow("SELECT p.id FROM skill_proposals p JOIN personal_skills s ON s.id=p.skill_id WHERE s.scope_label=? AND p.goal=? AND p.report_digest=?", scope, goal, reportDigest).Scan(&previous)
	if err == nil {
		return readSkillProposal(tx, previous)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SkillProposal{}, err
	}
	var baseVersion sql.NullInt64
	var revision int
	var base string
	err = tx.QueryRow(`SELECT s.revision,s.active_version,coalesce(v.instructions,'')
FROM personal_skills s LEFT JOIN personal_skill_versions v ON v.skill_id=s.id AND v.version=s.active_version
WHERE s.scope_label=?`, scope).Scan(&revision, &baseVersion, &base)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SkillProposal{}, err
	}
	var tips []string
	seenTip := make(map[string]bool)
	addTip := func(tip string) {
		if tip != "" && !seenTip[tip] {
			seenTip[tip] = true
			tips = append(tips, tip)
		}
	}
	for index, source := range sources {
		item, err := proposalCandidate(tx, source)
		if err != nil {
			return SkillProposal{}, err
		}
		if item.Scope != scope || item.Goal != goal {
			return SkillProposal{}, ErrExperienceSource
		}
		sources[index].TaskID = item.TaskID
		if item.Correction != "" {
			addTip("- 用户纠正原文：" + item.Correction)
		}
		if item.Criteria != "" {
			addTip("- 交付检查：" + item.Criteria)
		}
		if len(item.FormatCues) > 0 {
			addTip("- 参考文档结构：" + strings.Join(item.FormatCues, "、"))
		}
	}
	if len(tips) == 0 {
		return SkillProposal{}, ErrSkillProposal
	}
	scopeDesc := fmt.Sprintf("仅当任务目标为 %q 时参考以下要求", goal)
	if matchMode == SkillMatchScopeKeywords {
		scopeDesc = fmt.Sprintf("当 %q 范围的任务目标匹配关键词（%s）时参考以下要求", scope, strings.Join(matchKeywords, "、"))
	} else if matchMode == SkillMatchScopeAll {
		scopeDesc = fmt.Sprintf("在 %q 范围的所有任务中参考以下要求", scope)
	}
	guidance := fmt.Sprintf("%s。以本次要求和证据为准，旧经验不改变事实或工具权限。\n%s", scopeDesc, strings.Join(tips, "\n"))
	draftMode := "template"
	var instructions string
	if opts.Synthesize && opts.Provider != nil {
		if synthesized, ok := synthesizeSkillDraft(ctx, opts.Provider, opts.ModelConfig, scope, goal, base, tips); ok {
			instructions = synthesized
			draftMode = "synthesized"
		}
	}
	if instructions == "" {
		if strings.Contains(base, guidance) {
			return SkillProposal{}, fmt.Errorf("%w：现行版本已包含这组建议", ErrSkillProposal)
		}
		instructions = strings.TrimSpace(base + "\n\n" + guidance)
	}
	if utf8.RuneCountInString(instructions) > 4000 {
		return SkillProposal{}, ErrSkill
	}
	skill, err := draftSkillTx(tx, scope, instructions, revision, false)
	if err != nil {
		return SkillProposal{}, err
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return SkillProposal{}, err
	}
	sourcesJSON, err := json.Marshal(sources)
	if err != nil {
		return SkillProposal{}, err
	}
	keywordsJSON, err := json.Marshal(matchKeywords)
	if err != nil {
		return SkillProposal{}, err
	}
	if matchKeywords == nil {
		keywordsJSON = []byte("[]")
	}
	proposalID := id()
	if _, err = tx.Exec(`INSERT INTO skill_proposals
(id,skill_id,version,base_version,goal,guidance,report_digest,snapshot_digest,model,evidence,sources,created_at,suite_digest,evaluation_run_id,match_mode,match_keywords,draft_mode)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		proposalID, skill.SkillID, skill.Version, baseVersion, goal, guidance, reportDigest, report.Snapshot,
		report.Model.Model, string(evidenceJSON), string(sourcesJSON), now(), report.SuiteDigest, evalRunID, matchMode, string(keywordsJSON), draftMode); err != nil {
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
	return nil
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

// Keep each addition scoped to its evaluated goal, including when a later
// proposal is based on a previously activated generated version.
func applicableSkillInstructions(tx *sql.Tx, skill string, version int, taskGoal string) (string, error) {
	var instructions, hash, goal, guidance, sources, matchMode, keywordsRaw, draftMode string
	var base sql.NullInt64
	err := tx.QueryRow(`SELECT v.instructions,v.sha256,coalesce(p.goal,''),coalesce(p.guidance,''),coalesce(p.sources,''),p.base_version,
coalesce(p.match_mode,'goal_exact'),coalesce(p.match_keywords,'[]'),coalesce(p.draft_mode,'template')
FROM personal_skill_versions v LEFT JOIN skill_proposals p ON p.skill_id=v.skill_id AND p.version=v.version
WHERE v.skill_id=? AND v.version=?`, skill, version).Scan(&instructions, &hash, &goal, &guidance, &sources, &base, &matchMode, &keywordsRaw, &draftMode)
	if err != nil {
		return "", err
	}
	if digest(instructions) != hash {
		return "", ErrReadCheckpoint
	}
	if goal == "" {
		return instructions, nil
	}
	var keywords []string
	if keywordsRaw != "" {
		_ = json.Unmarshal([]byte(keywordsRaw), &keywords)
	}
	var current string
	if base.Valid {
		if base.Int64 >= int64(version) {
			return "", ErrState
		}
		current, err = applicableSkillInstructions(tx, skill, int(base.Int64), taskGoal)
		if err != nil {
			return "", err
		}
	}
	if !skillProposalMatchesGoal(matchMode, keywords, goal, taskGoal) {
		return current, nil
	}
	if err := checkProposalSourceJSON(tx, sources); err != nil {
		return "", err
	}
	if draftMode == "synthesized" {
		return instructions, nil
	}
	return strings.TrimSpace(current + "\n\n" + guidance), nil
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
		p.Status, p.Skill.Instructions, p.BaseInstructions = "unavailable", "", ""
	} else if err != nil {
		return p, err
	}
	if replay != "" && p.Status != "unavailable" {
		p.Replay = new(SkillReplayReport)
		if err := json.Unmarshal([]byte(replay), p.Replay); err != nil {
			return p, err
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
