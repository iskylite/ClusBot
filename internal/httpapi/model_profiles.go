package httpapi

import (
	"context"
	"net/http"

	"clusbot/internal/model"
)

func (s Server) modelProfiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := s.Settings.Profiles(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) saveModelProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key := r.PathValue("id")
	if key != "" && !validID(key) {
		writeModelSettingsError(w, model.InvalidSettings("档案标识无效"))
		return
	}
	var in model.ProfileInput
	if !decode(w, r, &in) {
		return
	}
	view, err := s.Settings.SaveProfile(r.Context(), key, in)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	status := http.StatusOK
	if key == "" {
		status = http.StatusCreated
	}
	jsonResponse(w, status, view)
}

func (s Server) deleteModelProfile(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	if !validID(key) {
		writeModelSettingsError(w, model.InvalidSettings("档案标识无效"))
		return
	}
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Settings.DeleteProfile(r.Context(), key, in.Revision); err != nil {
		writeModelSettingsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) testModelProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key := r.PathValue("id")
	if !validID(key) {
		writeModelSettingsError(w, model.InvalidSettings("档案标识无效"))
		return
	}
	var in struct {
		Revision int `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	duration, err := s.Settings.TestProfile(r.Context(), key, in.Revision)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]int64{"duration_ms": duration.Milliseconds(), "revision": int64(in.Revision)})
}

func (s Server) readBenchmarkAdmissionRole(w http.ResponseWriter, r *http.Request) {
	readModelRole(w, r, s.Settings.ReadBenchmarkAdmissionRole)
}

func (s Server) readBenchmarkRevisionRole(w http.ResponseWriter, r *http.Request) {
	readModelRole(w, r, s.Settings.ReadBenchmarkRevisionRole)
}

func readModelRole(w http.ResponseWriter, r *http.Request, read func(context.Context) (model.BenchmarkAdmissionRoleView, error)) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := read(r.Context())
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) setBenchmarkAdmissionRole(w http.ResponseWriter, r *http.Request) {
	setModelRole(w, r, s.Settings.SetBenchmarkAdmissionRole)
}

func (s Server) setBenchmarkRevisionRole(w http.ResponseWriter, r *http.Request) {
	setModelRole(w, r, s.Settings.SetBenchmarkRevisionRole)
}

func setModelRole(w http.ResponseWriter, r *http.Request, set func(context.Context, model.BenchmarkAdmissionRoleInput) (model.BenchmarkAdmissionRoleView, error)) {
	w.Header().Set("Cache-Control", "no-store")
	var in model.BenchmarkAdmissionRoleInput
	if !decode(w, r, &in) {
		return
	}
	if in.ProfileID != "" && !validID(in.ProfileID) {
		writeModelSettingsError(w, model.InvalidSettings("档案标识无效"))
		return
	}
	out, err := set(r.Context(), in)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
