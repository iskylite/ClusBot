package httpapi

import (
	"errors"
	"net/http"

	"clusbot/internal/work"
)

func (s Server) submitIsolatedReply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authenticated(r) {
		fail(w, 401, "unauthorized", "需要本机登录")
		return
	}
	var input work.IsolatedReplyInput
	if !decode(w, r, &input) {
		return
	}
	conversation := r.PathValue("id")
	if !validID(conversation) || !validID(input.ProfileID) {
		fail(w, 400, "invalid_isolated_reply", work.ErrIsolatedReplyInput.Error())
		return
	}
	run, err := s.Store.SubmitIsolatedReply(r.Context(), s.principal(r), conversation, input, s.Settings.IsolatedReplyCredential)
	switch {
	case errors.Is(err, work.ErrIsolatedReplyInput):
		fail(w, 400, "invalid_isolated_reply", err.Error())
	case errors.Is(err, work.ErrIsolatedReplyUnavailable):
		fail(w, 409, "isolated_reply_unavailable", err.Error())
	case err != nil:
		writeError(w, err)
	default:
		writeSubmission(w, run)
	}
}
