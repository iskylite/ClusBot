package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) experienceEvaluations(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ExperienceEvaluations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, runs)
}

func (s Server) runExperienceEvaluation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Suite work.ExperienceEvaluationSuite `json:"suite"`
	}
	if !decode(w, r, &input) {
		return
	}
	if s.Settings.Demo {
		fail(w, 503, "experience_eval_model", "请使用已配置模型的工作台运行三臂评测")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	provider, config, err := s.Settings.SkillReplayModel(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusServiceUnavailable, "experience_eval_model", "请先配置可用模型后再运行三臂评测")
			return
		}
		writeError(w, err)
		return
	}
	run, err := s.Store.RunExperienceEvaluation(r.Context(), input.Suite, config, provider)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_experience_evaluation", err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, run)
}

func (s Server) skillProposals(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.SkillProposals(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) proposeSkill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Scope           string   `json:"scope_label"`
		Goal            string   `json:"goal"`
		EvaluationRunID string   `json:"evaluation_run_id"`
		MatchMode       string   `json:"match_mode"`
		MatchKeywords   []string `json:"match_keywords"`
		Synthesize      bool     `json:"synthesize"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validID(input.EvaluationRunID) {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", "请使用本账号的服务端三臂评测记录生成提案")
		return
	}
	if input.MatchMode != "" && input.MatchMode != work.SkillMatchGoalExact || len(input.MatchKeywords) != 0 {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", "私有提案只适用于已评测的同范围、同目标，不能扩大到关键词或整个范围")
		return
	}
	opts := work.SkillProposalOptions{
		EvaluationRunID: input.EvaluationRunID,
		MatchMode:       input.MatchMode,
		MatchKeywords:   input.MatchKeywords,
		Synthesize:      input.Synthesize,
	}
	if opts.Synthesize && !s.Settings.Demo {
		if provider, config, err := s.Settings.SkillReplayModel(r.Context()); err == nil {
			opts.Provider = provider
			opts.ModelConfig = config
		}
	}
	result, err := s.Store.ProposeSkillWithOptions(r.Context(), input.Scope, input.Goal, work.ExperienceEvaluationReport{}, opts)
	if errors.Is(err, work.ErrSkill) || errors.Is(err, work.ErrSkillProposal) {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) replaySkillProposal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		work.SkillProposalVersion
		Suite work.ExperienceEvaluationSuite `json:"suite"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !validID(id) || !validSkillProposalVersion(input.SkillProposalVersion) {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", "需要提案编号、候选版本、当前修订和摘要")
		return
	}
	if len(input.Suite.Cases) < 2 {
		fail(w, http.StatusBadRequest, "invalid_skill_replay", "请提交与训练评测不同的独立留出集，至少包含两个用例")
		return
	}
	if s.Settings.Demo {
		fail(w, 503, "skill_replay_model", "请使用已配置模型的工作台运行 Skill 正文重放")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	provider, config, err := s.Settings.SkillReplayModel(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusServiceUnavailable, "experience_eval_model", "请先配置可用模型后再运行三臂评测")
			return
		}
		writeError(w, err)
		return
	}
	report, err := s.Store.ReplaySkillProposal(r.Context(), id, input.SkillProposalVersion, input.Suite, config, provider)
	switch {
	case errors.Is(err, work.ErrSkillReplayInput):
		fail(w, 400, "invalid_skill_replay", err.Error())
	case err != nil:
		writeError(w, err)
	default:
		jsonResponse(w, http.StatusOK, report)
	}
}

func (s Server) approveSkillProposal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		work.SkillProposalVersion
		ReplayID string `json:"replay_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !validID(id) || !validSkillProposalVersion(input.SkillProposalVersion) {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", "需要提案编号、候选版本、当前修订和摘要")
		return
	}
	if err := s.Store.ApproveSkillProposal(r.Context(), id, input.SkillProposalVersion, input.ReplayID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validSkillProposalVersion(v work.SkillProposalVersion) bool {
	return v.Version > 0 && v.Revision >= 0 && len(v.Digest) == 64
}
