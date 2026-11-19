package work

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type benchmarkComparison struct {
	PreviousTask string                                `json:"previous_task_id"`
	Delta        map[string]map[string]benchmarkMetric `json:"delta"`
}

// Previous benchmark reports are comparable only when the operator declared
// comparison conditions, both cases verified them, and the frozen profile and
// target membership are identical. A delta is not a causal explanation.
func previousBenchmarkComparison(tx *sql.Tx, d benchmarkDispatch, metrics map[string]map[string]benchmarkMetric) (*benchmarkComparison, error) {
	var task, raw string
	err := tx.QueryRow(`SELECT before.task_id,before.report FROM benchmark_cases current
JOIN benchmark_runs active ON active.task_id=current.task_id
JOIN benchmark_runs prior ON prior.actor=active.actor AND prior.profile_key=active.profile_key AND prior.profile_version=active.profile_version
JOIN benchmark_cases before ON before.task_id=prior.task_id AND before.case_key=current.case_key
AND before.frozen_targets=current.frozen_targets AND before.phase='verified'
JOIN task_states ts ON ts.task_id=before.task_id AND ts.state='succeeded'
JOIN tasks t ON t.id=before.task_id
WHERE current.step_id=? AND before.task_id<>current.task_id AND active.plan_revision=1 AND prior.plan_revision=1
ORDER BY t.completed_at DESC,before.task_id DESC LIMIT 1`, d.step).Scan(&task, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var prev v31BenchReport
	if err = json.Unmarshal([]byte(raw), &prev); err != nil {
		return nil, err
	}
	if !prev.Complete || len(prev.VerifiedMetrics) != len(metrics) {
		return nil, nil
	}
	out := benchmarkComparison{PreviousTask: task, Delta: make(map[string]map[string]benchmarkMetric, len(metrics))}
	for target, values := range metrics {
		before := prev.VerifiedMetrics[target]
		if len(before) != len(values) {
			return nil, nil
		}
		out.Delta[target] = make(map[string]benchmarkMetric, len(values))
		for key, m := range values {
			older, ok := before[key]
			if !ok || older.Unit != m.Unit || !finiteBenchmark(m.Value-older.Value) {
				return nil, nil
			}
			out.Delta[target][key] = benchmarkMetric{Value: m.Value - older.Value, Unit: m.Unit}
		}
	}
	return &out, nil
}
