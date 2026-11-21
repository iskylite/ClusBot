package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) registerPreferences(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/preferences", s.generalPreferences)
	api.HandleFunc("PUT /api/v1/preferences", s.saveGeneralPreferences)
}

func (s Server) generalPreferences(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "个人设置暂不可用")
		return
	}
	p, err := s.Store.GeneralPreferences(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, p)
}

func (s Server) saveGeneralPreferences(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "个人设置暂不可用")
		return
	}
	// Every field is required for replacement, including false booleans and
	// revision zero. A partial or null payload must not silently reset settings.
	var input struct {
		Language             *string `json:"language"`
		Timezone             *string `json:"timezone"`
		Appearance           *string `json:"appearance"`
		ReduceMotion         *bool   `json:"reduce_motion"`
		SendShortcut         *string `json:"send_shortcut"`
		DesktopNotifications *bool   `json:"desktop_notifications"`
		Revision             *int    `json:"revision"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Language == nil || input.Timezone == nil || input.Appearance == nil || input.ReduceMotion == nil ||
		input.SendShortcut == nil || input.DesktopNotifications == nil || input.Revision == nil {
		fail(w, http.StatusBadRequest, "invalid_preferences", "请提供完整的偏好设置及修订号")
		return
	}
	p, err := s.Store.SaveGeneralPreferences(r.Context(), work.GeneralPreferences{
		Language: *input.Language, Timezone: *input.Timezone, Appearance: *input.Appearance,
		ReduceMotion: *input.ReduceMotion, SendShortcut: *input.SendShortcut,
		DesktopNotifications: *input.DesktopNotifications, Revision: *input.Revision,
	})
	switch {
	case errors.Is(err, work.ErrInvalidPreferences):
		fail(w, http.StatusBadRequest, "invalid_preferences", err.Error())
	case errors.Is(err, work.ErrPreferencesRevision):
		fail(w, http.StatusConflict, "preferences_revision", err.Error())
	case err != nil:
		writeError(w, err)
	default:
		jsonResponse(w, http.StatusOK, p)
	}
}
