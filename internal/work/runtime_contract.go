package work

// P0-02 固定状态、版本和旧新映射。
//
// 本文件是 V3 §4、§5.1—5.2、§6.7 与第 2.2 节状态硬约束的单一权威定义，
// 消除跨模块各自解释同义状态的可能。本阶段只固定契约与旧新映射规则，
// 不改变现有存储行为；新状态机的持久化在 P1 各项落地。

import "fmt"

// ---------------------------------------------------------------------------
// V3 Task 九态（§5.2）
// ---------------------------------------------------------------------------

// V3TaskState 是 V3 正文固定的 Task 九态之一。序列化使用稳定英文标识；
// 不同模块不得自行发明同义状态（第 2.2 节硬约束）。
type V3TaskState string

const (
	V3TaskPending    V3TaskState = "pending"     // 待执行
	V3TaskRunning    V3TaskState = "running"     // 执行中
	V3TaskWaiting    V3TaskState = "waiting"     // 等待中
	V3TaskPaused     V3TaskState = "paused"      // 已暂停
	V3TaskAcceptance V3TaskState = "acceptance"  // 验收中
	V3TaskFinalizing V3TaskState = "finalizing"  // 收尾中
	V3TaskSucceeded  V3TaskState = "succeeded"   // 成功
	V3TaskFailed     V3TaskState = "failed"      // 失败
	V3TaskCancelled  V3TaskState = "cancelled"   // 已取消
)

// IsTerminal 报告 V3 Task 是否为终态；终态不复活（§5.1、§5.2）。
func (s V3TaskState) IsTerminal() bool {
	return s == V3TaskSucceeded || s == V3TaskFailed || s == V3TaskCancelled
}

// Valid 报告标识是否为受承认的 V3 Task 状态。
func (s V3TaskState) Valid() bool {
	switch s {
	case V3TaskPending, V3TaskRunning, V3TaskWaiting, V3TaskPaused,
		V3TaskAcceptance, V3TaskFinalizing, V3TaskSucceeded, V3TaskFailed, V3TaskCancelled:
		return true
	}
	return false
}

// v3TaskTransitions 是 §5.2 的事件/守卫/目标状态表的机械表达。
// 守卫的业务判定（预算、权限、Wait、未决操作核对等）在 P1-03 实现；
// 这里固定"哪些跨态在结构上被允许"，任何未列出的迁移一律拒绝。
var v3TaskTransitions = map[V3TaskState]map[V3TaskState]bool{
	V3TaskPending: {
		V3TaskRunning:    true, // 领取成功
		V3TaskWaiting:    true, // 依赖/事件/时间未满足
		V3TaskPaused:     true, // 有效暂停请求
		V3TaskFinalizing: true, // 取消或最终失败
	},
	V3TaskRunning: {
		V3TaskWaiting:    true, // 没有可继续的 Step
		V3TaskPaused:     true,
		V3TaskAcceptance: true, // 必需 Step 完成且操作有明确结果
		V3TaskFinalizing: true,
	},
	V3TaskWaiting: {
		V3TaskPending:    true, // 唤醒后重新检查资格
		V3TaskPaused:     true,
		V3TaskFinalizing: true,
	},
	V3TaskPaused: {
		V3TaskPending:    true, // 恢复重评后满足条件
		V3TaskWaiting:    true, // 恢复重评后仍在等待
		V3TaskFinalizing: true, // 暂停中被取消
	},
	V3TaskAcceptance: {
		V3TaskPending:    true, // 验收未通过但允许继续
		V3TaskPaused:     true,
		V3TaskFinalizing: true, // 验收通过/取消/最终失败
	},
	V3TaskFinalizing: {
		V3TaskSucceeded: true, // 仅运行时确认收尾完成
		V3TaskFailed:    true,
		V3TaskCancelled: true,
		// 注意：§5.2 允许未知效果在收尾中保持待核对，不新增"待核对"Task 状态。
	},
}

// CanTransitionTask 报告 from→to 是否为 §5.2 结构允许的迁移。
// 允许不代表条件已满足；守卫证据在 P1-03 逐项核对。
func CanTransitionTask(from, to V3TaskState) bool {
	return v3TaskTransitions[from][to]
}

// ---------------------------------------------------------------------------
// V3 Step 七态（§5.1）
// ---------------------------------------------------------------------------

type V3StepState string

const (
	V3StepBlocked   V3StepState = "blocked"   // 未就绪
	V3StepReady     V3StepState = "ready"     // 就绪
	V3StepRunning   V3StepState = "running"   // 执行中
	V3StepWaiting   V3StepState = "waiting"   // 等待
	V3StepCompleted V3StepState = "completed" // 完成
	V3StepFailed    V3StepState = "failed"    // 失败
	V3StepCancelled V3StepState = "cancelled" // 已取消
)

// ---------------------------------------------------------------------------
// V3 Operation 七态（§5.2）
// ---------------------------------------------------------------------------

type V3OperationState string

const (
	V3OperationRegistered V3OperationState = "registered" // 已登记
	V3OperationDispatched V3OperationState = "dispatched" // 已派发
	V3OperationRunning    V3OperationState = "running"    // 执行中
	V3OperationReconcile  V3OperationState = "reconcile"  // 待核对
	V3OperationSucceeded  V3OperationState = "succeeded"  // 成功
	V3OperationFailed     V3OperationState = "failed"     // 失败
	V3OperationCancelled  V3OperationState = "cancelled"  // 已取消
)

// v3OperationTransitions 是 §5.2 Operation 允许迁移的结构表达。
var v3OperationTransitions = map[V3OperationState]map[V3OperationState]bool{
	V3OperationRegistered: {
		V3OperationDispatched: true, // 最终准入通过并记录 attempt
		V3OperationCancelled:  true, // 确认未执行
	},
	V3OperationDispatched: {
		V3OperationRunning:   true, // 外部确认接收/开始
		V3OperationSucceeded: true, // 可信完成证据
		V3OperationFailed:    true, // 可信失败证据（可带部分效果）
		V3OperationReconcile: true, // 超时/断连/失联/冲突/重启不明
		V3OperationCancelled: true, // 确认未执行或已停止
	},
	V3OperationRunning: {
		V3OperationSucceeded: true,
		V3OperationFailed:    true,
		V3OperationReconcile: true,
		V3OperationCancelled: true, // 确认已停止
	},
	V3OperationReconcile: {
		V3OperationRegistered: true, // 确认未执行且准入仍有效，递增 attempt 重派
		V3OperationSucceeded:  true, // 迟到可信完成证据
		V3OperationFailed:     true,
		V3OperationCancelled:  true, // 确认未执行或已停止
	},
}

// CanTransitionOperation 报告 from→to 是否为 §5.2 结构允许的 Operation 迁移。
func CanTransitionOperation(from, to V3OperationState) bool {
	return v3OperationTransitions[from][to]
}

// ---------------------------------------------------------------------------
// V3 资产九态（§6.7）
// ---------------------------------------------------------------------------

// V3AssetState 是 §6.7 的九个唯一末级状态之一。"在管"是归组而非第十态。
type V3AssetState string

const (
	V3AssetCandidate    V3AssetState = "candidate"    // 待纳管
	V3AssetReview       V3AssetState = "review"       // 在管/待复核
	V3AssetPreDebug     V3AssetState = "pre_debug"    // 在管/待调试
	V3AssetDebugging    V3AssetState = "debugging"    // 在管/调试中
	V3AssetPreDelivery  V3AssetState = "pre_delivery" // 在管/待交付
	V3AssetActive       V3AssetState = "active"       // 在管/在役
	V3AssetMaintenance  V3AssetState = "maintenance"  // 在管/维护中
	V3AssetLent         V3AssetState = "lent"         // 借出
	V3AssetDecommission V3AssetState = "decommission" // 已退役
)

// Managed 报告状态是否属于"在管"分组（§6.7：在管是归组而非第十态）。
func (s V3AssetState) Managed() bool {
	switch s {
	case V3AssetReview, V3AssetPreDebug, V3AssetDebugging,
		V3AssetPreDelivery, V3AssetActive, V3AssetMaintenance:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 版本含义（第 2.2 节、§4.1、§5.1）
// ---------------------------------------------------------------------------

// RevisionKind 区分当前代码中混用的几种"版本/代际"，使新代码必须显式选择。
// 旧 task_revisions/run_bindings/context_manifests 按原含义解释，新增格式
// 记录其版本；历史不得回写成新语义（P0-02.2）。
type RevisionKind string

const (
	// RevisionStorageRow：存储行乐观版本。现对应 tasks.revision，
	// 每次状态/内容 UPDATE 递增，用于条件更新（CAS）。
	RevisionStorageRow RevisionKind = "storage_row"
	// RevisionWorkContract：工作合同版本。现由 task_revisions(task_id,revision)
	// 在首次绑定时快照表达；合同变化（目标/范围/完成条件修订）使旧计划失效，
	// 但不重写历史快照。
	RevisionWorkContract RevisionKind = "work_contract"
	// RevisionControl：控制修订。现由 context_manifests.control_revision 记录
	// 装配时的 tasks.revision；pause/stop/revise 均使其递增，用于识别
	// 装配于旧控制意图下的上下文。
	RevisionControl RevisionKind = "control"
	// RevisionAssignmentGeneration：任务分配代际。现由
	// assignments(task_id,generation) 表达，拒绝旧分配继续推进。
	RevisionAssignmentGeneration RevisionKind = "assignment_generation"
	// RevisionExecutionGeneration：执行代际。现由 runs.execution_generation 与
	// run_bindings(run_id,generation) 表达；一次 Task/Step 可有多个 Run，
	// 跨 Run 的 Task 预算不重置（P0-02.4）。
	RevisionExecutionGeneration RevisionKind = "execution_generation"
)

// ---------------------------------------------------------------------------
// 旧 Task 五态 → V3 映射（P0-02.3）
// ---------------------------------------------------------------------------

// LegacyTaskSnapshot 是判定旧记录所需的全部持久事实；不明者保留待核对，
// 不自动成功或重派。
type LegacyTaskSnapshot struct {
	Status       string // tasks.status（五态）
	StatusReason string // tasks.status_reason 细分
	// HasPendingPublication 存在未确认的文件发布（publications 开放状态）。
	HasPendingPublication bool
	// HasUnknownRead 存在结果未知且未确认的工具读取。
	HasUnknownRead bool
	// HasUnacceptedArtifact 存在尚未验收的成果。
	HasUnacceptedArtifact bool
	// LatestRunStatus 最新 Run 的状态（无 Run 时为空）。
	LatestRunStatus string
}

// V3TaskMapping 是旧记录的映射结论。
type V3TaskMapping struct {
	State V3TaskState
	// ReconcileRequired 为 true 表示映射不能判定业务等价，必须保留恢复待核对；
	// 例如未知读取、未确认发布、中断但控制意图不明。
	ReconcileRequired bool
	Reason            string
}

// MapLegacyTask 按 P0-02.3 的逐条规则映射旧 Task。
// 规则顺序即优先级；任一未知因素存在都不得产生"成功"结论。
func MapLegacyTask(s LegacyTaskSnapshot) V3TaskMapping {
	switch s.Status {
	case "completed":
		// 旧 completed 唯一来源是 Accept 已验收成果。
		return V3TaskMapping{State: V3TaskSucceeded, Reason: "旧任务已完成验收"}
	case "cancelled":
		return V3TaskMapping{State: V3TaskCancelled, Reason: "旧任务已取消"}
	case "waiting_user":
		if s.HasPendingPublication {
			// 文件发布未确认：不能断定成果交付，保留核对。
			return V3TaskMapping{State: V3TaskAcceptance, ReconcileRequired: true, Reason: "存在未确认的文件发布，交付结果待核对"}
		}
		return V3TaskMapping{State: V3TaskAcceptance, Reason: "等待人工验收或补充"}
	case "active":
		if s.HasUnknownRead {
			return V3TaskMapping{State: V3TaskRunning, ReconcileRequired: true, Reason: "存在结果未知的工具读取，须先核对"}
		}
		switch s.StatusReason {
		case "queued", "running":
			return V3TaskMapping{State: V3TaskPending, Reason: "旧任务待调度或执行中，恢复后重新核对资格"}
		case "waiting_dependencies":
			return V3TaskMapping{State: V3TaskWaiting, Reason: "等待依赖成果验收"}
		default:
			// active 但细分原因无法识别：不猜测，保留待核对。
			return V3TaskMapping{State: V3TaskPending, ReconcileRequired: true, Reason: "旧 active 细分原因未识别，恢复待核对"}
		}
	case "interrupted":
		switch s.StatusReason {
		case "paused", "pause_requested":
			return V3TaskMapping{State: V3TaskPaused, Reason: "旧任务已暂停"}
		case "reconciliation_required":
			return V3TaskMapping{State: V3TaskRunning, ReconcileRequired: true, Reason: "旧任务中断且读取结果待确认"}
		case "run_failed":
			return V3TaskMapping{State: V3TaskRunning, ReconcileRequired: true, Reason: "旧任务因运行失败中断，是否可继续待核对"}
		default:
			// process_interrupted / stop_requested 等：中断原因不能确定外部
			// 效果，恢复待核对，绝不自动重派或自动成功。
			return V3TaskMapping{State: V3TaskRunning, ReconcileRequired: true, Reason: "旧中断原因未识别，恢复待核对"}
		}
	default:
		// 未识别的旧状态：明确拒绝，不静默归类。
		return V3TaskMapping{State: V3TaskRunning, ReconcileRequired: true,
			Reason: fmt.Sprintf("未识别的旧任务状态 %q，必须人工核对", s.Status)}
	}
}

// ---------------------------------------------------------------------------
// 终态后续请求的判定（P0-02.4）
// ---------------------------------------------------------------------------

// SuccessorDecision 是对一次"针对既有 Task 的后续请求"的判定结果。
type SuccessorDecision int

const (
	// SuccessorCreateNew：必须创建关联新 Task（原任务终态不复活）。
	SuccessorCreateNew SuccessorDecision = iota
	// SuccessorResumeOriginal：允许恢复原任务（旧暂停可恢复）。
	SuccessorResumeOriginal
	// SuccessorRejected：请求在结构上不被接受（如未识别状态）。
	SuccessorRejected
)

func (d SuccessorDecision) String() string {
	switch d {
	case SuccessorCreateNew:
		return "create_successor_task"
	case SuccessorResumeOriginal:
		return "resume_original"
	default:
		return "rejected"
	}
}

// DecideTaskSuccessor 按 P0-02.4 判定：已取消/成功/失败 Task 的新委托
// 创建关联新 Task；旧暂停（映射后 V3TaskPaused）可恢复。输入为旧任务的
// V3 映射状态（由 MapLegacyTask 或新状态机给出）。
func DecideTaskSuccessor(state V3TaskState) SuccessorDecision {
	if state.IsTerminal() {
		return SuccessorCreateNew
	}
	if state == V3TaskPaused {
		return SuccessorResumeOriginal
	}
	if !state.Valid() {
		return SuccessorRejected
	}
	// 非终态且非暂停：任务仍可推进，不存在"后续新委托"的判定必要；
	// 复用恢复语义以保持单一入口（正式 Resume 入口切换在 P1-11 验收）。
	return SuccessorResumeOriginal
}

// ---------------------------------------------------------------------------
// 步骤归属判定（P0-02.5）
// ---------------------------------------------------------------------------

// StepOwnership 区分"独立子 Task"与"Task 内 Step"。
type StepOwnership int

const (
	// OwnershipChildTask：有独立目标/验收的步骤保留为子 Task（现有工作流模型）。
	OwnershipChildTask StepOwnership = iota
	// OwnershipStep：普通流程步骤使用 Task 内 Step（P1-04 落地）。
	OwnershipStep
)

// ClassifyWorkflowStep 按 P0-02.5 判定一个工作流步骤的归属：
// 有独立目标与验收条件的步骤保留独立子 Task；否则应为 Task 内 Step。
// 现有 workflow.go 模型下所有 WorkflowStep 均要求独立 goal/criteria，
// 因此当前全部归 OwnershipChildTask；Step 形态在 P1-04 引入。
func ClassifyWorkflowStep(hasIndependentGoal, hasIndependentCriteria bool) StepOwnership {
	if hasIndependentGoal && hasIndependentCriteria {
		return OwnershipChildTask
	}
	return OwnershipStep
}
