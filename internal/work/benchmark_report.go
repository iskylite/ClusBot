package work

import (
	"context"
	"encoding/json"
	"fmt"
)

// BenchmarkReport is a read projection, not a publication, acceptance decision,
// or new evaluation. It reflects the runtime's persisted verifier output.
type BenchmarkReport struct {
	SchemaVersion   int                   `json:"schema_version"`
	TaskID          string                `json:"task_id"`
	TaskState       string                `json:"task_state"`
	Profile         BenchmarkProfileRef   `json:"profile"`
	PlanRevision    int                   `json:"plan_revision"`
	AssetAcceptance string                `json:"asset_acceptance"`
	Cases           []BenchmarkReportCase `json:"cases"`
}

type BenchmarkReportCase struct {
	CaseKey          string                         `json:"case_key"`
	StepID           string                         `json:"step_id"`
	ExecutionControl *BenchmarkCaseExecutionControl `json:"execution_control,omitempty"`
	DispatchControl  *BenchmarkDispatchControl      `json:"dispatch_control,omitempty"`
	Phase            string                         `json:"phase"`
	OperationState   string                         `json:"operation_state,omitempty"`
	ToolOutcome      string                         `json:"tool_outcome"`
	RequiredEvidence string                         `json:"required_evidence"`
	Performance      string                         `json:"performance"`
	Reason           string                         `json:"reason,omitempty"`
	Targets          []BenchmarkReportTarget        `json:"targets"`
}

type BenchmarkReportTarget struct {
	Target          string                             `json:"target"`
	Evidence        string                             `json:"evidence"`
	Performance     string                             `json:"performance"`
	Reason          string                             `json:"reason,omitempty"`
	VerifiedMetrics map[string]BenchmarkMeasuredMetric `json:"verified_metrics,omitempty"`
}

func (s *Store) ReadBenchmarkReport(ctx context.Context, actor, task string) (BenchmarkReport, error) {
	progress, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil {
		return BenchmarkReport{}, err
	}
	report := BenchmarkReport{SchemaVersion: 1, TaskID: progress.TaskID, TaskState: progress.State,
		Profile: BenchmarkProfileRef{Profile: progress.Profile, Version: progress.Version}, PlanRevision: progress.PlanRevision,
		AssetAcceptance: "not_evaluated", Cases: make([]BenchmarkReportCase, 0, len(progress.Cases))}
	for _, c := range progress.Cases {
		item := BenchmarkReportCase{CaseKey: c.Key, StepID: c.StepID, Phase: c.Phase, ExecutionControl: c.ExecutionControl, DispatchControl: c.DispatchControl,
			ToolOutcome: "not_started", RequiredEvidence: "not_evaluated", Performance: "not_evaluated", Targets: []BenchmarkReportTarget{}}
		if c.Operation != nil {
			item.OperationState = c.Operation.State
			switch RuntimeOperationState(c.Operation.State) {
			case RuntimeOperationDispatched, RuntimeOperationRunning:
				item.ToolOutcome = "in_progress"
			case RuntimeOperationReconcile:
				item.ToolOutcome = "outcome_unknown"
			case RuntimeOperationSucceeded:
				item.ToolOutcome = "completed"
			case RuntimeOperationFailed:
				item.ToolOutcome = "failed"
			case RuntimeOperationCancelled:
				item.ToolOutcome = "cancelled"
			}
		}
		if c.ExecutionControl != nil && c.ExecutionControl.State == benchmarkSkippedThisRun {
			item.RequiredEvidence = "incomplete"
		}
		if c.Phase == "skipped" && len(c.Report) > 0 {
			var skipped struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(c.Report, &skipped); err != nil {
				return BenchmarkReport{}, fmt.Errorf("基准跳过原因无效: %w", err)
			}
			item.Reason = skipped.Reason
		}
		if len(c.Report) > 0 && c.Operation != nil && (c.Phase == "verified" || c.Phase == "incomplete") {
			var assessment benchReportJSON
			if err := json.Unmarshal(c.Report, &assessment); err != nil {
				return BenchmarkReport{}, fmt.Errorf("基准核验记录无效: %w", err)
			}
			if len(assessment.Checks) > 0 {
				item.RequiredEvidence = "incomplete"
				if assessment.Complete {
					item.RequiredEvidence = "covered"
				}
				item.Performance = assessment.Performance
				for _, check := range assessment.Checks {
					target := BenchmarkReportTarget{Target: check.Target, Evidence: check.Execution, Performance: check.Performance, Reason: check.Reason}
					if check.Execution == "verified" && len(assessment.VerifiedMetrics[check.Target]) > 0 {
						target.VerifiedMetrics = map[string]BenchmarkMeasuredMetric{}
						for metric, value := range assessment.VerifiedMetrics[check.Target] {
							target.VerifiedMetrics[metric] = BenchmarkMeasuredMetric(value)
						}
					}
					item.Targets = append(item.Targets, target)
				}
			}
		}
		report.Cases = append(report.Cases, item)
	}
	return report, nil
}
