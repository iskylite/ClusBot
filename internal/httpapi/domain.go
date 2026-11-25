package httpapi

import (
	"net/http"

	"clusbot/internal/work"
)

// domainSnapshot 返回 领域台账的只读快照（GET /api/v1/domain/snapshot）。
//
// 认证由 Handler 集中处理（!s.authenticated(r) 统一返回 401），这里不重复校验；
// 数据读取与 DTO 契约见 internal/work/runtime_domain_read.go。
func (s Server) domainSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.Store.DomainSnapshot(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, snapshot)
}

func (s Server) recordFaultSegment(w http.ResponseWriter, r *http.Request) {
	var in work.FaultSegmentInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Store.RecordFaultSegment(r.Context(), in)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_fault_segment", err.Error())
		return
	}
	jsonResponse(w, http.StatusCreated, out)
}

func (s Server) syncFaultSegments(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope string `json:"scope"`
	}
	_ = decode(w, r, &in)
	out, err := s.Store.SyncFaultSegmentsFromMCP(r.Context(), in.Scope)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, out)
}
