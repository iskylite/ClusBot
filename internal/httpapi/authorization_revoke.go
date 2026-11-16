package httpapi

import "net/http"

func (s Server) revokeAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RevokeAuthorizationByID(r.Context(), r.PathValue("id"), s.principal(r)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
