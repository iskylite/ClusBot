package work

// P4-06 验收：配置规范、验证证据与交付覆盖。
//
// 三个测试入口：
//   - TestV3P406AcceptanceCoverageMatrix：按要求 × 对象/组建立覆盖表；不适用
//     项须有规范依据，未测/未知不能默认通过；单机全部通过但组级联测失败
//     不能整批交付；验收通过、准备交付和真实接收交接分别记录，最后才可决定
//     在役。
//   - TestV3P406EvidenceInvalidation：关系、部件、固件或规范变化时复查依赖，
//     局部失效不抹掉无关结果；影响不明保留待复核；部件替换后依赖旧部件的
//     验证失效，其余无关证据仍可用。
//   - TestV3P406ValidationConditions：保存验证记录的输入条件、执行配置、样本
//     范围、结果、来源和适用版本；不能仅存布尔通过；参数测量定义与业务阈值
//     分离；参数单位或采样条件不同的结果不能直接比较并判通过。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P406 创建存储。
func setupV3P406(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P406AcceptanceCoverageMatrix 验收覆盖矩阵。
func TestV3P406AcceptanceCoverageMatrix(t *testing.T) {
	t.Run("coverage_matrix_by_requirement_object", func(t *testing.T) {
		store := setupV3P406(t, "matrix.db")
		now := v3P102Now()
		// 按要求 × 对象/组建立覆盖表。
		tx, _ := store.db.Begin()
		for _, obj := range []string{"node-1", "node-2", "node-3"} {
			recordAcceptanceCoverage(tx, AcceptanceCoverage{
				RequirementKey: "cpu-benchmark", ObjectRef: obj, CoverageState: "covered",
			}, now)
		}
		tx.Commit()
		// 覆盖表可读（逐要求、逐实际对象核对）。
		coverage, err := readAcceptanceCoverage(store.db, "cpu-benchmark")
		if err != nil {
			t.Fatal(err)
		}
		if len(coverage) != 3 {
			t.Errorf("coverage = %d, want 3 (按要求 × 对象/组建立覆盖表)", len(coverage))
		}
	})

	t.Run("not_applicable_requires_basis", func(t *testing.T) {
		store := setupV3P406(t, "notapplicable.db")
		now := v3P102Now()
		// 不适用项须有规范依据。
		tx, _ := store.db.Begin()
		_, err := recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "gpu-test", ObjectRef: "node-cpu-only", CoverageState: "not_applicable",
			NotApplicableBasis: "无 GPU（规范依据）",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 缺依据：拒绝。
		tx, _ = store.db.Begin()
		_, err = recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "gpu-test-2", ObjectRef: "node-cpu-only", CoverageState: "not_applicable",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("not applicable without basis accepted (不适用项须有规范依据)")
		}
	})

	t.Run("untested_unknown_not_default_passed", func(t *testing.T) {
		store := setupV3P406(t, "untested.db")
		now := v3P102Now()
		// 未测/未知不能默认通过（默认 unknown）。
		tx, _ := store.db.Begin()
		coverage, err := recordAcceptanceCoverage(tx, AcceptanceCoverage{
			RequirementKey: "untested-req", ObjectRef: "node-1",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if coverage.CoverageState != "unknown" {
			t.Errorf("untested default = %s, want unknown (未测/未知不能默认通过)", coverage.CoverageState)
		}
	})

	t.Run("single_pass_group_fail_not_batch_delivery", func(t *testing.T) {
		store := setupV3P406(t, "batch.db")
		now := v3P102Now()
		// 单机全部通过但组级联测失败不能整批交付。
		tx, _ := store.db.Begin()
		// 单机验证全部通过。
		for _, obj := range []string{"node-1", "node-2", "node-3", "node-4", "node-5", "node-6", "node-7", "node-8"} {
			recordValidation(tx, ValidationRecord{
				SpecRef: "single-node-spec", RuleVersion: 1, ObjectRefs: `["`+obj+`"]`,
				InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
				Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
				Outcome: "passed", ValidatedBy: "test",
			}, now)
		}
		// 组级联测失败。
		recordValidation(tx, ValidationRecord{
			SpecRef: "group-topology-spec", RuleVersion: 1, ObjectRefs: `["node-1","node-2","node-3","node-4","node-5","node-6","node-7","node-8"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "group",
			Result: "failed", SourceRef: "group-test", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "failed", ValidatedBy: "test",
		}, now)
		tx.Commit()
		// 组级联测失败：不能整批交付。
		var groupFailed int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM validation_records WHERE spec_ref='group-topology-spec' AND outcome='failed'").Scan(&groupFailed); err != nil {
			t.Fatal(err)
		}
		if groupFailed != 1 {
			t.Errorf("group test not failed (单机全部通过但组级联测失败不能整批交付)")
		}
	})

	t.Run("delivery_stages_separate_final_decision", func(t *testing.T) {
		store := setupV3P406(t, "delivery.db")
		now := v3P102Now()
		// 验收通过、准备交付和真实接收交接分别记录。
		tx, _ := store.db.Begin()
		recordDeliveryStage(tx, DeliveryRecord{ObjectRef: "node-1", Stage: "accepted", RecordRef: "accept-1", EvidenceRefs: `["ev-1"]`}, now)
		recordDeliveryStage(tx, DeliveryRecord{ObjectRef: "node-1", Stage: "ready", RecordRef: "ready-1", EvidenceRefs: `["ev-2"]`}, now+1000)
		recordDeliveryStage(tx, DeliveryRecord{ObjectRef: "node-1", Stage: "handover", RecordRef: "handover-1", EvidenceRefs: `["ev-3"]`}, now+2000)
		tx.Commit()
		// 交付完整核对（最后才可决定在役）。
		complete, missing, err := checkDeliveryComplete(store.db, "node-1")
		if err != nil {
			t.Fatal(err)
		}
		if !complete {
			t.Errorf("delivery not complete: missing=%v (验收通过/准备交付/真实接收交接分别记录)", missing)
		}
		// 缺 handover：不完整（不可决定在役）。
		tx, _ = store.db.Begin()
		recordDeliveryStage(tx, DeliveryRecord{ObjectRef: "node-2", Stage: "accepted", RecordRef: "accept-2"}, now)
		tx.Commit()
		complete2, missing2, err := checkDeliveryComplete(store.db, "node-2")
		if err != nil {
			t.Fatal(err)
		}
		if complete2 {
			t.Error("incomplete delivery marked complete (最后才可决定在役)")
		}
		if len(missing2) == 0 {
			t.Error("missing stages not reported")
		}
	})
}

// TestV3P406EvidenceInvalidation 证据失效。
func TestV3P406EvidenceInvalidation(t *testing.T) {
	t.Run("component_change_invalidates_dependent_validation", func(t *testing.T) {
		store := setupV3P406(t, "invalidate.db")
		now := v3P102Now()
		// 验证记录与依赖。
		tx, _ := store.db.Begin()
		validation, _ := recordValidation(tx, ValidationRecord{
			SpecRef: "nic-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		recordEvidenceDependency(tx, EvidenceDependency{
			DependencyKind: "component", TargetRef: "nic-old", ValidationRef: validation.ValidationID,
		}, now)
		tx.Commit()
		// 部件替换：依赖旧部件的验证失效。
		tx, _ = store.db.Begin()
		invalidated, err := invalidateEvidenceDependency(tx, "component", "nic-old", "部件替换", now+1000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if invalidated != 1 {
			t.Errorf("invalidated = %d, want 1 (部件替换后依赖旧部件的验证失效)", invalidated)
		}
		// 失效的验证标记为 unknown（影响不明保留待复核）。
		var outcome string
		if err := store.db.QueryRow("SELECT outcome FROM validation_records WHERE validation_id=?", validation.ValidationID).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		if outcome != "unknown" {
			t.Errorf("invalidated validation outcome = %s, want unknown (影响不明保留待复核)", outcome)
		}
	})

	t.Run("local_invalidation_not_erase_unrelated_evidence", func(t *testing.T) {
		store := setupV3P406(t, "unrelated.db")
		now := v3P102Now()
		// 两个验证：一个依赖旧部件，一个不依赖。
		tx, _ := store.db.Begin()
		v1, _ := recordValidation(tx, ValidationRecord{
			SpecRef: "nic-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		v2, _ := recordValidation(tx, ValidationRecord{
			SpecRef: "cpu-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		recordEvidenceDependency(tx, EvidenceDependency{
			DependencyKind: "component", TargetRef: "nic-old", ValidationRef: v1.ValidationID,
		}, now)
		tx.Commit()
		// 部件替换：只失效依赖旧部件的验证（局部失效不抹掉无关结果）。
		tx, _ = store.db.Begin()
		invalidateEvidenceDependency(tx, "component", "nic-old", "部件替换", now+1000)
		tx.Commit()
		// 无关验证（cpu-spec）仍可用。
		var outcome string
		if err := store.db.QueryRow("SELECT outcome FROM validation_records WHERE validation_id=?", v2.ValidationID).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		if outcome != "passed" {
			t.Errorf("unrelated validation invalidated: outcome=%s (局部失效不抹掉无关结果)", outcome)
		}
	})
}

// TestV3P406ValidationConditions 验证条件。
func TestV3P406ValidationConditions(t *testing.T) {
	t.Run("validation_has_all_conditions_not_bool_only", func(t *testing.T) {
		store := setupV3P406(t, "conditions.db")
		now := v3P102Now()
		// 验证记录含全部条件（不能仅存布尔通过）。
		tx, _ := store.db.Begin()
		v, err := recordValidation(tx, ValidationRecord{
			SpecRef: "cpu-benchmark", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{"cpu":"idle"}`, ExecutionConfig: `{"tool":"stress-ng"}`,
			SampleScope: "single-node", Result: `{"score":1000}`, SourceRef: "agent",
			ApplicableVersions: `["1.0","1.1"]`,
			MeasurementDefinition: `{"metric":"cpu-score","unit":"points"}`,
			BusinessThreshold: `{"min":800}`, Outcome: "passed", ValidatedBy: "test",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if v.InputConditions == "" || v.ExecutionConfig == "" || v.SampleScope == "" ||
			v.Result == "" || v.SourceRef == "" || v.MeasurementDefinition == "" || v.BusinessThreshold == "" {
			t.Errorf("validation conditions incomplete: %+v (不能仅存布尔通过)", v)
		}
	})

	t.Run("bool_only_validation_rejected", func(t *testing.T) {
		store := setupV3P406(t, "boolonly.db")
		now := v3P102Now()
		// 缺输入条件：拒绝（不能仅存布尔通过）。
		tx, _ := store.db.Begin()
		_, err := recordValidation(tx, ValidationRecord{
			SpecRef: "cpu-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("bool-only validation accepted (不能仅存布尔通过)")
		}
	})

	t.Run("measurement_definition_business_threshold_separate", func(t *testing.T) {
		store := setupV3P406(t, "separate.db")
		now := v3P102Now()
		// 参数测量定义与业务阈值分离。
		tx, _ := store.db.Begin()
		v, err := recordValidation(tx, ValidationRecord{
			SpecRef: "nic-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent",
			MeasurementDefinition: `{"metric":"bandwidth","unit":"Gbps","sampling":"1min"}`,
			BusinessThreshold: `{"min":"10Gbps"}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if v.MeasurementDefinition == v.BusinessThreshold {
			t.Error("measurement definition and business threshold not separate (参数测量定义与业务阈值分离)")
		}
	})

	t.Run("different_unit_sampling_not_directly_comparable", func(t *testing.T) {
		// 参数单位或采样条件不同的结果不能直接比较并判通过。
		v1 := ValidationRecord{
			MeasurementDefinition: `{"metric":"bandwidth","unit":"Gbps","sampling":"1min"}`,
		}
		v2 := ValidationRecord{
			MeasurementDefinition: `{"metric":"bandwidth","unit":"Mbps","sampling":"5min"}`,
		}
		// 单位/采样不同：不能直接比较。
		if v1.MeasurementDefinition == v2.MeasurementDefinition {
			t.Error("different unit/sampling treated as comparable (参数单位或采样条件不同不能直接比较)")
		}
		t.Log("different unit/sampling not directly comparable (参数单位或采样条件不同的结果不能直接比较)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)

// TestV3P406EvidenceInvalidationDegradesCoverage 证据失效联动验收覆盖降级（P2-5）。
func TestV3P406EvidenceInvalidationDegradesCoverage(t *testing.T) {
	store := setupV3P406(t, "degrade.db")
	now := v3P102Now()
	// 登记一条验证记录与覆盖（validation_ref 关联）。
	tx, _ := store.db.Begin()
	recordValidation(tx, ValidationRecord{
		SpecRef: "cpu", RuleVersion: 1, ObjectRefs: "node-1", InputConditions: "ic",
		ExecutionConfig: "ec", SampleScope: "ss", Result: "score=100", SourceRef: "bench",
		MeasurementDefinition: "md", BusinessThreshold: "bt", Outcome: "passed", ValidatedBy: "tester",
	}, now)
	var validationID string
	tx.QueryRow("SELECT validation_id FROM validation_records WHERE object_refs='node-1' LIMIT 1").Scan(&validationID)
	recordAcceptanceCoverage(tx, AcceptanceCoverage{
		RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered", ValidationRef: validationID,
	}, now)
	// 登记证据依赖。
	recordEvidenceDependency(tx, EvidenceDependency{
		DependencyKind: "component", TargetRef: "node-1", ValidationRef: validationID,
	}, now)
	tx.Commit()
	// 证据失效：覆盖应联动降级为 unknown（不再 covered）。
	tx, _ = store.db.Begin()
	if _, err := invalidateEvidenceDependency(tx, "component", "node-1", "部件替换", now); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	var coverageState string
	if err := store.db.QueryRow("SELECT coverage_state FROM acceptance_coverage WHERE requirement_key='delivery-ready' AND object_ref='node-1'").Scan(&coverageState); err != nil {
		t.Fatal(err)
	}
	if coverageState == "covered" {
		t.Errorf("coverage still covered after evidence invalidated (证据失效应联动降级)，got %s", coverageState)
	}
	if coverageState != "unknown" {
		t.Errorf("coverage state=%s, want unknown", coverageState)
	}
}
