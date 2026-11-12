package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

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
		Scope  string                          `json:"scope_label"`
		Goal   string                          `json:"goal"`
		Report work.ExperienceEvaluationReport `json:"report"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.Store.ProposeSkill(r.Context(), input.Scope, input.Goal, input.Report)
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
	if s.Settings.Demo {
		fail(w, 503, "skill_replay_model", "请使用已配置模型的工作台运行 Skill 正文重放")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	provider, config, err := s.Settings.SkillReplayModel(r.Context())
	if err != nil {
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
