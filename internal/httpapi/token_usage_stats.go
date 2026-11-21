package httpapi

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"clusbot/internal/work"
)

type tokenUsageBucket struct {
	Date               string `json:"date"`
	InputTokens        int64  `json:"input_tokens"`
	OutputTokens       int64  `json:"output_tokens"`
	TotalTokens        int64  `json:"total_tokens"`
	Requests           int    `json:"requests"`
	UnreportedRequests int    `json:"unreported_requests"`
}

type tokenUsageModelBreakdown struct {
	Model              string  `json:"model"`
	Brand              string  `json:"brand"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	Percent            float64 `json:"percent"`
	Requests           int     `json:"requests"`
	UnreportedRequests int     `json:"unreported_requests"`
}

type tokenUsageStatsResponse struct {
	Range              string                     `json:"range"`
	TotalTokens        int64                      `json:"total_tokens"`
	InputTokens        int64                      `json:"input_tokens"`
	OutputTokens       int64                      `json:"output_tokens"`
	Requests           int                        `json:"requests"`
	DailyBuckets       []tokenUsageBucket         `json:"daily_buckets"`
	ByModel            []tokenUsageModelBreakdown `json:"by_model"`
	UnreportedRequests int                        `json:"unreported_requests"`
}

// tokenUsageStats 按时间窗口（24h/7d/30d）汇总模型调用量、输入/输出 Token 趋势及按模型/品牌分布，供图表卡片渲染。
func (s Server) tokenUsageStats(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		fail(w, http.StatusServiceUnavailable, "store_unavailable", "存储暂不可用")
		return
	}
	rng := r.URL.Query().Get("range")
	var days int
	switch rng {
	case "24h":
		days = 1
	case "30d":
		days = 30
	default:
		rng = "7d"
		days = 7
	}
	cutoffMS := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	stores := []*work.Store{s.Store}
	account, hasAccount := accountFromContext(r)
	if r.URL.Query().Get("scope") == "all" && s.ServerMode && hasAccount && account.IsSuperadmin && s.AdminTaskStores != nil {
		stores = nil
		for _, st := range s.AdminTaskStores() {
			if st != nil {
				stores = append(stores, st)
			}
		}
		if len(stores) == 0 {
			stores = []*work.Store{s.Store}
		}
	}

	resp := tokenUsageStatsResponse{
		Range:        rng,
		DailyBuckets: []tokenUsageBucket{},
		ByModel:      []tokenUsageModelBreakdown{},
	}
	dailyMap := map[string]*tokenUsageBucket{}
	modelMap := map[string]*tokenUsageModelBreakdown{}

	for _, st := range stores {
		stats, err := st.TokenUsageWindowStats(r.Context(), cutoffMS)
		if err != nil {
			writeError(w, err)
			return
		}
		resp.InputTokens += stats.InputTokens
		resp.OutputTokens += stats.OutputTokens
		resp.TotalTokens += stats.InputTokens + stats.OutputTokens
		resp.Requests += stats.Requests
		resp.UnreportedRequests += stats.UnreportedRequests
		for _, d := range stats.Daily {
			b := dailyMap[d.Date]
			if b == nil {
				b = &tokenUsageBucket{Date: d.Date}
				dailyMap[d.Date] = b
			}
			b.InputTokens += d.InputTokens
			b.OutputTokens += d.OutputTokens
			b.TotalTokens += d.InputTokens + d.OutputTokens
			b.Requests += d.Requests
			b.UnreportedRequests += d.UnreportedRequests
		}
		for _, m := range stats.Models {
			key := m.Model
			if key == "" {
				key = "default"
			}
			bm := modelMap[key]
			if bm == nil {
				bm = &tokenUsageModelBreakdown{
					Model: key,
					Brand: inferModelBrand(key, key),
				}
				modelMap[key] = bm
			}
			bm.InputTokens += m.InputTokens
			bm.OutputTokens += m.OutputTokens
			bm.TotalTokens += m.InputTokens + m.OutputTokens
			bm.Requests += m.Requests
			bm.UnreportedRequests += m.UnreportedRequests
		}
	}

	for _, b := range dailyMap {
		resp.DailyBuckets = append(resp.DailyBuckets, *b)
	}
	slices.SortFunc(resp.DailyBuckets, func(a, b tokenUsageBucket) int {
		return cmp.Compare(a.Date, b.Date)
	})

	for _, bm := range modelMap {
		if resp.TotalTokens > 0 {
			bm.Percent = float64(bm.TotalTokens*1000/resp.TotalTokens) / 10.0
		}
		resp.ByModel = append(resp.ByModel, *bm)
	}
	slices.SortFunc(resp.ByModel, func(a, b tokenUsageModelBreakdown) int {
		return cmp.Compare(b.TotalTokens, a.TotalTokens)
	})

	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, resp)
}
