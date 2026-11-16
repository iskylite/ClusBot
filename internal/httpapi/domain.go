package httpapi

import "net/http"

// domainSnapshot 返回 V3 领域台账的只读快照（GET /api/v1/domain/snapshot）。
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
