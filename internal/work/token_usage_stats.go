package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type DailyTokenStat struct {
	Date               string
	InputTokens        int64
	OutputTokens       int64
	Requests           int
	UnreportedRequests int
}

type ModelTokenStat struct {
	Model              string
	InputTokens        int64
	OutputTokens       int64
	Requests           int
	UnreportedRequests int
}

type TokenUsageWindow struct {
	InputTokens        int64
	OutputTokens       int64
	Requests           int
	UnreportedRequests int
	Daily              []DailyTokenStat
	Models             []ModelTokenStat
}

// TokenUsageWindowStats 聚合指定起始时间戳之后的每日 Token 趋势与按模型请求统计。
func (s *Store) TokenUsageWindowStats(ctx context.Context, sinceUnixMS int64) (TokenUsageWindow, error) {
	var out TokenUsageWindow
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.created_at, r.input_tokens, r.output_tokens, r.model_requests, r.reported_requests,
		       COALESCE((SELECT mr.reported_model FROM context_manifests cm JOIN model_receipts mr ON mr.manifest_id=cm.id WHERE cm.run_id=r.id AND mr.reported_model<>'' ORDER BY cm.id DESC LIMIT 1), ''),
		       COALESCE((SELECT cm.manifest FROM context_manifests cm WHERE cm.run_id=r.id ORDER BY cm.id DESC LIMIT 1), ''),
		       CASE WHEN r.reported_requests > 1 THEN (
		         SELECT count(DISTINCT COALESCE(NULLIF(mr.reported_model,''), CASE WHEN json_valid(cm.manifest) THEN json_extract(cm.manifest,'$.requested_model') END))
		         FROM context_manifests cm LEFT JOIN model_receipts mr ON mr.manifest_id=cm.id WHERE cm.run_id=r.id
		       ) ELSE 0 END
		FROM runs r
		WHERE r.usage_tracked=1 AND r.created_at>=?
		ORDER BY r.created_at ASC`, sinceUnixMS)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	daily := map[string]*DailyTokenStat{}
	models := map[string]*ModelTokenStat{}

	for rows.Next() {
		var createdMS int64
		var input, output sql.NullInt64
		var reqs, reported, distinctModels int
		var reportedModel, manifestJSON string
		if err := rows.Scan(&createdMS, &input, &output, &reqs, &reported, &reportedModel, &manifestJSON, &distinctModels); err != nil {
			return out, err
		}
		inTok, outTok := input.Int64, output.Int64
		if reqs == 0 && (inTok > 0 || outTok > 0) {
			reqs, reported = 1, 1
		}
		if reqs == 0 {
			continue
		}
		if !input.Valid || !output.Valid {
			reported = 0
		}
		unreported := max(0, reqs-reported)
		out.UnreportedRequests += unreported
		out.InputTokens += inTok
		out.OutputTokens += outTok
		out.Requests += reqs

		day := time.UnixMilli(createdMS).UTC().Format("2006-01-02")
		d := daily[day]
		if d == nil {
			d = &DailyTokenStat{Date: day}
			daily[day] = d
		}
		d.InputTokens += inTok
		d.OutputTokens += outTok
		d.Requests += reqs
		d.UnreportedRequests += unreported

		modelName := reportedModel
		if modelName == "" && manifestJSON != "" {
			var mf struct {
				RequestedModel string `json:"requested_model"`
			}
			if json.Unmarshal([]byte(manifestJSON), &mf) == nil && mf.RequestedModel != "" {
				modelName = mf.RequestedModel
			}
		}
		if distinctModels > 1 || !ValidModelReceiptIdentifier(modelName) {
			// Historical aggregate totals can't be apportioned across several
			// providers or attributed to an unidentifiable model.
			modelName = "unattributed"
		}
		ms := models[modelName]
		if ms == nil {
			ms = &ModelTokenStat{Model: modelName}
			models[modelName] = ms
		}
		ms.InputTokens += inTok
		ms.OutputTokens += outTok
		ms.Requests += reqs
		ms.UnreportedRequests += unreported
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	for _, d := range daily {
		out.Daily = append(out.Daily, *d)
	}
	for _, m := range models {
		out.Models = append(out.Models, *m)
	}
	return out, nil
}
