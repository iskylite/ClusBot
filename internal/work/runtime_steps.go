package work

// P1-04：Step 依赖与确定性流程基础。
//
// 本文件把 Task 内步骤、依赖与等待从父子任务模型中独立出来：
//   - 实现模型步骤、确定性步骤和能力步骤的类型及输入/结果引用，每步绑定
//     合同与定义版本（P1-04.1）；
//   - 复用 DAG 无环与缺失依赖校验；父子 Task 不再是表达每个内部步骤的
//     唯一方式（P1-04.2）；
//   - 仅当必需依赖完成且证据版本有效时生成就绪单元；多个独立步骤可就绪（P1-04.3）；
//   - 上游证据变化使依赖重新核对，不删除旧结果；不可满足依赖产生明确等待/
//     失败原因（P1-04.4）；
//   - 确定性步骤无需调用模型；只写计划 JSON 而没有实际推进不能验收（P1-04.5）。
//
// 与现有工作流的关系：保留真正独立的子 Task（有独立目标/验收）；普通流程
// 步骤使用本文件的 Step。已有个人成果工作流继续核验已验收成果。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// Step 类型（P1-04.1）
// ---------------------------------------------------------------------------

// StepKind 是步骤类型。
type StepKind string

const (
	// StepKindModel：需要模型判断的步骤（产生模型 work_unit）。
	StepKindModel StepKind = "model"
	// StepKindDeterministic：规则可确定推进的步骤（无需模型调用）。
	StepKindDeterministic StepKind = "deterministic"
	// StepKindCapability：能力步骤（经 Operation 派发，P3 接入）。
	StepKindCapability StepKind = "capability"
)

// StepDefinition 是一个步骤的定义（绑定合同与定义版本）。
type StepDefinition struct {
	Key       string
	Kind      StepKind
	Version   int
	Condition string // 完成条件
	Optional  bool
	// Input 是输入引用（大内容走材料引用，不内嵌）。
	InputRef string
	// DeterministicHandler 是确定性步骤的处理器标识（StepKindDeterministic 必填）。
	DeterministicHandler string
	// CapabilityRef 是能力步骤的能力引用（StepKindCapability 必填，P3 接入）。
	CapabilityRef string
}

// validate 校验定义完整性。
func (d StepDefinition) validate() error {
	if d.Key == "" {
		return errors.New("步骤定义缺少 key")
	}
	switch d.Kind {
	case StepKindDeterministic:
		if d.DeterministicHandler == "" {
			return fmt.Errorf("确定性步骤 %s 缺少处理器标识", d.Key)
		}
	case StepKindCapability:
		if d.CapabilityRef == "" {
			return fmt.Errorf("能力步骤 %s 缺少能力引用", d.Key)
		}
	case StepKindModel:
	default:
		return fmt.Errorf("未识别的步骤类型 %q", d.Kind)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step 计划与 DAG 校验（P1-04.2）
// ---------------------------------------------------------------------------

// StepPlan 是一个 Task 的步骤计划（DAG）。
type StepPlan struct {
	TaskID           string
	ContractRevision int
	Steps            []StepDefinition
	// Dependencies 是 key → 依赖的 key 集合。
	Dependencies map[string][]string
}

// validate 复用 DAG 无环与缺失依赖校验：环、自依赖、未知步骤 ID 在写入前被拒绝。
func (p StepPlan) validate() error {
	if p.TaskID == "" {
		return errors.New("计划缺少任务")
	}
	if len(p.Steps) == 0 {
		return errors.New("计划至少包含一个步骤")
	}
	index := make(map[string]int, len(p.Steps))
	for i, def := range p.Steps {
		if err := def.validate(); err != nil {
			return err
		}
		if _, dup := index[def.Key]; dup {
			return fmt.Errorf("重复的步骤 key %q", def.Key)
		}
		index[def.Key] = i
	}
	// 缺失依赖与自依赖校验。
	for key, deps := range p.Dependencies {
		if _, ok := index[key]; !ok {
			return fmt.Errorf("依赖声明指向未知步骤 %q", key)
		}
		for _, dep := range deps {
			if dep == key {
				return fmt.Errorf("步骤 %q 自依赖", key)
			}
			if _, ok := index[dep]; !ok {
				return fmt.Errorf("步骤 %q 依赖未知步骤 %q", key, dep)
			}
		}
	}
	// 无环校验（三色遍历）。
	const (
		white = 0 // 未访问
		gray  = 1 // 访问中
		black = 2 // 已完成
	)
	color := make(map[string]int, len(p.Steps))
	var visit func(string) bool
	visit = func(key string) bool {
		switch color[key] {
		case gray:
			return false // 回边：环
		case black:
			return true
		}
		color[key] = gray
		for _, dep := range p.Dependencies[key] {
			if !visit(dep) {
				return false
			}
		}
		color[key] = black
		return true
	}
	for key := range index {
		if !visit(key) {
			return fmt.Errorf("步骤依赖存在环（涉及 %q）", key)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 就绪判定（P1-04.3）
// ---------------------------------------------------------------------------

// ReadyStep 是一个就绪步骤及其生成的执行单元。
type ReadyStep struct {
	Step Step
	Unit WorkUnit
}

// computeReadySteps 计算当前就绪的步骤：仅当必需依赖完成且证据版本有效时。
// 多个独立步骤可同时就绪（执行并发在 P2 完善）。
func computeReadySteps(q recordsQuerier, taskID string, contractRevision int) ([]ReadyStep, error) {
	steps, err := listSteps(q, taskID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Step, len(steps))
	for _, s := range steps {
		byID[s.StepID] = s
	}
	var ready []ReadyStep
	for _, s := range steps {
		if s.State != V3StepReady && s.State != V3StepBlocked {
			continue
		}
		// 合同隔离：旧合同版本的步骤不就绪（P1-02 的合同修订隔离）。
		if s.ContractRevision != contractRevision {
			continue
		}
		ok, reason, err := stepDependenciesSatisfied(q, s, byID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		_ = reason
		// 生成就绪单元（kind 映射步骤类型；确定性步骤在 P2-07 接入执行）。
		kind := WorkUnitModel
		if def, ok := stepDefinitionFor(q, s); ok == nil && def.Kind == StepKindDeterministic {
			kind = WorkUnitDeterministic
		}
		unit := WorkUnit{
			UnitID:    id(),
			TaskID:    taskID,
			StepID:    s.StepID,
			Kind:      kind,
			ReadyAt:   now(),
			State:     WorkUnitReady,
			InputRef:  s.InputRef,
			CreatedAt: now(),
			UpdatedAt: now(),
		}
		ready = append(ready, ReadyStep{Step: s, Unit: unit})
	}
	return ready, nil
}

// stepDependenciesSatisfied 核对一个步骤的全部必需依赖已完成且证据版本有效。
// 返回 (是否满足, 不满足原因, error)。不可满足依赖产生明确等待/失败原因。
func stepDependenciesSatisfied(q recordsQuerier, s Step, byID map[string]Step) (bool, string, error) {
	deps, err := stepDependencies(q, s.StepID)
	if err != nil {
		return false, "", err
	}
	for _, depID := range deps {
		dep, ok := byID[depID]
		if !ok {
			return false, fmt.Sprintf("依赖的步骤 %s 不存在", depID), nil
		}
		// 可选步骤的取消不阻塞；必需依赖必须完成。
		if dep.Optional && dep.State == V3StepCancelled {
			continue
		}
		if dep.State == V3StepCancelled || dep.State == V3StepFailed {
			return false, fmt.Sprintf("依赖步骤 %s 已 %s", depID, dep.State), nil
		}
		if dep.State != V3StepCompleted {
			return false, fmt.Sprintf("依赖步骤 %s 未完成（当前 %s）", depID, dep.State), nil
		}
		// 证据版本有效性：上游证据变化后须重新核对（P1-04.4）。
		if dep.EvidenceRevision != s.EvidenceRevision && s.EvidenceRevision != 0 {
			return false, fmt.Sprintf("依赖步骤 %s 的证据版本已变化（%d → %d），须重新核对",
				depID, s.EvidenceRevision, dep.EvidenceRevision), nil
		}
	}
	return true, "", nil
}

// ---------------------------------------------------------------------------
// 确定性步骤执行器（P1-04.5）
// ---------------------------------------------------------------------------

// DeterministicResult 是确定性步骤的结果。
type DeterministicResult struct {
	OutputRef string // 结果引用（材料引用）
	Summary   string
	// FollowUp 是后续事件/命令（由运行时提交，不在事务内做长计算/外部调用）。
	FollowUp []string
}

// DeterministicHandler 是确定性步骤处理器：输入有版本和范围，只能经运行时
// 提交结果和后续事件；数据库事务中不做长计算/外部调用。
type DeterministicHandler func(ctx context.Context, input DeterministicInput) (DeterministicResult, error)

// DeterministicInput 是确定性步骤的输入。
type DeterministicInput struct {
	TaskID           string
	StepID           string
	ContractRevision int
	DefinitionVersion int
	InputRef         string
}

// deterministicRegistry 是确定性处理器注册表；未知类型明确拒绝。
type deterministicRegistry struct {
	handlers map[string]DeterministicHandler
}

var registry = &deterministicRegistry{handlers: map[string]DeterministicHandler{}}

// RegisterDeterministic 注册一个确定性处理器（启动时调用）。
func RegisterDeterministic(name string, h DeterministicHandler) {
	registry.handlers[name] = h
}

// deterministicHandler 按标识取回处理器；未知类型明确拒绝。
func deterministicHandler(name string) (DeterministicHandler, error) {
	h, ok := registry.handlers[name]
	if !ok {
		return nil, fmt.Errorf("未知的确定性处理器 %q", name)
	}
	return h, nil
}

// executeDeterministicStep 执行一个确定性步骤：无需调用模型。
// 处理器在事务外交付结果；结果的持久化由运行时提交（P1-10）。
func executeDeterministicStep(ctx context.Context, step Step, def StepDefinition) (DeterministicResult, error) {
	if step.State != V3StepReady && step.State != V3StepRunning {
		return DeterministicResult{}, fmt.Errorf("步骤 %s 不就绪（当前 %s）", step.StepID, step.State)
	}
	handler, err := deterministicHandler(def.DeterministicHandler)
	if err != nil {
		return DeterministicResult{}, err
	}
	return handler(ctx, DeterministicInput{
		TaskID:            step.TaskID,
		StepID:            step.StepID,
		ContractRevision:  step.ContractRevision,
		DefinitionVersion: def.Version,
		InputRef:          step.InputRef,
	})
}

// stepDefinitionFor 读取一个 Step 的定义（从 definition_key 与注册信息还原）。
// 当前实现按 definition_key 推断类型；完整定义持久化在后续扩展。
func stepDefinitionFor(q recordsQuerier, s Step) (StepDefinition, error) {
	def := StepDefinition{Key: s.DefinitionKey, Version: s.DefinitionVersion, Condition: s.Condition}
	// 确定性处理器已注册 → deterministic；否则按模型步骤处理。
	if _, err := deterministicHandler(s.DefinitionKey); err == nil {
		def.Kind = StepKindDeterministic
		def.DeterministicHandler = s.DefinitionKey
	} else {
		def.Kind = StepKindModel
	}
	return def, nil
}

// ---------------------------------------------------------------------------
// 证据失效检测（P1-04.4）
// ---------------------------------------------------------------------------

// InvalidateDependents 在上游步骤证据变化后，使依赖它的步骤重新核对。
// 不删除旧结果；返回需要重新核对的下游步骤 ID。
func InvalidateDependents(q recordsQuerier, changedStepID string) ([]string, error) {
	// 直接依赖 changedStepID 的步骤。
	rows, err := q.Query(`SELECT step_id FROM step_dependencies WHERE depends_on_step_id=?`, changedStepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// markStepEvidenceStale 把依赖失效证据的步骤标记为需要重新核对（不删除旧结果）。
func markStepEvidenceStale(tx *sql.Tx, stepID string, newEvidenceRevision int) error {
	res, err := tx.Exec(`UPDATE steps SET state=?, evidence_revision=?, updated_at=?
WHERE step_id=? AND state IN ('ready','blocked')`,
		string(V3StepBlocked), newEvidenceRevision, now(), stepID)
	if err != nil {
		return err
	}
	_, err = res.RowsAffected()
	return err
}

// ---------------------------------------------------------------------------
// 工作流迁移适配（P1-04.5 保留独立子 Task）
// ---------------------------------------------------------------------------

// classifyWorkflowSteps 把工作流步骤分类为"独立子 Task"或"Task 内 Step"。
// 有独立目标/验收的保留子 Task；普通流程步骤使用 Step（P0-02.5 的判定）。
func classifyWorkflowSteps(plan WorkflowRequest) map[string]StepOwnership {
	out := make(map[string]StepOwnership, len(plan.Steps))
	for _, step := range plan.Steps {
		out[step.Key] = ClassifyWorkflowStep(step.Goal != "", step.Criteria != "")
	}
	return out
}

// 确保 sort 引用保留（后续排序就绪步骤时使用）。
var _ = sort.Strings
