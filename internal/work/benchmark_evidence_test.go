package work

import (
	"strings"
	"testing"
)

func TestV31BenchmarkRejectsIncompleteOrUnrelatedEvidence(t *testing.T) {
	job := benchmarkJob{"bench-mcp", "O9", "J17", 1}
	e := benchmarkRequirement{
		Target: "pod7", Check: "deepep", Validator: "deepep@1", Job: job,
		ActualConfig:     map[string]string{"GPU": "40", "SM": "60"},
		RequiredMetrics:  map[string]string{"dispatch": "GB/s"},
		NeedsRawEvidence: true,
	}
	raw := benchmarkRawEvidence{job, "pod7", "deepep", "artifact:J17/1", strings.Repeat("a", 64)}
	r := benchmarkObservation{job, "pod7", "deepep", "deepep@1", "measured",
		map[string]string{"GPU": "40", "SM": "60"}, map[string]benchmarkMetric{"dispatch": {162, "GB/s"}}, &raw}
	tests := []struct {
		name, want string
		alter      func(*benchmarkObservation)
	}{
		{"verified", "verified", func(*benchmarkObservation) {}},
		{"not this connection", "missing", func(r *benchmarkObservation) { r.Job.ConnectionID = "other" }},
		{"old generation", "missing", func(r *benchmarkObservation) { r.Job.Generation = 2 }},
		{"no related raw log", "missing_evidence", func(r *benchmarkObservation) { r.Raw = nil }},
		{"wrong object raw", "unknown", func(r *benchmarkObservation) { copy := *r.Raw; copy.Target = "node-other"; r.Raw = &copy }},
		{"request but not actual", "unknown", func(r *benchmarkObservation) { r.ActualConfig = nil }},
		{"wrong actual", "configuration_mismatch", func(r *benchmarkObservation) { r.ActualConfig = map[string]string{"GPU": "32", "SM": "60"} }},
		{"skip not success", "skipped", func(r *benchmarkObservation) { r.Outcome = "skipped" }},
		{"provider rejected", "rejected", func(r *benchmarkObservation) { r.Outcome = "rejected" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ob := r
			test.alter(&ob)
			out, err := assessBenchmarkEvidence([]benchmarkRequirement{e}, []benchmarkObservation{ob})
			if err != nil || out.Checks[0].Execution != test.want || out.Complete != (test.want == "verified") || out.Performance != "not_configured" {
				t.Fatalf("%+v %v", out, err)
			}
		})
	}
}

func TestV31BenchmarkTargetMatrixAndThreshold(t *testing.T) {
	j := benchmarkJob{"bench", "O9", "J17", 1}
	a := benchmarkRequirement{Target: "node-1", Check: "bandwidth", Job: j, Validator: "bench@1",
		RequiredMetrics: map[string]string{"read": "GB/s"}, Threshold: &benchmarkThreshold{"read", "GB/s", 100}}
	b := a
	b.Target = "node-2"
	r := benchmarkObservation{Job: j, Target: a.Target, Check: a.Check, Validator: a.Validator, Outcome: "measured", Metrics: map[string]benchmarkMetric{"read": {101, "GB/s"}}}
	out, err := assessBenchmarkEvidence([]benchmarkRequirement{a, b}, []benchmarkObservation{r, r})
	if err != nil || out.Verified != 1 || out.Checks[1].Execution != "missing" || out.Performance != "incomplete" {
		t.Fatalf("%+v %v", out, err)
	}
	r2 := r
	r2.Target = b.Target
	r2.Metrics = map[string]benchmarkMetric{"read": {99, "GB/s"}}
	out, err = assessBenchmarkEvidence([]benchmarkRequirement{a, b}, []benchmarkObservation{r, r2})
	if err != nil || !out.Complete || out.Performance != "not_met" {
		t.Fatalf("%+v %v", out, err)
	}
	r3 := r2
	r3.Outcome = "rejected"
	out, err = assessBenchmarkEvidence([]benchmarkRequirement{a, b}, []benchmarkObservation{r, r2, r3})
	if err != nil || out.Checks[1].Execution != "unknown" || out.Complete {
		t.Fatalf("%+v %v", out, err)
	}
}
