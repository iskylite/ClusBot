package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) listCapabilityBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := s.Store.ListCapabilityBindings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, bindings)
}

func (s Server) upsertCapabilityBinding(w http.ResponseWriter, r *http.Request) {
	if account, ok := accountFromContext(r); ok && !account.IsSuperadmin {
		accountError(w, work.ErrAccountDenied)
		return
	}
	var in work.CapabilityBindingInput
	if !decode(w, r, &in) {
		return
	}
	binding, err := s.Store.UpsertCapabilityBinding(r.Context(), in)
	if errors.Is(err, work.ErrCapabilityBinding) {
		fail(w, http.StatusBadRequest, "invalid_capability_binding", err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, binding)
}
