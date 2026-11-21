package httpapi

import (
	"net/http"

	"clusbot/internal/model"
)

func (s Server) modelRouting(w http.ResponseWriter, r *http.Request) {
	out, err := s.Settings.ModelRouting(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) previewModelRouting(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Role     string `json:"role"`
		Business string `json:"business"`
	}
	if !decode(w, r, &input) {
		return
	}
	out, err := s.Settings.PreviewModelRoutingForBusiness(r.Context(), input.Business, input.Role)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) modelRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	policies, err := s.Settings.ModelRoutingPolicies(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"policies": policies})
}

func (s Server) saveModelRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input model.RoutingPolicyInput
	if !decode(w, r, &input) {
		return
	}
	for _, candidate := range input.Candidates {
		if !validID(candidate.ProfileID) {
			writeModelSettingsError(w, model.InvalidSettings("档案标识无效"))
			return
		}
	}
	policy, err := s.Settings.SaveModelRoutingPolicy(r.Context(), r.PathValue("business"), r.PathValue("role"), input)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, policy)
}
