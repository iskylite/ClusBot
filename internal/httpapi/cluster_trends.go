package httpapi

import (
	"net/http"
	"time"
)

func (s Server) registerClusterTrends(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/domain/cluster-trends", s.clusterTrends)
}

func (s Server) clusterTrends(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "存储暂不可用")
		return
	}
	window := r.URL.Query().Get("range")
	if window == "" {
		window = "7d"
	}
	if window != "24h" && window != "7d" && window != "30d" {
		fail(w, http.StatusBadRequest, "invalid_trend_range", "趋势时间范围须为 24h、7d 或 30d")
		return
	}
	stats, err := s.Store.ClusterTrendStats(r.Context(), s.principal(r), window, time.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, stats)
}
