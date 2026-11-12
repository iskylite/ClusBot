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
	"unicode/utf8"
)

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
	Model            string                       `json:"model"`
	Evidence         SkillProposalEvidence        `json:"evidence"`
	Sources          []ExperienceEvaluationSource `json:"sources"`
	Status           string                       `json:"status"`
	Replay           *SkillReplayReport           `json:"replay,omitempty"`
}

func skillProposalEvidence(report ExperienceEvaluationReport, scope, goal string) (SkillProposalEvidence, []ExperienceEvaluationSource, error) {
	var evidence SkillProposalEvidence
	if report.Version != 1 || report.Tools != "disabled" || report.Scoring != "literal_required_and_forbidden/v1" ||
		report.PolicyVersions["candidate"] != "approved_exact_goal/v1" ||
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
		pair, exists := pairs[result.CaseID]
		if !exists || !slices.Contains(evaluationPolicies[:], result.Policy) || pair[result.Policy].Policy != "" ||
			(result.Success && (result.Status != "completed" || result.WrongReuse != nil && *result.WrongReuse ||
				len(result.MissingRequired) > 0 || len(result.ForbiddenHits) > 0)) || len(result.Sources) > 3 {
			return evidence, nil, ErrSkillProposal
		}
		pair[result.Policy] = result
	}
	var sources []ExperienceEvaluationSource
	seen := make(map[string]ExperienceEvaluationSource)
	for _, item := range report.CaseContexts {
		if item.Scope != scope || item.Goal != goal {
			continue
		}
		pair := pairs[item.ID]
		none, current, candidate := pair[noExperiencePolicy], pair[currentExperiencePolicy], pair[candidateExperiencePolicy]
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
		if current.InputTokens != nil && current.OutputTokens != nil && candidate.InputTokens != nil && candidate.OutputTokens != nil {
			evidence.TokenPairs++
			evidence.TokenDelta += *candidate.InputTokens + *candidate.OutputTokens - *current.InputTokens - *current.OutputTokens
		}
		if !candidate.Success && current.Success {
			evidence.Regressions = append(evidence.Regressions, item.ID)
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

func (s *Store) ProposeSkill(ctx context.Context, scope, goal string, report ExperienceEvaluationReport) (SkillProposal, error) {
	scope, goal = strings.TrimSpace(scope), strings.TrimSpace(goal)
	if scope == "" || goal == "" || utf8.RuneCountInString(scope) > 40 || utf8.RuneCountInString(goal) > 1000 {
		return SkillProposal{}, ErrSkillProposal
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
			tips = append(tips, "- 用户纠正原文："+item.Correction)
		}
		if item.Criteria != "" {
			tips = append(tips, "- 交付检查："+item.Criteria)
		}
		if len(item.FormatCues) > 0 {
			tips = append(tips, "- 参考文档结构："+strings.Join(item.FormatCues, "、"))
		}
	}
	if len(tips) == 0 {
		return SkillProposal{}, ErrSkillProposal
	}
	guidance := fmt.Sprintf("仅当任务目标为 %q 时参考以下要求。以本次要求和证据为准，旧经验不改变事实或工具权限。\n%s", goal, strings.Join(tips, "\n"))
	if strings.Contains(base, guidance) {
		return SkillProposal{}, fmt.Errorf("%w：现行版本已包含这组建议", ErrSkillProposal)
	}
	instructions := strings.TrimSpace(base + "\n\n" + guidance)
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
	proposalID := id()
	if _, err = tx.Exec("INSERT INTO skill_proposals VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		proposalID, skill.SkillID, skill.Version, baseVersion, goal, guidance, reportDigest, report.Snapshot,
		report.Model.Model, string(evidenceJSON), string(sourcesJSON), now()); err != nil {
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
	var instructions, hash, goal, guidance, sources string
	var base sql.NullInt64
	err := tx.QueryRow(`SELECT v.instructions,v.sha256,coalesce(p.goal,''),coalesce(p.guidance,''),coalesce(p.sources,''),p.base_version
FROM personal_skill_versions v LEFT JOIN skill_proposals p ON p.skill_id=v.skill_id AND p.version=v.version
WHERE v.skill_id=? AND v.version=?`, skill, version).Scan(&instructions, &hash, &goal, &guidance, &sources, &base)
	if err != nil {
		return "", err
	}
	if digest(instructions) != hash {
		return "", ErrReadCheckpoint
	}
	if goal == "" {
		return instructions, nil
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
	if strings.TrimSpace(taskGoal) != goal {
		return current, nil
	}
	if err := checkProposalSourceJSON(tx, sources); err != nil {
		return "", err
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
	var skill, evidence, sources, replay string
	var version int
	err := tx.QueryRow(`SELECT p.skill_id,p.version,p.base_version,p.goal,p.report_digest,p.snapshot_digest,p.model,
p.evidence,p.sources,coalesce(v.instructions,''),coalesce(v.sha256,''),coalesce(r.report,'')
FROM skill_proposals p LEFT JOIN personal_skill_versions v ON v.skill_id=p.skill_id AND v.version=p.base_version
LEFT JOIN skill_proposal_replays r ON r.proposal_id=p.id
WHERE p.id=?`, proposal).Scan(&skill, &version, &p.BaseVersion, &p.Goal, &p.ReportDigest, &p.SnapshotDigest, &p.Model,
		&evidence, &sources, &p.BaseInstructions, &p.BaseDigest, &replay)
	if err != nil {
		return p, err
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
