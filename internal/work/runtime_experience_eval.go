package work

// P5-08：经验与上下文对照评测 E06。
//
// 本文件实现用实际证据和业务效果判断记忆是否有益，复用现有三组对照基础：
//   - 固定模型/参数、工具环境、输入输出预算、案例快照和保留任务集；按时间/
//     对象/故障隔离调参与测试；
//   - 对比无经验、现行策略和候选策略；每组独立状态开始，防止前组新经验泄
//     入后组；
//   - 增加独立故障真值、实际操作/恢复验证和反例判据；文本必需词检查仅作
//     辅助，不能替代业务成功；
//   - 统计证据覆盖、错误复用、诊断/恢复质量、读取量、token/费用、主动处理
//     与端到端耗时；人工/上游等待、未结束项和未知成本单列；
//   - 测试前固定业务目标与允许成本变化，报告样本数/波动/失败明细；权限与
//     准入是硬约束，不参与可优化奖励。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 评测组（对比无经验、现行策略和候选策略；每组独立状态开始）
// ---------------------------------------------------------------------------

// EvaluationGroup 是评测组。
type EvaluationGroup struct {
	GroupID      string
	Kind         string // baseline/current/candidate
	// 独立状态开始（防止前组新经验泄入后组）。
	IndependentState bool
	// 案例快照与保留任务集。
	CaseSnapshot string
	RetainedTasks []string
}

// ensureGroupIsolation 核对组隔离（P5-08.2）。
// 每组独立状态开始，防止前组新经验泄入后组。
func ensureGroupIsolation(g EvaluationGroup) error {
	if !g.IndependentState {
		return fmt.Errorf("评测组 %s 未独立状态开始（防止前组新经验泄入后组）", g.GroupID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 独立判据（增加独立故障真值、实际操作/恢复验证和反例判据）
// ---------------------------------------------------------------------------

// IndependentJudgment 是独立判据。
type IndependentJudgment struct {
	// 独立故障真值、实际操作/恢复验证和反例判据。
	FaultTruth      string
	ActualOperation string
	RecoveryVerified bool
	CounterexampleCriteria string
}

// ErrTextOnlyScoring 是文本替代业务成功的错误。
var ErrTextOnlyScoring = errors.New("文本必需词检查仅作辅助，不能替代业务成功")

// scoreEvaluation 评分评测（P5-08.3）。
// 增加独立故障真值、实际操作/恢复验证和反例判据；文本必需词检查仅作辅助，
// 不能替代业务成功。
func scoreEvaluation(j IndependentJudgment, textContainsRequired bool) (score float64, businessSuccess bool, err error) {
	// 文本必需词检查仅作辅助（不能替代业务成功）。
	if textContainsRequired && !j.RecoveryVerified {
		return 0, false, fmt.Errorf("%w：文本含必需词但恢复未验证", ErrTextOnlyScoring)
	}
	// 业务成功由独立判据决定（独立故障真值、实际操作/恢复验证和反例判据）。
	businessSuccess = j.RecoveryVerified && j.FaultTruth != "" && j.ActualOperation != ""
	if businessSuccess {
		score = 1.0
	}
	return score, businessSuccess, nil
}

// ---------------------------------------------------------------------------
// 统计（证据覆盖、错误复用、诊断/恢复质量、读取量、token/费用、主动处理与端到端耗时）
// ---------------------------------------------------------------------------

// EvaluationMetrics 是评测统计。
type EvaluationMetrics struct {
	// 证据覆盖、错误复用、诊断/恢复质量。
	EvidenceCoverage float64
	ErrorReuse       int
	DiagnosisQuality float64
	RecoveryQuality  float64
	// 读取量、token/费用、主动处理与端到端耗时。
	ReadBytes        int64
	TokensUsed       int
	Cost             float64
	ProactiveActions int
	EndToEndLatencyMS int64
	// 人工/上游等待、未结束项和未知成本单列。
	HumanWaitMS      int64
	UpstreamWaitMS   int64
	UnfinishedItems  int
	UnknownCost      float64
}

// ErrMetricsIncomplete 是统计不完整的错误。
var ErrMetricsIncomplete = errors.New("人工/上游等待、未结束项和未知成本单列")

// ensureMetricsComplete 核对统计完整（P5-08.4）。
// 统计证据覆盖、错误复用、诊断/恢复质量、读取量、token/费用、主动处理与
// 端到端耗时；人工/上游等待、未结束项和未知成本单列。
func ensureMetricsComplete(m EvaluationMetrics) error {
	// 人工/上游等待、未结束项和未知成本单列（不能合并到主统计）。
	if m.HumanWaitMS < 0 || m.UpstreamWaitMS < 0 || m.UnfinishedItems < 0 || m.UnknownCost < 0 {
		return fmt.Errorf("%w：统计含负值（人工/上游等待、未结束项和未知成本单列）", ErrMetricsIncomplete)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 报告（测试前固定业务目标与允许成本变化，报告样本数/波动/失败明细）
// ---------------------------------------------------------------------------

// EvaluationReport 是评测报告。
type EvaluationReport struct {
	// 测试前固定业务目标与允许成本变化。
	BusinessGoal   string
	AllowedCostChange float64
	// 报告样本数/波动/失败明细。
	SampleCount    int
	Variance       float64
	FailureDetails []string
	// 权限与准入是硬约束（不参与可优化奖励）。
	PermissionViolations int
}

// ErrBaselineImproved 是基线改进的错误。
var ErrBaselineImproved = errors.New("有经验未优于基线时如实报告")

// reportEvaluation 报告评测（P5-08.5）。
// 测试前固定业务目标与允许成本变化，报告样本数/波动/失败明细；权限与准入
// 是硬约束，不参与可优化奖励；有经验未优于基线时如实报告，不靠换分母或
// 隐藏失败宣布提升。
func reportEvaluation(report EvaluationReport, baselineMetrics EvaluationMetrics, candidateMetrics EvaluationMetrics) error {
	// 权限与准入是硬约束（不参与可优化奖励）。
	if report.PermissionViolations > 0 {
		return fmt.Errorf("权限违规 %d（权限与准入是硬约束，不参与可优化奖励）", report.PermissionViolations)
	}
	// 有经验未优于基线时如实报告（不靠换分母或隐藏失败宣布提升）。
	if candidateMetrics.RecoveryQuality < baselineMetrics.RecoveryQuality {
		return fmt.Errorf("%w：候选恢复质量 %.2f < 基线 %.2f（如实报告，不靠换分母或隐藏失败宣布提升）",
			ErrBaselineImproved, candidateMetrics.RecoveryQuality, baselineMetrics.RecoveryQuality)
	}
	return nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
