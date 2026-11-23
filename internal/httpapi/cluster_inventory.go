package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

func (s Server) registerClusterInventory(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/domain/cluster-inventory", s.clusterInventory)
}

func (s Server) clusterInventory(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "存储暂不可用")
		return
	}
	limit := 24
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(w, http.StatusBadRequest, "invalid_limit", "每页数量须为 1 到 100")
			return
		}
		limit = parsed
	}
	after := r.URL.Query().Get("after")
	if len(after) > 1024 {
		fail(w, http.StatusBadRequest, "invalid_cursor", "节点游标无效")
		return
	}
	podRef := r.URL.Query().Get("pod_ref")
	if len(podRef) > 1024 {
		fail(w, http.StatusBadRequest, "invalid_pod", "POD 标识无效")
		return
	}
	page, err := s.Store.ClusterInventoryForPod(r.Context(), s.principal(r), podRef, after, limit, time.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, page)
}
