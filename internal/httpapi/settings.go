package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func (s Server) modelSettings(w http.ResponseWriter, r *http.Request) {
	value, err := s.Settings.View(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeModelSettings(w, value)
}

func (s Server) modelRuntimeProjection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	value, err := s.Settings.RuntimeProjection(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, value)
}

func (s Server) saveModelSettings(w http.ResponseWriter, r *http.Request) {
	var input model.SettingsInput
	if !decode(w, r, &input) {
		return
	}
	value, err := s.Settings.Save(r.Context(), input)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	s.writeModelSettings(w, value)
}

func (s Server) testModelSettings(w http.ResponseWriter, r *http.Request) {
	var input model.SettingsInput
	if !decode(w, r, &input) {
		return
	}
	duration, err := s.Settings.Test(r.Context(), input)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]int64{"duration_ms": duration.Milliseconds()})
}

func (s Server) discoverModels(w http.ResponseWriter, r *http.Request) {
	var input model.SettingsInput
	if !decode(w, r, &input) {
		return
	}
	models, err := s.Settings.Discover(r.Context(), input)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, models)
}

func (s Server) writeModelSettings(w http.ResponseWriter, value model.SettingsView) {
	jsonResponse(w, http.StatusOK, struct {
		model.SettingsView
		WorkspacePath string `json:"workspace_path"`
	}{value, s.WorkspacePath})
}

func (s Server) modelReady(w http.ResponseWriter, r *http.Request) bool {
	value, err := s.Settings.View(r.Context())
	if err != nil {
		writeError(w, err)
		return false
	}
	if value.Mode == "unconfigured" {
		fail(w, 503, "model_not_configured", "请先在设置中填写模型 ID")
		return false
	}
	return true
}

func writeModelSettingsError(w http.ResponseWriter, err error) {
	var invalid model.InvalidSettings
	switch {
	case errors.As(err, &invalid):
		fail(w, 400, "invalid_model_settings", err.Error())
	case errors.Is(err, work.ErrModelSettingsRevision):
		fail(w, 409, "model_settings_revision", err.Error())
	case errors.Is(err, model.ErrTestTimeout):
		fail(w, 504, "model_test_timeout", err.Error())
	case errors.Is(err, model.ErrModelDiscovery):
		fail(w, 502, "model_discovery_failed", err.Error())
	case errors.Is(err, model.ErrTestFailed):
		fail(w, 502, "model_test_failed", err.Error())
	default:
		writeError(w, err)
	}
}
