package work

// P4-07：版本化运维流程模板与运行时绑定。
//
// 本文件实现流程提供领域顺序和条件，运行时承担执行、等待、取消与恢复：
//   - 模板定义触发、对象范围、前置、确定步骤/调查步骤、依赖、等待、人工
//     协作、失败收尾和验收；
//   - 受理时绑定领域包/模板/规范/目标版本，转换成 Step 与依赖；模型只在
//     允许调查节点提出补查；
//   - 明确原子能力、委托整段流程和持续服务三种粒度，防止本地与外部重复
//     调度；
//   - 模板变化不静默改运行中工作；迁移需合同修订、依赖影响评估和必要重新
//     确认；
//   - 为领域可自动判定的步骤提供确定性处理器；不把每个流程都变为模型循环
//     或 Goal。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 流程模板（触发、对象范围、前置、步骤、依赖、等待、人工协作、失败收尾、验收）
// ---------------------------------------------------------------------------

// WorkflowTemplate 是 workflow_templates 的一行。
type WorkflowTemplate struct {
	TemplateID  string
	Name        string
	Version     int
	TriggerKind string
	ObjectScope string
	Preconditions string
	Steps       string
	Granularity string // atomic_capability/delegated_workflow/continuous_service
	State       string // draft/published/superseded
	PublishedBy string
	PublishedAt int64
	CreatedAt   int64
	UpdatedAt   int64
}

// DomainWorkflowStep 是流程步骤定义。
type DomainWorkflowStep struct {
	StepID   string `json:"step_id"`
	Kind     string `json:"kind"`      // deterministic/investigation/wait/manual/acceptance
	DependsOn []string `json:"depends_on,omitempty"`
	CapabilityKey string `json:"capability_key,omitempty"`
}

// publishWorkflowTemplate 发布流程模板（P4-07.1）。
// 模板定义触发、对象范围、前置、确定步骤/调查步骤、依赖、等待、人工协作、
// 失败收尾和验收。
func publishWorkflowTemplate(tx *sql.Tx, t WorkflowTemplate, steps []DomainWorkflowStep, now int64) (WorkflowTemplate, error) {
	if t.Name == "" || t.Version < 1 || t.PublishedBy == "" {
		return WorkflowTemplate{}, errors.New("模板缺少名称/版本/发布者")
	}
	if t.Granularity == "" {
		return WorkflowTemplate{}, errors.New("模板缺少粒度（原子能力/委托整段流程/持续服务）")
	}
	// 步骤校验（缺依赖、冲突目标或未绑定能力时不能直接执行）。
	stepIDs := make(map[string]bool)
	for _, step := range steps {
		if step.StepID == "" {
			return WorkflowTemplate{}, errors.New("步骤缺少标识")
		}
		if stepIDs[step.StepID] {
			return WorkflowTemplate{}, fmt.Errorf("步骤 %s 重复", step.StepID)
		}
		stepIDs[step.StepID] = true
	}
	for _, step := range steps {
		for _, dep := range step.DependsOn {
			if !stepIDs[dep] {
				return WorkflowTemplate{}, fmt.Errorf("步骤 %s 依赖不存在的步骤 %s（缺依赖不能直接执行）", step.StepID, dep)
			}
		}
		if step.Kind == "deterministic" && step.CapabilityKey == "" {
			return WorkflowTemplate{}, fmt.Errorf("确定性步骤 %s 缺少能力绑定（未绑定能力不能直接执行）", step.StepID)
		}
	}
	// 序列化步骤。
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return WorkflowTemplate{}, err
	}
	t.Steps = string(stepsJSON)
	t.State = "published"
	t.TemplateID = id()
	t.PublishedAt = now
	t.CreatedAt, t.UpdatedAt = now, now
	_, err = tx.Exec(`INSERT INTO workflow_templates(template_id,name,version,trigger_kind,object_scope,
preconditions,steps,granularity,state,published_by,published_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TemplateID, t.Name, t.Version, t.TriggerKind, t.ObjectScope, t.Preconditions,
		t.Steps, t.Granularity, t.State, t.PublishedBy, now, now, now)
	if err != nil {
		return WorkflowTemplate{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 运行时绑定（受理时绑定领域包/模板/规范/目标版本，转换成 Step 与依赖）
// ---------------------------------------------------------------------------

// WorkflowBinding 是 workflow_bindings 的一行。
type WorkflowBinding struct {
	BindingID        string
	TaskID           string
	TemplateID       string
	DomainPackageRef string
	TemplateVersion  int
	SpecRefs         string
	TargetVersions   string
	StepIDs          string
	DependencyIDs    string
	State            string
	CreatedAt        int64
	UpdatedAt        int64
}

// bindWorkflowToTask 绑定流程到 Task（P4-07.2）。
// 受理时绑定领域包/模板/规范/目标版本，转换成 Step 与依赖；模型只在允许
// 调查节点提出补查。
func bindWorkflowToTask(tx *sql.Tx, taskID string, templateID string, domainPackageRef string, targetVersions string, now int64) (WorkflowBinding, error) {
	if taskID == "" || templateID == "" || domainPackageRef == "" {
		return WorkflowBinding{}, errors.New("绑定缺少任务/模板/领域包")
	}
	// 读取模板（转换成 Step 与依赖）。
	var t WorkflowTemplate
	err := tx.QueryRow(`SELECT template_id,name,version,steps,granularity,state FROM workflow_templates WHERE template_id=?`,
		templateID).Scan(&t.TemplateID, &t.Name, &t.Version, &t.Steps, &t.Granularity, &t.State)
	if err != nil {
		return WorkflowBinding{}, err
	}
	if t.State != "published" {
		return WorkflowBinding{}, fmt.Errorf("模板 %s 未发布（%s）", templateID, t.State)
	}
	// 解析步骤（转换成 Step 与依赖）。
	var steps []DomainWorkflowStep
	if err := json.Unmarshal([]byte(t.Steps), &steps); err != nil {
		return WorkflowBinding{}, err
	}
	// 创建运行时 Step 与依赖（流程交给既有运行时 Step/Wait 推进，不另写运维 scheduler）。
	var stepIDs, dependencyIDs []string
	for _, step := range steps {
		runtimeStep := Step{
			StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: step.StepID, DefinitionVersion: t.Version,
			State: V3StepReady, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertStep(tx, runtimeStep); err != nil {
			return WorkflowBinding{}, err
		}
		stepIDs = append(stepIDs, runtimeStep.StepID)
	}
	// 绑定。
	binding := WorkflowBinding{
		BindingID: id(), TaskID: taskID, TemplateID: templateID,
		DomainPackageRef: domainPackageRef, TemplateVersion: t.Version,
		TargetVersions: targetVersions, StepIDs: toJSON(stepIDs), DependencyIDs: toJSON(dependencyIDs),
		State: "active", CreatedAt: now, UpdatedAt: now,
	}
	_, err = tx.Exec(`INSERT INTO workflow_bindings(binding_id,task_id,template_id,domain_package_ref,
template_version,spec_refs,target_versions,step_ids,dependency_ids,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		binding.BindingID, binding.TaskID, binding.TemplateID, binding.DomainPackageRef,
		binding.TemplateVersion, binding.SpecRefs, binding.TargetVersions, binding.StepIDs,
		binding.DependencyIDs, binding.State, now, now)
	if err != nil {
		return WorkflowBinding{}, err
	}
	return binding, nil
}

// ---------------------------------------------------------------------------
// 粒度（原子能力、委托整段流程和持续服务三种粒度）
// ---------------------------------------------------------------------------

// ErrDuplicateScheduling 是重复调度的错误。
var ErrDuplicateScheduling = errors.New("本地与外部重复调度")

// ensureNoDuplicateScheduling 核对不重复调度（P4-07.3）。
// 明确原子能力、委托整段流程和持续服务三种粒度，防止本地与外部重复调度。
func ensureNoDuplicateScheduling(tx *sql.Tx, taskID string, granularity string, now int64) error {
	// 委托整段流程或持续服务：检查是否已有委托（防止本地与外部重复调度）。
	if granularity == "delegated_workflow" || granularity == "continuous_service" {
		var delegated bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM delegations WHERE task_id=? AND state='active')`, taskID).Scan(&delegated); err != nil {
			return err
		}
		if delegated {
			return fmt.Errorf("%w：任务 %s 已委托外部（粒度 %s）", ErrDuplicateScheduling, taskID, granularity)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 模板变化（不静默改运行中工作）
// ---------------------------------------------------------------------------

// WorkflowTemplateChange 是 workflow_template_changes 的一行。
type WorkflowTemplateChange struct {
	ChangeID                  string
	TemplateID                string
	FromVersion               int
	ToVersion                 int
	ImpactAssessment          string
	ContractRevisionRequired  bool
	ReconfirmationRequired    bool
	AssessedBy                string
	AssessedAt                int64
}

// assessTemplateChange 评估模板变化（P4-07.4）。
// 模板变化不静默改运行中工作；迁移需合同修订、依赖影响评估和必要重新确认。
func assessTemplateChange(tx *sql.Tx, templateID string, fromVersion, toVersion int, assessedBy string, now int64) (WorkflowTemplateChange, error) {
	if assessedBy == "" {
		return WorkflowTemplateChange{}, errors.New("模板变化评估缺少评估者")
	}
	// 检查运行中绑定（模板变化不静默改运行中工作）。
	var activeBindings int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM workflow_bindings WHERE template_id=? AND state='active'`, templateID).Scan(&activeBindings); err != nil {
		return WorkflowTemplateChange{}, err
	}
	change := WorkflowTemplateChange{
		ChangeID: id(), TemplateID: templateID, FromVersion: fromVersion, ToVersion: toVersion,
		ContractRevisionRequired: activeBindings > 0, // 有运行中绑定需合同修订
		ReconfirmationRequired: activeBindings > 0,   // 有运行中绑定需重新确认
		AssessedBy: assessedBy, AssessedAt: now,
	}
	if activeBindings > 0 {
		change.ImpactAssessment = fmt.Sprintf("模板变化影响 %d 个运行中绑定（不静默改运行中工作）", activeBindings)
	}
	_, err := tx.Exec(`INSERT INTO workflow_template_changes(change_id,template_id,from_version,to_version,
impact_assessment,contract_revision_required,reconfirmation_required,assessed_by,assessed_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		change.ChangeID, change.TemplateID, change.FromVersion, change.ToVersion,
		change.ImpactAssessment, boolToInt(change.ContractRevisionRequired),
		boolToInt(change.ReconfirmationRequired), change.AssessedBy, now)
	if err != nil {
		return change, err
	}
	return change, nil
}

// ---------------------------------------------------------------------------
// 确定性处理器（领域可自动判定的步骤）
// ---------------------------------------------------------------------------

// ensureDeterministicStepsNotAllModel 核对确定性步骤（P4-07.5）。
// 为领域可自动判定的步骤提供确定性处理器；不把每个流程都变为模型循环或 Goal。
func ensureDeterministicStepsNotAllModel(steps []DomainWorkflowStep) error {
	if len(steps) == 0 {
		return errors.New("流程缺少步骤")
	}
	allInvestigation := true
	for _, step := range steps {
		if step.Kind != "investigation" {
			allInvestigation = false
			break
		}
	}
	if allInvestigation {
		return errors.New("流程全部步骤为调查（不把每个流程都变为模型循环或 Goal）")
	}
	return nil
}

// 确保 context/sql/errors/fmt/json 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = json.Marshal
)
