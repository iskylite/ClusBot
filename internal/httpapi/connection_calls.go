package httpapi

import (
	"net/http"

	"clusbot/internal/work"
)

func (s Server) connectionCalls(w http.ResponseWriter, r *http.Request) {
	var items []work.ConnectionCall
	var err error
	if account, ok := accountFromContext(r); ok {
		items, err = s.Store.AccountConnectionCalls(r.Context(), account.PrincipalID, r.PathValue("id"), 50)
	} else {
		items, err = s.Store.ConnectionCalls(r.Context(), r.PathValue("id"), 50)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, items)
}
