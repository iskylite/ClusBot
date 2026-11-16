package httpapi

import (
	"net/http"
	"strings"

	"clusbot/internal/work"
)

func (s Server) lendAsset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ObjectRef     string `json:"object_ref"`
		Lender        string `json:"lender"`
		Borrower      string `json:"borrower"`
		DueAt         int64  `json:"due_at"`
		ReadOnlyScope string `json:"read_only_scope"`
	}
	if !decode(w, r, &in) {
		return
	}
	loan, err := s.Store.LendAsset(r.Context(), strings.TrimSpace(in.ObjectRef), strings.TrimSpace(in.Lender), strings.TrimSpace(in.Borrower), in.DueAt, in.ReadOnlyScope)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, loan)
}

func (s Server) returnAsset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ObjectRef                 string `json:"object_ref"`
		IdentityVerified          bool   `json:"identity_verified"`
		CurrentStateKnown         bool   `json:"current_state_known"`
		RepairCompleted           bool   `json:"repair_completed"`
		OperationsReconciled      bool   `json:"operations_reconciled"`
		ResponsibilityTransferred bool   `json:"responsibility_transferred"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.ReturnAsset(r.Context(), strings.TrimSpace(in.ObjectRef), work.ReturnVerification{
		IdentityVerified: in.IdentityVerified, CurrentStateKnown: in.CurrentStateKnown,
		RepairCompleted: in.RepairCompleted, OperationsReconciled: in.OperationsReconciled,
		ResponsibilityTransferred: in.ResponsibilityTransferred,
	}); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
