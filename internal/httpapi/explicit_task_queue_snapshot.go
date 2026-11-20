package httpapi

import (
	"net/http"

	"clusbot/internal/work"
)

func (s Server) explicitTaskQueueSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.Store.ReadExplicitTaskQueue(r.Context(), s.principal(r))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, struct {
		Scope                   string `json:"scope"`
		InstanceConfiguredLimit int    `json:"instance_configured_limit"`
		work.ExplicitTaskQueueSnapshot
	}{
		Scope:                     "current_principal",
		InstanceConfiguredLimit:   s.MaxPendingExplicitTasks,
		ExplicitTaskQueueSnapshot: snapshot,
	})
}
