package work

import (
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
)

// benchmarkJob identifies an execution using locally resolved bindings, not
// the task ID or generation claimed in a provider payload.
type benchmarkJob struct {
	ConnectionID, OperationID, ProviderJobID string
	Generation                               int
}

type benchmarkMetric struct {
	Value float64
	Unit  string
}

type benchmarkThreshold struct {
	Metric, Unit string
	Minimum      float64
}

// One requirement per target / check. A target can also be a frozen group
// for a collective check, rather than an individual member.
type benchmarkRequirement struct {
	Target, Check, Validator string
	Job                      benchmarkJob
	ActualConfig             map[string]string
	RequiredMetrics          map[string]string // metric -> agreed unit
	NeedsRawEvidence         bool
	Threshold                *benchmarkThreshold
}

// benchmarkRawEvidence comes from a trusted, job-scoped artifact reader after
// checking bytes against the manifest. Hash syntax alone proves nothing;
// this type is internal and must not be decoded directly from model input.
type benchmarkRawEvidence struct {
	Job                        benchmarkJob
	Target, Check, Ref, SHA256 string
}

// benchmarkObservation is produced by a versioned validator/adapter, not by
// a report-writing Worker. A process exit code or job status alone is not an
// observation of completed measurement.
type benchmarkObservation struct {
	Job                               benchmarkJob
	Target, Check, Validator, Outcome string // measured/failed/skipped/rejected
	ActualConfig                      map[string]string
	Metrics                           map[string]benchmarkMetric
	Raw                               *benchmarkRawEvidence
}

type benchmarkCheckResult struct {
	Target, Check, Execution, Performance, Reason string
}
type benchmarkAssessment struct {
	Checks      []benchmarkCheckResult
	Complete    bool // all required target/check pairs were verified
	Verified    int
	Performance string // not_configured / incomplete / met / not_met
	Unexpected  int
}

func assessBenchmarkEvidence(expected []benchmarkRequirement, results []benchmarkObservation) (benchmarkAssessment, error) {
	type key struct{ target, check string }
	index := make(map[key]benchmarkRequirement, len(expected))
	for _, e := range expected {
		k := key{e.Target, e.Check}
		if e.Target == "" || e.Check == "" || e.Validator == "" || !validBenchmarkJob(e.Job) || len(e.RequiredMetrics) == 0 {
			return benchmarkAssessment{}, fmt.Errorf("基准要求缺少目标、测项、绑定、核验器或指标：%v", k)
		}
		if _, ok := index[k]; ok {
			return benchmarkAssessment{}, fmt.Errorf("重复基准要求：%v", k)
		}
		for name, unit := range e.RequiredMetrics {
			if name == "" || unit == "" {
				return benchmarkAssessment{}, fmt.Errorf("无效指标要求：%v", k)
			}
		}
		for name, val := range e.ActualConfig {
			if name == "" || val == "" {
				return benchmarkAssessment{}, fmt.Errorf("无效实测配置要求：%v", k)
			}
		}
		if t := e.Threshold; t != nil && (t.Unit == "" || e.RequiredMetrics[t.Metric] != t.Unit || !finiteBenchmark(t.Minimum)) {
			return benchmarkAssessment{}, fmt.Errorf("无效达标线：%v", k)
		}
		index[k] = e
	}
	if len(index) == 0 {
		return benchmarkAssessment{}, fmt.Errorf("基准没有必测项")
	}
	accepted := make(map[key]benchmarkObservation, len(index))
	conflicting := make(map[key]bool)
	out := benchmarkAssessment{Performance: "not_configured"}
	for _, r := range results {
		k := key{r.Target, r.Check}
		exp, ok := index[k]
		if !ok || r.Job != exp.Job || r.Validator != exp.Validator {
			out.Unexpected++
			continue
		}
		if last, ok := accepted[k]; ok && !reflect.DeepEqual(last, r) {
			conflicting[k] = true
		}
		accepted[k] = r
	}
	configured, anyBelow := false, false
	for _, e := range expected {
		k := key{e.Target, e.Check}
		r, exists := accepted[k]
		configured = configured || e.Threshold != nil
		cr := benchmarkCheckResult{Target: e.Target, Check: e.Check, Execution: "missing", Performance: "not_configured", Reason: "未收到本次作业的对应测项"}
		if e.Threshold != nil {
			cr.Performance = "incomplete"
		}
		switch {
		case conflicting[k]:
			cr.Execution, cr.Reason = "unknown", "同一测项的可信结果相互冲突，需核对"
		case exists:
			cr.Execution, cr.Reason = checkBenchmarkEvidence(e, r)
		}
		if cr.Execution == "verified" {
			out.Verified++
			if e.Threshold != nil {
				if r.Metrics[e.Threshold.Metric].Value < e.Threshold.Minimum {
					cr.Performance = "not_met"
					anyBelow = true
				} else {
					cr.Performance = "met"
				}
			}
		}
		out.Checks = append(out.Checks, cr)
	}
	out.Complete = out.Verified == len(expected)
	if configured {
		switch {
		case anyBelow:
			out.Performance = "not_met"
		case !out.Complete:
			out.Performance = "incomplete"
		default:
			out.Performance = "met"
		}
	}
	return out, nil
}

func checkBenchmarkEvidence(e benchmarkRequirement, r benchmarkObservation) (string, string) {
	switch r.Outcome {
	case "failed", "skipped", "rejected":
		return r.Outcome, "已收到执行回执，但未完成有效测量"
	case "measured":
	default:
		return "unknown", "执行状态无法核实"
	}
	for k, want := range e.ActualConfig {
		got, ok := r.ActualConfig[k]
		if !ok || got == "" {
			return "unknown", "缺少实测配置，不以请求参数代替"
		}
		if got != want {
			return "configuration_mismatch", "实测配置不符合本次要求"
		}
	}
	for name, unit := range e.RequiredMetrics {
		m, ok := r.Metrics[name]
		if !ok || m.Unit != unit || !finiteBenchmark(m.Value) {
			return "unknown", "测项指标、单位或数值无法核实"
		}
	}
	if e.NeedsRawEvidence {
		raw := r.Raw
		if raw == nil {
			return "missing_evidence", "指标已收到，原始证据缺失"
		}
		if raw.Job != e.Job || raw.Target != e.Target || raw.Check != e.Check || raw.Ref == "" || !validBenchmarkSHA(raw.SHA256) {
			return "unknown", "原始证据的作业归属或完整性无法核实"
		}
	}
	return "verified", "已核实本次配置和证据"
}

func validBenchmarkJob(j benchmarkJob) bool {
	return j.ConnectionID != "" && j.OperationID != "" && j.ProviderJobID != "" && j.Generation > 0
}
func finiteBenchmark(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }
func validBenchmarkSHA(sum string) bool {
	if len(sum) != 64 {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil
}
