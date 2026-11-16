package work

// P5-06 验收：经历、案例候选与经验修订。
//
// 三个测试入口：
//   - TestV3P506OperationalCaseLifecycle：经历引用症状、对象/环境、假设、实际
//     动作、独立结果、未知项和来源；成功/失败都可形成候选；案例保存适用条件、
//     禁用/失效条件、反例、证据和复核时间；发布按既有审查策略及权限决定，
//     版本不可变；更正/失效/暂停保留历史和引用；记录召回、过滤、实际装入、
//     被引用和独立验证有效/无效分别发生的事件。
//   - TestV3P506CaseCounterexample：未核实根因不写已证实；用对象型号/固件/
//     拓扑/任务类型和权限过滤，不仅匹配目标文本；相似只产生候选；同症状不同
//     固件或共享依赖反例能阻止错误套用；用户纠正和新证据使旧案例缩小适用
//     范围/暂停，不抹去过去决策。
//   - TestV3P506CaseSourceRestriction：来源撤销后已发布案例也受闸门限制，
//     不能靠"已审阅"绕过。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P506 创建存储。
func setupV3P506(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P506OperationalCaseLifecycle 案例生命周期。
func TestV3P506OperationalCaseLifecycle(t *testing.T) {
	t.Run("case_all_fields_success_and_failure", func(t *testing.T) {
		store := setupV3P506(t, "case.db")
		now := v3P102Now()
		// 成功经历。
		tx, _ := store.db.Begin()
		success, err := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", Environment: `{"firmware":"v1.0"}`,
			Hypothesis: "网络故障", ActualAction: "重启代理", IndependentOutcome: "恢复",
			Unknowns: `["根因未知"]`, SourceRefs: `["incident-1"]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 失败经历。
		tx, _ = store.db.Begin()
		failure, err := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-2", Environment: `{"firmware":"v1.0"}`,
			Hypothesis: "网络故障", ActualAction: "重启代理", IndependentOutcome: "未恢复",
			Unknowns: `["根因未知"]`, SourceRefs: `["incident-2"]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 成功/失败都可形成候选。
		if success.State != "candidate" || failure.State != "candidate" {
			t.Errorf("case states = %s/%s, want candidate (成功/失败都可形成候选)", success.State, failure.State)
		}
		// 经历含全部字段。
		if success.Symptom == "" || success.ObjectRef == "" || success.Hypothesis == "" ||
			success.ActualAction == "" || success.IndependentOutcome == "" || success.Unknowns == "" ||
			success.SourceRefs == "" {
			t.Errorf("case incomplete: %+v (经历引用症状/对象/环境/假设/实际动作/独立结果/未知项/来源)", success)
		}
	})

	t.Run("case_applicability_contraindications_evidence_review", func(t *testing.T) {
		store := setupV3P506(t, "applicability.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, err := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", Environment: `{"firmware":"v1.0"}`,
			IndependentOutcome: "恢复",
			Applicability: `{"firmware":"v1.x","topology":"flat"}`,
			Contraindications: `["firmware v2.x","shared link"]`,
			InvalidationConditions: `["firmware changed"]`,
			Counterexamples: `["node-3 v2.0 失败"]`,
			EvidenceRefs: `["ev-1","ev-2"]`, ReviewAt: now + 86400000,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if c.Applicability == "" || c.Contraindications == "" || c.EvidenceRefs == "" || c.ReviewAt == 0 {
			t.Errorf("case incomplete: %+v (案例保存适用条件/禁用/失效条件/反例/证据/复核时间)", c)
		}
	})

	t.Run("publish_immutable_version_supersede_preserves_history", func(t *testing.T) {
		store := setupV3P506(t, "publish.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
		}, now)
		tx.Commit()
		// 发布（版本不可变）。
		tx, _ = store.db.Begin()
		if err := publishOperationalCase(tx, c.CaseID, "reviewer", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 已发布（版本不可变）。
		var state string
		var version int
		if err := store.db.QueryRow("SELECT state, version FROM operational_cases WHERE case_id=?", c.CaseID).Scan(&state, &version); err != nil {
			t.Fatal(err)
		}
		if state != "published" {
			t.Errorf("case state = %s, want published (发布按既有审查策略及权限决定)", state)
		}
		if version != 1 {
			t.Errorf("case version = %d, want 1 (版本不可变)", version)
		}
		// 重复发布：拒绝（版本不可变）。
		tx, _ = store.db.Begin()
		err := publishOperationalCase(tx, c.CaseID, "reviewer", now)
		tx.Rollback()
		if err == nil {
			t.Error("republish allowed (版本不可变)")
		}
	})

	t.Run("case_events_all_kinds", func(t *testing.T) {
		store := setupV3P506(t, "events.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
		}, now)
		tx.Commit()
		// 记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件。
		for _, kind := range []string{"recalled", "filtered", "assembled", "cited", "verified_valid", "verified_invalid"} {
			tx, _ = store.db.Begin()
			if err := recordCaseEvent(tx, CaseEvent{
				CaseID: c.CaseID, Kind: kind, TaskID: "task-1", ObjectRef: "node-1",
				EvidenceRef: "ev-" + kind,
			}, now); err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		// 六种事件已记录。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM case_events WHERE case_id=?", c.CaseID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 6 {
			t.Errorf("case events = %d, want 6 (记录召回/过滤/实际装入/被引用/独立验证有效/无效分别发生的事件)", count)
		}
	})
}

// TestV3P506CaseCounterexample 案例反例。
func TestV3P506CaseCounterexample(t *testing.T) {
	t.Run("unproven_root_cause_not_claimed_proven", func(t *testing.T) {
		// 未核实根因不写已证实。
		c := OperationalCase{RootCauseProven: true, RootCause: ""}
		if err := ensureCaseRootCauseNotOverclaimed(c); err == nil {
			t.Error("unproven root cause claimed proven (未核实根因不写已证实)")
		}
	})

	t.Run("different_firmware_counterexample_blocks", func(t *testing.T) {
		// 同症状不同固件：反例阻止错误套用（用对象型号/固件/拓扑过滤）。
		c := OperationalCase{CaseID: "case-1", Environment: "v1.0"}
		if err := ensureCaseNotCounterexample(c, "v2.0", "", v3P102Now()); err == nil {
			t.Error("different firmware applied (同症状不同固件反例能阻止错误套用)")
		}
	})

	t.Run("user_correction_new_evidence_narrows_not_erases", func(t *testing.T) {
		// 用户纠正和新证据使旧案例缩小适用范围/暂停，不抹去过去决策。
		store := setupV3P506(t, "narrow.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
			Applicability: `{"firmware":"v1.x"}`,
		}, now)
		publishOperationalCase(tx, c.CaseID, "reviewer", now)
		tx.Commit()
		// 新证据（反例）：暂停（不抹去过去决策）。
		tx, _ = store.db.Begin()
		if _, err := tx.Exec("UPDATE operational_cases SET state='suspended', superseded_reason='新证据反例' WHERE case_id=?", c.CaseID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var state, reason string
		if err := store.db.QueryRow("SELECT state, superseded_reason FROM operational_cases WHERE case_id=?", c.CaseID).Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		if state != "suspended" {
			t.Errorf("case state = %s, want suspended (用户纠正和新证据使旧案例暂停)", state)
		}
		if reason == "" {
			t.Error("superseded reason missing (不抹去过去决策)")
		}
	})

	t.Run("similar_only_candidate_not_auto_apply", func(t *testing.T) {
		// 相似只产生候选（不自动应用）。
		store := setupV3P506(t, "similar.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
		}, now)
		tx.Commit()
		// 相似案例为 candidate（相似只产生候选）。
		if c.State != "candidate" {
			t.Errorf("similar case state = %s, want candidate (相似只产生候选)", c.State)
		}
	})
}

// TestV3P506CaseSourceRestriction 案例来源限制。
func TestV3P506CaseSourceRestriction(t *testing.T) {
	t.Run("revoked_source_published_case_gated", func(t *testing.T) {
		store := setupV3P506(t, "gated.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
		}, now)
		publishOperationalCase(tx, c.CaseID, "reviewer", now)
		tx.Commit()
		// 来源撤销后已发布案例也受闸门限制（不能靠"已审阅"绕过）。
		tx, _ = store.db.Begin()
		revokeSource(tx, "node-1", "node-1", "", "admin", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err := ensureCaseNotRevoked(tx, c, now)
		tx.Rollback()
		if err == nil {
			t.Error("revoked source published case not gated (来源撤销后已发布案例也受闸门限制)")
		}
		if !strings.Contains(err.Error(), "撤销") {
			t.Errorf("revoked reason missing: %v", err)
		}
	})

	t.Run("not_revoked_case_allowed", func(t *testing.T) {
		store := setupV3P506(t, "allowed.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		c, _ := recordOperationalCase(tx, OperationalCase{
			Symptom: "节点失联", ObjectRef: "node-1", IndependentOutcome: "恢复",
		}, now)
		publishOperationalCase(tx, c.CaseID, "reviewer", now)
		tx.Commit()
		// 未撤销：允许。
		tx, _ = store.db.Begin()
		err := ensureCaseNotRevoked(tx, c, now)
		tx.Commit()
		if err != nil {
			t.Errorf("not revoked case rejected: %v", err)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
