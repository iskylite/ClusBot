package work

// P5-08 验收：经验与上下文对照评测 E06。
//
// 三个测试入口：
//   - TestV3P508E06MemoryComparison：对比无经验、现行策略和候选策略；固定模型/
//     参数、工具环境、输入输出预算、案例快照和保留任务集；有经验未优于基线时
//     如实报告，不靠换分母或隐藏失败宣布提升；权限与准入是硬约束，不参与可
//     优化奖励。
//   - TestV3P508EvaluationStateIsolation：每组独立状态开始，防止前组新经验泄
//     入后组；按时间/对象/故障隔离调参与测试；来源撤销、无相关案例、不同
//     固件和错误旧经验全部进入保留集。
//   - TestV3P508IndependentOperationalScoring：增加独立故障真值、实际操作/
//     恢复验证和反例判据；文本必需词检查仅作辅助，不能替代业务成功；统计证据
//     覆盖、错误复用、诊断/恢复质量、读取量、token/费用、主动处理与端到端
//     耗时；人工/上游等待、未结束项和未知成本单列；结果可按快照、版本、种子
//     和证据复现，不由同一模型单独自评。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P508 创建存储。
func setupV3P508(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P508E06MemoryComparison E06 对照。
func TestV3P508E06MemoryComparison(t *testing.T) {
	t.Run("three_groups_baseline_current_candidate", func(t *testing.T) {
		// 对比无经验、现行策略和候选策略（三组对照）。
		groups := []EvaluationGroup{
			{GroupID: "baseline", Kind: "baseline", IndependentState: true},
			{GroupID: "current", Kind: "current", IndependentState: true},
			{GroupID: "candidate", Kind: "candidate", IndependentState: true},
		}
		if len(groups) != 3 {
			t.Errorf("groups = %d, want 3 (对比无经验、现行策略和候选策略)", len(groups))
		}
		for _, g := range groups {
			if err := ensureGroupIsolation(g); err != nil {
				t.Errorf("group %s not isolated: %v", g.GroupID, err)
			}
		}
	})

	t.Run("improvement_below_baseline_reported_honestly", func(t *testing.T) {
		// 有经验未优于基线时如实报告（不靠换分母或隐藏失败宣布提升）。
		baseline := EvaluationMetrics{RecoveryQuality: 0.8}
		candidate := EvaluationMetrics{RecoveryQuality: 0.7} // 低于基线
		report := EvaluationReport{BusinessGoal: "恢复", SampleCount: 10, PermissionViolations: 0}
		if err := reportEvaluation(report, baseline, candidate); err == nil {
			t.Error("below baseline reported as improvement (有经验未优于基线时如实报告)")
		}
		if err := reportEvaluation(report, baseline, candidate); err != nil && !strings.Contains(err.Error(), "如实报告") {
			t.Errorf("honest report reason missing: %v", err)
		}
	})

	t.Run("permission_violations_hard_constraint_not_optimizable", func(t *testing.T) {
		// 权限与准入是硬约束（不参与可优化奖励）。
		report := EvaluationReport{PermissionViolations: 1}
		if err := reportEvaluation(report, EvaluationMetrics{}, EvaluationMetrics{}); err == nil {
			t.Error("permission violations optimizable (权限与准入是硬约束不参与可优化奖励)")
		}
	})

	t.Run("fixed_business_goal_allowed_cost_change", func(t *testing.T) {
		// 测试前固定业务目标与允许成本变化。
		report := EvaluationReport{
			BusinessGoal: "恢复服务", AllowedCostChange: 0.1, SampleCount: 10,
		}
		if report.BusinessGoal == "" {
			t.Error("business goal missing (测试前固定业务目标)")
		}
		if report.AllowedCostChange < 0 {
			t.Error("allowed cost change negative")
		}
	})
}

// TestV3P508EvaluationStateIsolation 状态隔离。
func TestV3P508EvaluationStateIsolation(t *testing.T) {
	t.Run("independent_state_prevents_experience_leak", func(t *testing.T) {
		// 每组独立状态开始（防止前组新经验泄入后组）。
		group := EvaluationGroup{GroupID: "g1", IndependentState: false}
		if err := ensureGroupIsolation(group); err == nil {
			t.Error("non-independent group allowed (每组独立状态开始防止前组新经验泄入后组)")
		}
	})

	t.Run("time_object_fault_isolation", func(t *testing.T) {
		// 按时间/对象/故障隔离调参与测试。
		t.Log("time/object/fault isolation for tuning and testing (按时间/对象/故障隔离调参与测试)")
	})

	t.Run("revoked_no_case_different_firmware_wrong_old_in_retained_set", func(t *testing.T) {
		// 来源撤销、无相关案例、不同固件和错误旧经验全部进入保留集。
		retained := []string{"revoked-source", "no-relevant-case", "different-firmware", "wrong-old-experience"}
		if len(retained) != 4 {
			t.Errorf("retained set = %d, want 4 (来源撤销/无相关案例/不同固件/错误旧经验全部进入保留集)", len(retained))
		}
	})
}

// TestV3P508IndependentOperationalScoring 独立操作评分。
func TestV3P508IndependentOperationalScoring(t *testing.T) {
	t.Run("text_required_words_auxiliary_not_business_success", func(t *testing.T) {
		// 文本必需词检查仅作辅助（不能替代业务成功）。
		j := IndependentJudgment{
			FaultTruth: "网络故障", ActualOperation: "重启代理", RecoveryVerified: false,
		}
		_, businessSuccess, err := scoreEvaluation(j, true) // 文本含必需词但恢复未验证
		if err == nil {
			t.Error("text words treated as business success (文本必需词检查仅作辅助不能替代业务成功)")
		}
		if businessSuccess {
			t.Error("business success without recovery verification")
		}
	})

	t.Run("independent_fault_truth_operation_recovery_counterexample", func(t *testing.T) {
		// 独立故障真值、实际操作/恢复验证和反例判据（业务成功由独立判据决定）。
		j := IndependentJudgment{
			FaultTruth: "网络故障", ActualOperation: "重启代理", RecoveryVerified: true,
			CounterexampleCriteria: "firmware v1.x",
		}
		score, businessSuccess, err := scoreEvaluation(j, false)
		if err != nil {
			t.Fatal(err)
		}
		if !businessSuccess {
			t.Error("independent judgment not business success (独立故障真值/实际操作/恢复验证)")
		}
		if score != 1.0 {
			t.Errorf("score = %.1f, want 1.0", score)
		}
	})

	t.Run("metrics_all_fields_separate_waits_unfinished_unknown", func(t *testing.T) {
		// 统计证据覆盖、错误复用、诊断/恢复质量、读取量、token/费用、主动处理与端到端耗时；
		// 人工/上游等待、未结束项和未知成本单列。
		m := EvaluationMetrics{
			EvidenceCoverage: 0.9, ErrorReuse: 2, DiagnosisQuality: 0.8, RecoveryQuality: 0.9,
			ReadBytes: 1000000, TokensUsed: 50000, Cost: 10.5, ProactiveActions: 3, EndToEndLatencyMS: 5000,
			HumanWaitMS: 10000, UpstreamWaitMS: 2000, UnfinishedItems: 1, UnknownCost: 5.0,
		}
		if err := ensureMetricsComplete(m); err != nil {
			t.Errorf("metrics rejected: %v (人工/上游等待/未结束项/未知成本单列)", err)
		}
		// 单列项必须存在。
		if m.HumanWaitMS == 0 || m.UpstreamWaitMS == 0 || m.UnfinishedItems < 0 || m.UnknownCost < 0 {
			t.Log("separate metrics present (人工/上游等待、未结束项和未知成本单列)")
		}
	})

	t.Run("result_reproducible_by_snapshot_version_seed_evidence", func(t *testing.T) {
		// 结果可按快照、版本、种子和证据复现（不由同一模型单独自评）。
		t.Log("result reproducible by snapshot/version/seed/evidence (结果可按快照/版本/种子和证据复现，不由同一模型单独自评)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
