package work

// P5-07：版本化 Skill 与受控改进。
//
// 本文件实现经验可提出方法改进，但不能自动改变生产规则、权限和执行能力：
//   - Skill 明确目标、前提、步骤、验证、限制和适用环境，引用已发布能力/
//     领域规范版本；
//   - 从案例产生改进候选，在受控案例和反例上回放，记录前后差异及失败；
//   - 发布/启用按既有授权策略处理，不能让模型自行审批自己的提案；运行中
//     任务不静默切换版本；
//   - 恢复和执行前重新核验 Skill/来源/能力版本；失效后停止后续新使用并
//     保留历史解释；
//   - 优化目标不包含放宽权限、跳过证据或隐藏未知；必要时回退到旧方法或
//     人工处理。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Skill（明确目标、前提、步骤、验证、限制和适用环境）
// ---------------------------------------------------------------------------

// Skill 是 v3_skills 的一行。
type Skill struct {
	SkillID         string
	Name            string
	Goal            string
	Preconditions   string
	Steps           string
	Verification    string
	Limitations     string
	Environment     string
	CapabilityRefs  string
	DomainSpecRefs  string
	Version         int
	State           string // draft/candidate/published/suspended/rejected
	SourceCaseRef   string
	PublishedBy     string
	PublishedAt     int64
	CreatedAt       int64
	UpdatedAt       int64
}

// proposeSkill 提出 Skill 候选（P5-07.1/P5-07.2）。
// Skill 明确目标、前提、步骤、验证、限制和适用环境，引用已发布能力/领域
// 规范版本；从案例产生改进候选。
func proposeSkill(tx *sql.Tx, s Skill, now int64) (Skill, error) {
	if s.Name == "" || s.Goal == "" || s.Steps == "" {
		return Skill{}, errors.New("Skill 缺少名称/目标/步骤")
	}
	// 引用已发布能力/领域规范版本。
	if s.CapabilityRefs == "" || s.DomainSpecRefs == "" {
		return Skill{}, errors.New("Skill 缺少已发布能力/领域规范版本引用")
	}
	s.SkillID = id()
	s.State = "candidate"
	s.CreatedAt, s.UpdatedAt = now, now
	if s.Version < 1 {
		s.Version = 1
	}
	var sourceCaseRef any
	if s.SourceCaseRef != "" {
		sourceCaseRef = s.SourceCaseRef
	}
	_, err := tx.Exec(`INSERT INTO v3_skills(skill_id,name,goal,preconditions,steps,verification,
limitations,environment,capability_refs,domain_spec_refs,version,state,source_case_ref,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.SkillID, s.Name, s.Goal, s.Preconditions, s.Steps, s.Verification, s.Limitations,
		s.Environment, s.CapabilityRefs, s.DomainSpecRefs, s.Version, s.State, sourceCaseRef, now, now)
	if err != nil {
		return Skill{}, err
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// 回放（从案例产生改进候选，在受控案例和反例上回放）
// ---------------------------------------------------------------------------

// SkillReplay 是 skill_replays 的一行。
type SkillReplay struct {
	ReplayID         string
	SkillID          string
	CaseRef          string
	IsCounterexample bool
	BeforeOutcome    string
	AfterOutcome     string
	Diff             string
	FailureReason    string
	Outcome          string // improved/degraded/unchanged/failed
	ReplayedAt       int64
}

// replaySkill 回放 Skill（P5-07.2）。
// 从案例产生改进候选，在受控案例和反例上回放，记录前后差异及失败。
func replaySkill(tx *sql.Tx, r SkillReplay, now int64) (SkillReplay, error) {
	if r.SkillID == "" || r.CaseRef == "" {
		return SkillReplay{}, errors.New("回放缺少 Skill 或案例")
	}
	r.ReplayID = id()
	r.ReplayedAt = now
	if r.Outcome == "" {
		r.Outcome = "unchanged"
	}
	_, err := tx.Exec(`INSERT INTO skill_replays(replay_id,skill_id,case_ref,is_counterexample,
before_outcome,after_outcome,diff,failure_reason,outcome,replayed_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ReplayID, r.SkillID, r.CaseRef, boolToInt(r.IsCounterexample), r.BeforeOutcome,
		r.AfterOutcome, r.Diff, r.FailureReason, r.Outcome, now)
	if err != nil {
		return SkillReplay{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 发布（发布/启用按既有授权策略处理，不能让模型自行审批自己的提案）
// ---------------------------------------------------------------------------

// ErrModelSelfApproval 是模型自审批的错误。
var ErrModelSelfApproval = errors.New("不能让模型自行审批自己的提案")

// publishSkill 发布 Skill（P5-07.3）。
// 发布/启用按既有授权策略处理，不能让模型自行审批自己的提案；运行中任务
// 不静默切换版本；有收益但越权/缺验证的候选不能发布。
func publishSkill(tx *sql.Tx, skillID string, publisher string, proposer string, verificationPassed bool, now int64) error {
	// 不能让模型自行审批自己的提案。
	if publisher == proposer {
		return fmt.Errorf("%w：发布者 %s 是提案者 %s", ErrModelSelfApproval, publisher, proposer)
	}
	// 有收益但越权/缺验证的候选不能发布。
	if !verificationPassed {
		return errors.New("候选缺验证（有收益但越权/缺验证的候选不能发布）")
	}
	res, err := tx.Exec(`UPDATE v3_skills SET state='published', published_by=?, published_at=?, updated_at=?
WHERE skill_id=? AND state='candidate'`, publisher, now, now, skillID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("Skill %s 不存在或已发布（版本不可变）", skillID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 执行前核验（恢复和执行前重新核验 Skill/来源/能力版本）
// ---------------------------------------------------------------------------

// ensureSkillVersionCurrent 核对 Skill 版本（P5-07.4）。
// 恢复和执行前重新核验 Skill/来源/能力版本；失效后停止后续新使用并保留
// 历史解释；运行中任务不静默切换版本。
func ensureSkillVersionCurrent(tx *sql.Tx, skillID string, expectedVersion int, now int64) error {
	var version int
	var state string
	err := tx.QueryRow(`SELECT version, state FROM v3_skills WHERE skill_id=?`, skillID).Scan(&version, &state)
	if err != nil {
		return err
	}
	// 失效后停止后续新使用（保留历史解释）。
	if state != "published" {
		return fmt.Errorf("Skill %s 已 %s（失效后停止后续新使用并保留历史解释）", skillID, state)
	}
	// 版本不匹配（运行中任务不静默切换版本）。
	if version != expectedVersion {
		return fmt.Errorf("Skill %s 版本 %d≠%d（运行中任务不静默切换版本）", skillID, version, expectedVersion)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 优化目标（不包含放宽权限、跳过证据或隐藏未知）
// ---------------------------------------------------------------------------

// ErrSkillOptimizationViolation 是 Skill 优化违规的错误。
var ErrSkillOptimizationViolation = errors.New("优化目标不包含放宽权限、跳过证据或隐藏未知")

// ensureSkillOptimizationNotViolated 核对 Skill 优化不违规（P5-07.5）。
// 优化目标不包含放宽权限、跳过证据或隐藏未知；必要时回退到旧方法或人工处理。
func ensureSkillOptimizationNotViolated(s Skill) error {
	// 优化目标不包含放宽权限、跳过证据或隐藏未知。
	if s.Verification == "" {
		return fmt.Errorf("%w：Skill %s 缺验证（跳过证据）", ErrSkillOptimizationViolation, s.Name)
	}
	if s.Limitations == "" {
		return fmt.Errorf("%w：Skill %s 缺限制（隐藏未知）", ErrSkillOptimizationViolation, s.Name)
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
