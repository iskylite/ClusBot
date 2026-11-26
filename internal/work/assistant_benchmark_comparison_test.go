package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type comparisonFixtureProvider struct {
	job, unit, method string
	value             float64
}

func (*comparisonFixtureProvider) Supports(BenchmarkBinding) bool { return true }
func (p *comparisonFixtureProvider) Start(context.Context, BenchmarkJobRequest) (string, error) {
	return p.job, nil
}
func (*comparisonFixtureProvider) Lookup(context.Context, BenchmarkJobRequest) (string, error) {
	return "", errors.New("read-only preview must not need lookup")
}
func (p *comparisonFixtureProvider) Get(_ context.Context, req BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	if p.job != job {
		return BenchmarkJobResult{}, ErrConflict
	}
	var rows []BenchmarkMeasurement
	for _, ref := range req.Targets {
		rows = append(rows, BenchmarkMeasurement{Target: ref, Outcome: "measured", ActualConfig: map[string]string{"method": p.method, "gpu_count": "40"},
			Metrics: map[string]BenchmarkMeasuredMetric{"rate": {Value: p.value, Unit: p.unit}},
			Raw:     &BenchmarkRawArtifact{Ref: "fixture-stdout", SHA256: digest("raw fixture evidence"), Bytes: []byte("raw fixture evidence")}})
	}
	return BenchmarkJobResult{JobID: job, State: "completed", Measurements: rows}, nil
}

func TestAssistantBenchmarkComparisonUsesVerifiedReportsOnly(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "comparison.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:" + strings.Repeat("a", 32)
	if err := s.BindAccountBusinessStore(ctx, actor); err != nil {
		t.Fatal(err)
	}
	grant := DomainBenchmarkGrant{Connection: "fixture", Revision: 1, Tool: "sample", ExpiresAt: now() + 60000}
	if err := s.ImportDomainAssetsForAccount(ctx, actor, []DomainAssetEntry{
		{ObjectRef: "node-1", Kind: "node", Identity: "fixture-1", Read: true, Benchmark: []DomainBenchmarkGrant{grant}},
		{ObjectRef: "node-2", Kind: "node", Identity: "fixture-2", Read: true, Benchmark: []DomainBenchmarkGrant{grant}},
	}); err != nil {
		t.Fatal(err)
	}
	publish := func(version int, node, unit, method string, declares bool) {
		t.Helper()
		c := BenchmarkCase{Key: "test", Binding: BenchmarkBinding{Connection: "fixture", Revision: 1, Tool: "sample", Validator: benchmarkValidatorV1},
			Targets: []string{node}, ActualConfig: map[string]string{"method": method, "gpu_count": "40"}, RequiredMetrics: map[string]string{"rate": unit}, NeedsRawEvidence: true}
		if declares {
			c.ComparisonKeys = []string{"method", "gpu_count"}
		}
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "pair", Version: version, Cases: []BenchmarkCase{c}}); err != nil {
			t.Fatal(err)
		}
	}
	publish(1, "node-1", "GB/s", "first", true)
	publish(2, "node-1", "GiB/s", "first", true)
	publish(3, "node-2", "GB/s", "first", true)
	publish(4, "node-1", "GB/s", "second", true)
	publish(5, "node-1", "GB/s", "first", false)
	actual := func(key string, version int, unit, method string, value float64) string {
		t.Helper()
		p := &comparisonFixtureProvider{job: key, unit: unit, method: method, value: value}
		id, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: key, Profile: "pair", Version: version}, p)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if ok, e := s.runBenchmarkOnce(ctx, p); !ok || e != nil {
				t.Fatalf("fixture runtime could not settle: %v %v", ok, e)
			}
		}
		return id
	}
	before := actual("baseline", 1, "GB/s", "first", 100)
	after := actual("current", 1, "GB/s", "first", 125)
	units := actual("units", 2, "GiB/s", "first", 99)
	target := actual("target", 3, "GB/s", "first", 99)
	method := actual("method", 4, "GB/s", "second", 98)
	unknown := actual("unknown", 5, "GB/s", "first", 98)
	read := func(current string) AssistantBenchmarkComparison {
		t.Helper()
		req := AssistantBenchmarkComparisonRequest{Baseline: before, Current: current, Case: "test"}
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		out, err := projectAssistantBenchmarkComparison(ctx, tx, actor, req)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(after); got.Reason != "comparable" || len(got.Rows) != 1 || got.Rows[0].Delta == nil || *got.Rows[0].Delta != 25 ||
		got.Rows[0].Baseline.Unit != "GB/s" || got.Rows[0].Current.Unit != "GB/s" {
		t.Fatalf("same verified basis did not compare: %+v", got)
	}
	for _, test := range []struct{ task, reason string }{{units, "metric_or_unit"}, {target, "target_scope"}, {method, "method_unverified"}, {unknown, "method_unverified"}} {
		got := read(test.task)
		if got.Reason != test.reason || got.CurrentStatus != "verified" || len(got.Rows) == 0 {
			t.Fatalf("wrong side-by-side status %s: %+v", test.reason, got)
		}
		for _, row := range got.Rows {
			if row.Delta != nil {
				t.Fatalf("unlike conditions produced a difference: %+v", got)
			}
		}
	}
	if _, err := s.db.Exec("UPDATE benchmark_runs SET plan_revision=2 WHERE task_id=?", after); err != nil {
		t.Fatal(err)
	}
	if got := read(after); got.Reason != "method_unverified" || got.Rows[0].Delta != nil {
		t.Fatalf("revised plan merely shared the original profile and received a delta: %+v", got)
	}
	if _, err := s.db.Exec("DELETE FROM benchmark_evidence WHERE step_id=(SELECT step_id FROM benchmark_cases WHERE task_id=?)", after); err != nil {
		t.Fatal(err)
	}
	if got := read(after); got.Reason != "unverified" || got.CurrentStatus != "missing_required_evidence" || len(got.Rows) != 1 ||
		got.Rows[0].Current != nil || got.Rows[0].Delta != nil {
		t.Fatalf("deleted original still represented as verified: %+v", got)
	}

	req := AssistantBenchmarkComparisonRequest{Baseline: before, Current: after, Case: "test"}
	if _, err := s.OpenAssistantBenchmarkComparison(ctx, "user:"+strings.Repeat("e", 32), "", "foreign", "compare", req); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign account could start reading: %v", err)
	}
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='read' AND object_ref='node-1'", now(), actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAssistantBenchmarkComparison(ctx, actor, "", "revoked", "compare", req); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old reports retained access after current read grant withdrawal: %v", err)
	}
}
