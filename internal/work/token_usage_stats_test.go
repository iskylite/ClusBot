package work

import (
	"path/filepath"
	"testing"
)

func TestTokenUsageWindowStatsKeepsUnreportedAttempts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "token-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	for i, usage := range []struct {
		key                string
		input, output      any
		requests, reported int
	}{
		{"partial-usage", int64(11), int64(5), 3, 1},
		{"unreported-usage", nil, nil, 1, 0},
		{"deterministic", nil, nil, 0, 0},
	} {
		run, err := store.Submit(ctx, usage.key, usage.key, "", "", false)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if _, err := store.db.Exec(`UPDATE runs SET usage_tracked=1,input_tokens=?,output_tokens=?,model_requests=?,reported_requests=? WHERE id=?`,
			usage.input, usage.output, usage.requests, usage.reported, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.TokenUsageWindowStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.InputTokens != 11 || stats.OutputTokens != 5 || stats.Requests != 4 || stats.UnreportedRequests != 3 ||
		len(stats.Daily) != 1 || stats.Daily[0].UnreportedRequests != 3 || len(stats.Models) != 1 || stats.Models[0].UnreportedRequests != 3 {
		t.Fatalf("unexpected usage: %+v", stats)
	}
}
