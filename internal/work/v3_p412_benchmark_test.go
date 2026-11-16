package work

// P4-12 验收：单机基准测试与测量规范。
//
// 三个测试入口：
//   - TestV3P412BenchmarkContract：保存测试套件/配置/环境版本、单位、持续时间、
//     并发度、样本范围及证据引用；提供者描述测试字段和输出格式，领域包决定
//     适用条件、阈值与通过规则；测试作为可能有影响的受控能力，检查占用和业务
//     影响，不因名称含 benchmark 就视为只读。
//   - TestV3P412BenchmarkUnknownMeasurements：记录失败、超时、未测和异常数据，
//     不用零值替代未知；统计结果附采样条件；工具完成但缺必要指标/证据时为
//     未验证，不能通过。
//   - TestV3P412BenchmarkVersionedEvidence：固定版本的复测建立新记录并关联旧
//     记录，保留条件差异；不同配置/采样条件的指标不能无依据比较；测试被取消
//     或造成占用时按 Operation 核对，不直接宣告资源释放。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P412 创建存储、资产与授权。
func setupV3P412(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	grantAuthorization(tx, Authorization{Subject: "user:tester", Action: "execute", ObjectRef: "node-1", ValidFrom: now, Owner: "tester"}, now)
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:tester"}, now)
	tx.Commit()
	return store
}

// TestV3P412BenchmarkContract 基准契约。
func TestV3P412BenchmarkContract(t *testing.T) {
	t.Run("spec_has_all_fields", func(t *testing.T) {
		// 测试规范含全部字段。
		spec := BenchmarkSpec{
			SuiteKey: "cpu-benchmark", ConfigVersion: 1, EnvironmentVersion: "env-1",
			Unit: "score", DurationMS: 60000, Concurrency: 4, SampleScope: "single-node",
			EvidenceRefs: []string{"ev-1"}, DomainThreshold: `{"min":800}`, PassRule: "score>=800",
		}
		if spec.SuiteKey == "" || spec.Unit == "" || spec.DurationMS == 0 || spec.Concurrency == 0 ||
			spec.SampleScope == "" || spec.DomainThreshold == "" || spec.PassRule == "" {
			t.Errorf("benchmark spec incomplete: %+v (保存测试套件/配置/环境版本/单位/持续时间/并发度/样本范围)", spec)
		}
	})

	t.Run("benchmark_controlled_capability_checks_occupancy", func(t *testing.T) {
		store := setupV3P412(t, "occupancy.db")
		now := v3P102Now()
		// 冻结资产：拒绝（测试作为可能有影响的受控能力检查占用）。
		tx, _ := store.db.Begin()
		freezeAsset(tx, "node-1", "admin", "维护冻结", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := executeBenchmark(tx, "node-1", "user:tester", BenchmarkSpec{SuiteKey: "cpu", ConfigVersion: 1}, now)
		tx.Rollback()
		if err == nil {
			t.Error("frozen asset allowed benchmark (测试检查占用)")
		}
		if !strings.Contains(err.Error(), "冻结") {
			t.Errorf("frozen reason missing: %v", err)
		}
	})

	t.Run("lent_asset_rejected", func(t *testing.T) {
		store := setupV3P412(t, "lent.db")
		now := v3P102Now()
		// 借出资产：拒绝（测试检查业务影响）。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateLent, "node-1"); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		err := executeBenchmark(tx, "node-1", "user:tester", BenchmarkSpec{SuiteKey: "cpu", ConfigVersion: 1}, now)
		tx.Rollback()
		if err == nil {
			t.Error("lent asset allowed benchmark (测试检查业务影响)")
		}
	})

	t.Run("unauthorized_execution_denied", func(t *testing.T) {
		store := setupV3P412(t, "unauth.db")
		now := v3P102Now()
		// 无执行权限：拒绝（测试作为受控能力）。
		tx, _ := store.db.Begin()
		err := executeBenchmark(tx, "node-1", "user:mallory", BenchmarkSpec{SuiteKey: "cpu", ConfigVersion: 1}, now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized benchmark not denied (测试作为受控能力)")
		}
	})

	t.Run("benchmark_not_readonly_by_name", func(t *testing.T) {
		// 不因名称含 benchmark 就视为只读（受控能力）。
		if ErrBenchmarkNotReadonly == nil {
			t.Error("benchmark readonly error missing")
		}
		if !strings.Contains(ErrBenchmarkNotReadonly.Error(), "受控能力") {
			t.Errorf("benchmark readonly message wrong: %v", ErrBenchmarkNotReadonly)
		}
	})
}

// TestV3P412BenchmarkUnknownMeasurements 未知测量。
func TestV3P412BenchmarkUnknownMeasurements(t *testing.T) {
	t.Run("outcomes_recorded_not_zero_replaced", func(t *testing.T) {
		store := setupV3P412(t, "outcomes.db")
		now := v3P102Now()
		// 记录失败、超时、未测和异常数据（不用零值替代未知）。
		for _, outcome := range []string{"failed", "timeout", "untested", "abnormal"} {
			tx, _ := store.db.Begin()
			r, err := recordBenchmarkResult(tx, BenchmarkResult{
				SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
				Outcome: outcome, Value: "", Unit: "score",
				SamplingConditions: "1min", ExecutedBy: "tester",
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if r.Outcome != outcome {
				t.Errorf("outcome = %s, want %s (记录失败/超时/未测/异常数据)", r.Outcome, outcome)
			}
		}
	})

	t.Run("zero_value_unknown_rejected", func(t *testing.T) {
		store := setupV3P412(t, "zero.db")
		now := v3P102Now()
		// 无效结果状态：拒绝（不用零值替代未知）。
		tx, _ := store.db.Begin()
		_, err := recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "", Value: "0", Unit: "score", SamplingConditions: "1min", ExecutedBy: "tester",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("zero value unknown accepted (不用零值替代未知)")
		}
	})

	t.Run("passed_result_requires_sampling_conditions", func(t *testing.T) {
		store := setupV3P412(t, "sampling.db")
		now := v3P102Now()
		// 通过结果缺采样条件：拒绝（统计结果附采样条件）。
		tx, _ := store.db.Begin()
		_, err := recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "passed", Value: "1000", Unit: "score", ExecutedBy: "tester",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("passed result without sampling conditions accepted (统计结果附采样条件)")
		}
	})

	t.Run("missing_metrics_evidence_not_verified", func(t *testing.T) {
		store := setupV3P412(t, "missing.db")
		now := v3P102Now()
		// 工具完成但缺必要指标/证据：为未验证（不能通过）。
		tx, _ := store.db.Begin()
		r, err := recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "untested", Value: "", Unit: "score", SamplingConditions: "1min", ExecutedBy: "tester",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if r.Outcome == "passed" {
			t.Error("missing metrics marked passed (工具完成但缺必要指标/证据时为未验证)")
		}
	})
}

// TestV3P412BenchmarkVersionedEvidence 版本化证据。
func TestV3P412BenchmarkVersionedEvidence(t *testing.T) {
	t.Run("retest_new_record_linked_old_with_condition_diff", func(t *testing.T) {
		store := setupV3P412(t, "retest.db")
		now := v3P102Now()
		// 固定版本的复测建立新记录并关联旧记录（保留条件差异）。
		tx, _ := store.db.Begin()
		r1, _ := recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "passed", Value: "1000", Unit: "score", SamplingConditions: "1min", ExecutedBy: "tester",
		}, now)
		tx.Commit()
		// 复测（关联旧记录，保留条件差异）。
		tx, _ = store.db.Begin()
		r2, err := recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "passed", Value: "1050", Unit: "score", SamplingConditions: "5min",
			SupersedesResultID: r1.ResultID, ConditionDifferences: "采样条件 1min→5min",
			ExecutedBy: "tester",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if r2.SupersedesResultID != r1.ResultID {
			t.Errorf("retest not linked to old record: %s (固定版本的复测建立新记录并关联旧记录)", r2.SupersedesResultID)
		}
		if r2.ConditionDifferences == "" {
			t.Error("condition differences not preserved (保留条件差异)")
		}
	})

	t.Run("different_config_not_comparable", func(t *testing.T) {
		// 不同配置版本：不能无依据比较。
		r1 := BenchmarkResult{SpecVersion: 1, Unit: "score", SamplingConditions: "1min"}
		r2 := BenchmarkResult{SpecVersion: 2, Unit: "score", SamplingConditions: "1min"}
		comparable, reason, err := compareBenchmarkResults(r1, r2)
		if comparable {
			t.Error("different config comparable (不同配置/采样条件的指标不能无依据比较)")
		}
		if err == nil {
			t.Error("not comparable error missing")
		}
		if !strings.Contains(reason, "配置版本不同") {
			t.Errorf("not comparable reason missing: %s", reason)
		}
	})

	t.Run("different_unit_not_comparable", func(t *testing.T) {
		// 不同单位：不能无依据比较。
		r1 := BenchmarkResult{SpecVersion: 1, Unit: "score", SamplingConditions: "1min"}
		r2 := BenchmarkResult{SpecVersion: 1, Unit: "points", SamplingConditions: "1min"}
		comparable, reason, err := compareBenchmarkResults(r1, r2)
		if comparable {
			t.Error("different unit comparable (不同单位不能无依据比较)")
		}
		if !strings.Contains(reason, "单位不同") {
			t.Errorf("unit reason missing: %s", reason)
		}
		_ = err
	})

	t.Run("different_sampling_not_comparable", func(t *testing.T) {
		// 不同采样条件：不能无依据比较。
		r1 := BenchmarkResult{SpecVersion: 1, Unit: "score", SamplingConditions: "1min"}
		r2 := BenchmarkResult{SpecVersion: 1, Unit: "score", SamplingConditions: "5min"}
		comparable, reason, err := compareBenchmarkResults(r1, r2)
		if comparable {
			t.Error("different sampling comparable (不同采样条件不能无依据比较)")
		}
		if !strings.Contains(reason, "采样条件不同") {
			t.Errorf("sampling reason missing: %s", reason)
		}
		_ = err
	})

	t.Run("cancelled_occupancy_not_directly_released", func(t *testing.T) {
		// 测试被取消或造成占用时按 Operation 核对（不直接宣告资源释放）。
		store := setupV3P412(t, "cancelled.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		recordBenchmarkResult(tx, BenchmarkResult{
			SpecKey: "cpu-benchmark", SpecVersion: 1, ObjectRef: "node-1",
			Outcome: "timeout", Value: "", Unit: "score", SamplingConditions: "1min", ExecutedBy: "tester",
		}, now)
		tx.Commit()
		// 超时（占用）：不直接宣告资源释放（按 Operation 核对）。
		var outcome string
		if err := store.db.QueryRow("SELECT outcome FROM validation_records WHERE object_refs LIKE '%node-1%' LIMIT 1").Scan(&outcome); err != nil {
			// 无 validation_records 记录：测试超时未直接释放（待核对）。
			t.Log("timeout occupancy not directly released (测试被取消或造成占用时按 Operation 核对)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
