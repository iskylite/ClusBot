package httpapi

import (
	"net/http"

	"clusbot/internal/model"
)

func (s Server) registerModelProviderRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/settings/model/providers", s.modelProviders)
	api.HandleFunc("POST /api/v1/settings/model/providers", s.saveModelProvider)
	api.HandleFunc("PUT /api/v1/settings/model/providers/{id}", s.saveModelProvider)
	api.HandleFunc("DELETE /api/v1/settings/model/providers/{id}", s.deleteModelProvider)
	api.HandleFunc("POST /api/v1/settings/model/providers/{id}/discover", s.discoverModelProvider)
	api.HandleFunc("POST /api/v1/settings/model/providers/{id}/diagnose", s.diagnoseModelProvider)
	api.HandleFunc("GET /api/v1/settings/model/catalog", s.modelProfiles)
	api.HandleFunc("POST /api/v1/settings/model/catalog", s.saveCatalogModel)
	api.HandleFunc("PUT /api/v1/settings/model/catalog/{id}", s.saveCatalogModel)
	api.HandleFunc("DELETE /api/v1/settings/model/catalog/{id}", s.deleteModelProfile)
	api.HandleFunc("GET /api/v1/settings/model/default", s.defaultModel)
	api.HandleFunc("PUT /api/v1/settings/model/default", s.setDefaultModel)
}

func (s Server) modelProviders(w http.ResponseWriter, r *http.Request) {
	out, err := s.Settings.Providers(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) saveModelProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != "" && !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("提供方标识无效"))
		return
	}
	var in model.ProviderInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Settings.SaveProvider(r.Context(), id, in)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	jsonResponse(w, status, out)
}

func (s Server) deleteModelProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("提供方标识无效"))
		return
	}
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Settings.DeleteProvider(r.Context(), id, in.Revision); err != nil {
		writeModelSettingsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) saveCatalogModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != "" && !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("模型标识无效"))
		return
	}
	var in model.CatalogModelInput
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.ProviderID) {
		writeModelSettingsError(w, model.InvalidSettings("提供方标识无效"))
		return
	}
	out, err := s.Settings.SaveCatalogModel(r.Context(), id, in)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	jsonResponse(w, status, out)
}

func (s Server) defaultModel(w http.ResponseWriter, r *http.Request) {
	out, err := s.Settings.DefaultModel(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	var in model.DefaultModelInput
	if !decode(w, r, &in) {
		return
	}
	if in.ProfileID != "" && !validID(in.ProfileID) {
		writeModelSettingsError(w, model.InvalidSettings("模型标识无效"))
		return
	}
	out, err := s.Settings.SetDefaultModel(r.Context(), in)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) discoverModelProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("提供方标识无效"))
		return
	}
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Settings.DiscoverProvider(r.Context(), id, in.Revision)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) diagnoseModelProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeModelSettingsError(w, model.InvalidSettings("提供方标识无效"))
		return
	}
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Settings.DiagnoseProvider(r.Context(), id, in.Revision)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
