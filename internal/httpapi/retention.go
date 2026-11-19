package httpapi

import (
	"net/http"

	"clusbot/internal/work"
)

func (s Server) saveRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PolicyKey          string `json:"policy_key"`
		ObjectKind         string `json:"object_kind"`
		RetentionMS        int64  `json:"retention_ms"`
		LegalHold          bool   `json:"legal_hold"`
		BusinessHold       bool   `json:"business_hold"`
		TombstoneMS        int64  `json:"tombstone_ms"`
		UnknownRetentionMS int64  `json:"unknown_retention_ms"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.UpsertRetentionPolicy(r.Context(), work.RetentionPolicy{
		PolicyKey: in.PolicyKey, ObjectKind: in.ObjectKind, RetentionMS: in.RetentionMS,
		LegalHold: in.LegalHold, BusinessHold: in.BusinessHold, TombstoneMS: in.TombstoneMS,
		UnknownRetentionMS: in.UnknownRetentionMS,
	}); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) storageSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, s.Store.ReadStorageSnapshot(r.Context()))
}
