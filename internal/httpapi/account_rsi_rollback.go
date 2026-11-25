package httpapi

import (
	"net/http"

	"clusbot/internal/work"
)

func (s Server) rollbackSkillProposal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		work.SkillProposalVersion
		Disable bool `json:"disable,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !validID(id) || !validSkillProposalVersion(input.SkillProposalVersion) {
		fail(w, http.StatusBadRequest, "invalid_skill_proposal", "需要提案编号、正在生效的版本、当前修订和摘要")
		return
	}
	if err := s.Store.RollbackSkillProposal(r.Context(), id, input.SkillProposalVersion, input.Disable); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
