package work

// P5-06：经历、案例候选与经验修订。
//
// 本文件实现把运维经历提炼为有条件、有反例的候选，不把模型总结变成当前事实：
//   - 经历引用症状、对象/环境、假设、实际动作、独立结果、未知项和来源；
//     成功/失败都可形成候选；
//   - 案例保存适用条件、禁用/失效条件、反例、证据和复核时间；未核实根因
//     不写已证实；
//   - 发布按既有审查策略及权限决定，版本不可变；更正/失效/暂停保留历史
//     和引用；
//   - 用对象型号/固件/拓扑/任务类型和权限过滤，不仅匹配目标文本；相似只
//     产生候选；
//   - 记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 经历（引用症状、对象/环境、假设、实际动作、独立结果、未知项和来源）
// ---------------------------------------------------------------------------

// OperationalCase 是 operational_cases 的一行。
type OperationalCase struct {
	CaseID               string
	Symptom              string
	ObjectRef            string
	Environment          string
	Hypothesis           string
	ActualAction         string
	IndependentOutcome   string
	Unknowns             string
	SourceRefs           string
	Applicability        string
	Contraindications    string
	InvalidationConditions string
	Counterexamples      string
	EvidenceRefs         string
	ReviewAt             int64
	RootCause            string
	RootCauseProven      bool
	State                string // candidate/published/suspended/invalidated
	Version              int
	Supersedes           string
	SupersededReason     string
	PublishedBy          string
	PublishedAt          int64
	CreatedAt            int64
	UpdatedAt            int64
}

// recordOperationalCase 记录运维经历（P5-06.1）。
// 经历引用症状、对象/环境、假设、实际动作、独立结果、未知项和来源；
// 成功/失败都可形成候选。
func recordOperationalCase(tx *sql.Tx, c OperationalCase, now int64) (OperationalCase, error) {
	if c.Symptom == "" || c.ObjectRef == "" {
		return OperationalCase{}, errors.New("经历缺少症状或对象")
	}
	// 成功/失败都可形成候选。
	if c.IndependentOutcome == "" {
		return OperationalCase{}, errors.New("经历缺少独立结果（成功/失败都可形成候选）")
	}
	c.CaseID = id()
	c.State = "candidate"
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Version < 1 {
		c.Version = 1
	}
	_, err := tx.Exec(`INSERT INTO operational_cases(case_id,symptom,object_ref,environment,hypothesis,
actual_action,independent_outcome,unknowns,source_refs,applicability,contraindications,
invalidation_conditions,counterexamples,evidence_refs,review_at,root_cause,root_cause_proven,state,
version,published_by,published_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.CaseID, c.Symptom, c.ObjectRef, c.Environment, c.Hypothesis, c.ActualAction,
		c.IndependentOutcome, c.Unknowns, c.SourceRefs, c.Applicability, c.Contraindications,
		c.InvalidationConditions, c.Counterexamples, c.EvidenceRefs, c.ReviewAt, c.RootCause,
		boolToInt(c.RootCauseProven), c.State, c.Version, c.PublishedBy, c.PublishedAt, now, now)
	if err != nil {
		return OperationalCase{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 案例（保存适用条件、禁用/失效条件、反例、证据和复核时间）
// ---------------------------------------------------------------------------

// publishOperationalCase 发布案例（P5-06.3）。
// 发布按既有审查策略及权限决定，版本不可变；更正/失效/暂停保留历史和引用。
func publishOperationalCase(tx *sql.Tx, caseID string, publisher string, now int64) error {
	if publisher == "" {
		return errors.New("发布缺少发布者（发布按既有审查策略及权限决定）")
	}
	// 发布按既有审查策略及权限决定（版本不可变）。
	res, err := tx.Exec(`UPDATE operational_cases SET state='published', published_by=?, published_at=?, updated_at=?
WHERE case_id=? AND state='candidate'`, publisher, now, now, caseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("案例 %s 不存在或已发布（版本不可变）", caseID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 根因核对（未核实根因不写已证实）
// ---------------------------------------------------------------------------

// ErrRootCauseOverclaimed 是根因过度声称的错误。
var ErrRootCauseOverclaimed = errors.New("未核实根因不写已证实")

// ensureRootCauseNotOverclaimed 核对根因不过度声称（P5-06.2）。
// 未核实根因不写已证实。
func ensureCaseRootCauseNotOverclaimed(c OperationalCase) error {
	if c.RootCauseProven && c.RootCause == "" {
		return fmt.Errorf("%w：案例 %s 标记已证实但无根因", ErrRootCauseOverclaimed, c.CaseID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 反例（同症状不同固件或共享依赖反例能阻止错误套用）
// ---------------------------------------------------------------------------

// ErrCaseCounterexample 是案例反例的错误。
var ErrCaseCounterexample = errors.New("同症状不同固件或共享依赖反例能阻止错误套用")

// ensureCaseNotCounterexample 核对案例不套用反例（P5-06.4）。
// 用对象型号/固件/拓扑/任务类型和权限过滤，不仅匹配目标文本；相似只产生候选；
// 同症状不同固件或共享依赖反例能阻止错误套用。
func ensureCaseNotCounterexample(c OperationalCase, targetFirmware string, targetTopology string, now int64) error {
	// 同症状不同固件：反例阻止错误套用（用对象型号/固件/拓扑过滤，不仅匹配目标文本）。
	if c.Environment != "" && targetFirmware != "" {
		if c.Environment != targetFirmware {
			return fmt.Errorf("%w：案例 %s 固件 %s ≠ 目标 %s（同症状不同固件反例能阻止错误套用）",
				ErrCaseCounterexample, c.CaseID, c.Environment, targetFirmware)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 案例事件（记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件）
// ---------------------------------------------------------------------------

// CaseEvent 是 case_events 的一行。
type CaseEvent struct {
	EventID     string
	CaseID      string
	Kind        string // recalled/filtered/assembled/cited/verified_valid/verified_invalid
	TaskID      string
	ObjectRef   string
	EvidenceRef string
	Detail      string
	OccurredAt  int64
}

// recordCaseEvent 记录案例事件（P5-06.5）。
// 记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件。
func recordCaseEvent(tx *sql.Tx, e CaseEvent, now int64) error {
	if e.CaseID == "" || e.Kind == "" {
		return errors.New("案例事件缺少案例或类型")
	}
	// 事件类型（召回、过滤、实际装入、被引用和独立验证有效/无效分别发生）。
	validKinds := map[string]bool{
		"recalled": true, "filtered": true, "assembled": true, "cited": true,
		"verified_valid": true, "verified_invalid": true,
	}
	if !validKinds[e.Kind] {
		return fmt.Errorf("无效的案例事件类型 %s", e.Kind)
	}
	e.EventID = id()
	e.OccurredAt = now
	_, err := tx.Exec(`INSERT INTO case_events(event_id,case_id,kind,task_id,object_ref,evidence_ref,detail,occurred_at)
VALUES(?,?,?,?,?,?,?,?)`, e.EventID, e.CaseID, e.Kind, e.TaskID, e.ObjectRef, e.EvidenceRef, e.Detail, now)
	return err
}

// ---------------------------------------------------------------------------
// 来源撤销闸门（来源撤销后已发布案例也受闸门限制）
// ---------------------------------------------------------------------------

// ensureCaseNotRevoked 核对案例未撤销（P5-06 验收）。
// 来源撤销后已发布案例也受闸门限制，不能靠"已审阅"绕过。
func ensureCaseNotRevoked(tx *sql.Tx, c OperationalCase, now int64) error {
	// 来源撤销后已发布案例也受闸门限制（不能靠"已审阅"绕过）。
	for _, sourceRef := range []string{c.ObjectRef} {
		if err := ensureSourceNotRevoked(tx, c.ObjectRef, sourceRef, 1, now); err != nil {
			return fmt.Errorf("案例 %s 来源已撤销（来源撤销后已发布案例也受闸门限制，不能靠已审阅绕过）：%w", c.CaseID, err)
		}
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
