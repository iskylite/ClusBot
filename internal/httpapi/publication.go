package httpapi

import "net/http"

func (s Server) requestPublication(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ArtifactID string `json:"artifact_id"`
		Filename   string `json:"filename"`
		RequestID  string `json:"client_request_id"`
		Revision   int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.ArtifactID) || len(in.RequestID) < 8 || len(in.RequestID) > 128 {
		fail(w, 400, "invalid_publication", "需要有效的成果和请求标识")
		return
	}
	p, err := s.Store.RequestPublication(r.Context(), r.PathValue("id"), in.ArtifactID, in.Filename, in.RequestID, in.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 201, p)
}

func (s Server) decidePublication(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision string `json:"decision"`
		Revision int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Store.DecidePublication(r.Context(), r.PathValue("id"), in.Decision, in.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, p)
}

func (s Server) reconcilePublication(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Store.ReconcilePublication(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, p)
}
