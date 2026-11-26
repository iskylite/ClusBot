package httpapi

import (
	"errors"
	"net/http"
	"slices"

	"clusbot/internal/work"
)

func (s Server) listSkills(w http.ResponseWriter, r *http.Request) {
	result, err := s.Store.Skills(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if account, ok := accountFromContext(r); ok && !account.IsSuperadmin {
		result = slices.DeleteFunc(result, func(version work.SkillVersion) bool { return !version.Active })
	}
	jsonResponse(w, http.StatusOK, result)
}

func (s Server) draftSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope        string `json:"scope_label"`
		Instructions string `json:"instructions"`
		Revision     int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.Store.DraftSkill(r.Context(), in.Scope, in.Instructions, in.Revision)
	if errors.Is(err, work.ErrSkill) {
		fail(w, http.StatusBadRequest, "invalid_skill", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, result)
}

func (s Server) activateSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version  int    `json:"version"`
		Revision int    `json:"revision"`
		Digest   string `json:"sha256"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.ActivateSkill(r.Context(), r.PathValue("id"), in.Version, in.Revision, in.Digest); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) disableSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.DisableSkill(r.Context(), r.PathValue("id"), in.Revision); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) runSkillBinding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	binding, err := s.Store.ReadRunSkillBinding(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, binding)
}

func (s Server) readConversationSkillSelection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := s.Store.ReadConversationSkillSelection(r.Context(), s.principal(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) setConversationSkillSelection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		work.ConversationSkillBindingInput
		Preview *assistantSkillConfirmation `json:"preview_confirmation,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(r.PathValue("id")) || (in.SkillID != "" && !validID(in.SkillID)) {
		fail(w, 400, "invalid_skill_binding", "会话或 Skill 标识无效。")
		return
	}
	if in.Action == assistantSkillConfirmAction {
		if in.Preview == nil || in.SkillID != "" || in.Version != 0 || in.ExpectedBindingRevision != nil {
			fail(w, 400, "invalid_skill_binding", "确认只能使用原预览，不能另外指定 Skill 或版本。")
			return
		}
		s.confirmAssistantSkillPreview(w, r, *in.Preview)
		return
	}
	if in.Preview != nil {
		fail(w, 400, "invalid_skill_binding", "预览确认不能用于其他绑定操作。")
		return
	}
	out, err := s.Store.SetConversationSkillSelection(r.Context(), s.principal(r), r.PathValue("id"), in.ConversationSkillBindingInput)
	if errors.Is(err, work.ErrSkill) {
		fail(w, 400, "invalid_skill_binding", "请选择可用的 Skill 版本，或清除当前选用。")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func skillDocumentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrSkillDocument):
		fail(w, http.StatusBadRequest, "invalid_skill_document", err.Error())
	case errors.Is(err, work.ErrSkillDocumentSource):
		fail(w, http.StatusConflict, "skill_document_source_changed", err.Error())
	default:
		writeError(w, err)
	}
}

func (s Server) previewSkillDocument(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in work.SkillDocumentInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.PreviewSkillDocument(r.Context(), in)
	if err != nil {
		skillDocumentError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) importSkillDocument(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		work.SkillDocumentInput
		ExpectedSourceSHA256 string `json:"expected_source_sha256"`
		PreviewBinding       string `json:"preview_binding"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.ImportSkillDocument(r.Context(), in.SkillDocumentInput, in.ExpectedSourceSHA256, in.PreviewBinding)
	if err != nil {
		skillDocumentError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, out)
}
