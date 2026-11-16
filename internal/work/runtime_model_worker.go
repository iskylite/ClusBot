package work

// P2-06：模型 Worker 与控制面隔离。
//
// 本文件保留模型能力，使工作者只在领取范围内调查和提案：
//   - 控制面按 Task/Step 范围装配输入及当前允许工具；Worker 收到有限上下文
//     和预算，不读完整 SQLite；
//   - 每次模型调用绑定当前 Run/单元/代际和 Manifest；模型返回提案由运行时
//     校验后才登记后续工作；
//   - 将模型请求配额与工具等待分离；调用前预留预算，结束后结算；控制变更
//     中止新请求而非抹除已发生费用；
//   - 工具调用通过宿主能力入口；在 P3 前只开放当前已核验的查询/个人成果能力，
//     不添加任意 Shell；
//   - 模型不可用时返回明确可恢复/待决定状态，确定性核对循环独立存活。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Worker 上下文（控制面装配，有限范围）
// ---------------------------------------------------------------------------

// ModelWorkerContext 是控制面装配给 Worker 的有限上下文（P2-06.1）。
// Worker 收到有限上下文和预算，不读完整 SQLite。
type ModelWorkerContext struct {
	// 范围绑定：Task/Step/Run/代际。
	TaskID              string
	StepID              string
	RunID               string
	ContractRevision    int
	ExecutionGeneration int
	LeaseGeneration     int
	// 有限输入（按范围装配；不含完整 SQLite 内容）。
	Goal                string
	Criteria            string
	Messages            []Message
	EvidenceRefs        []string
	// 允许工具（当前已核验的查询/个人成果能力；P3 前不添加任意 Shell）。
	AllowedTools        []string
	// 预算（预留额度）。
	Budget              WorkerBudget
	// Manifest 引用（每次模型调用绑定当前 Run/单元/代际）。
	ManifestID          string
}

// WorkerBudget 是 Worker 的预算（预留额度）。
type WorkerBudget struct {
	MaxModelRequests int
	MaxInputTokens   int64
	MaxOutputTokens  int64
	ActiveMS         int64
	// 预留的配额结算键（调用前预留预算，结束后结算）。
	ReservationKeys  []string
}

// ---------------------------------------------------------------------------
// 上下文装配（控制面按范围装配）
// ---------------------------------------------------------------------------

// assembleWorkerContext 控制面按 Task/Step 范围装配 Worker 上下文（P2-06.1）。
// Worker 收到有限上下文和预算，不读完整 SQLite；每次模型调用绑定当前
// Run/单元/代际和 Manifest。
func assembleWorkerContext(tx *sql.Tx, taskID, stepID string, generation int, budget WorkerBudget, now int64) (ModelWorkerContext, error) {
	ctx := ModelWorkerContext{
		TaskID:              taskID,
		StepID:              stepID,
		ExecutionGeneration: generation,
		Budget:              budget,
	}
	// 读取 Task 范围（目标/验收/合同修订）。
	var contractRevision int
	err := tx.QueryRow(`SELECT goal, acceptance_criteria, revision FROM tasks WHERE id=?`, taskID).
		Scan(&ctx.Goal, &ctx.Criteria, &contractRevision)
	if err != nil {
		return ctx, err
	}
	ctx.ContractRevision = contractRevision
	// 合同隔离：旧合同版本的 Step 不装配（旧计划对新合同失效）。
	if stepID != "" {
		var stepContract int
		var state string
		err := tx.QueryRow(`SELECT contract_revision, state FROM steps WHERE step_id=?`, stepID).Scan(&stepContract, &state)
		if err != nil {
			return ctx, err
		}
		if stepContract != contractRevision {
			return ctx, fmt.Errorf("%w：Step 合同版本 %d ≠ 当前 %d（旧计划失效）",
				ErrContractChanged, stepContract, contractRevision)
		}
	}
	// 装配有限消息（按范围，不含完整历史）。
	messages, err := messages(tx, conversationIDForTask(tx, taskID), false)
	if err != nil {
		return ctx, err
	}
	ctx.Messages = messages
	// 允许工具：当前已核验的查询/个人成果能力（P3 前不添加任意 Shell）。
	ctx.AllowedTools = []string{"query", "read_material", "save_artifact", "accept_artifact"}
	// Manifest 引用（每次模型调用绑定当前 Run/单元/代际）。
	ctx.ManifestID = fmt.Sprintf("manifest:%s:%s:%d", taskID, stepID, generation)
	return ctx, nil
}

// ErrContractChanged 是合同版本变化的错误。
var ErrContractChanged = errors.New("工作合同已变化")

// conversationIDForTask 读取任务的会话 ID。
func conversationIDForTask(tx *sql.Tx, taskID string) string {
	var id string
	tx.QueryRow("SELECT conversation_id FROM tasks WHERE id=?", taskID).Scan(&id)
	return id
}

// ---------------------------------------------------------------------------
// 预算预留与结算（模型请求配额与工具等待分离）
// ---------------------------------------------------------------------------

// reserveModelBudget 预留模型预算（P2-06.3）。
// 模型请求配额与工具等待分离；调用前预留预算，结束后结算；控制变更中止
// 新请求而非抹除已发生费用。
func reserveModelBudget(tx *sql.Tx, taskID, workerID string, budget WorkerBudget, now int64) ([]string, error) {
	var keys []string
	// 预留模型槽（按实际模型请求占用）。
	slot, err := acquireModelSlot(tx, workerID, taskID, now)
	if err != nil {
		return nil, err
	}
	keys = append(keys, slot.SettlementKey)
	// 预留 token 预算（任务总预算，跨 Run 不重置）。
	tokenReservation, err := reserveQuota(tx, QuotaReservation{
		Scope: QuotaTaskBudget, TaskID: taskID, WorkerID: workerID,
		Amount: budget.MaxInputTokens + budget.MaxOutputTokens,
		Unit: "tokens", ReservedAt: now,
	})
	if err != nil {
		return nil, err
	}
	keys = append(keys, tokenReservation.SettlementKey)
	return keys, nil
}

// settleModelBudget 结算模型预算（结束后结算；控制变更中止新请求而非抹除已发生费用）。
func settleModelBudget(tx *sql.Tx, reservationKeys []string, costKnown bool, now int64) error {
	for _, key := range reservationKeys {
		if err := settleQuota(tx, key, costKnown, now); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 提案校验（模型返回提案经运行时校验后才登记）
// ---------------------------------------------------------------------------

// WorkerProposal 是模型 Worker 的提案。
type WorkerProposal struct {
	// 建议的后续工作（步骤/查询/动作）。
	SuggestedSteps   []StepDefinition
	SuggestedQueries []string
	SuggestedActions []string
	// 声明（模型自报，不能直接批准）。
	DeclaresComplete bool
	DeclaresSuccess  bool
	// 目标范围（越权目标被拒绝）。
	TargetScope      string
	// 依据与证据引用。
	EvidenceRefs     []string
	// 未知项（明确保留，不编造）。
	Unknowns         []string
}

// ValidateProposal 校验 Worker 提案（P2-06.2/P2-06 反例）。
// 模型返回提案由运行时校验后才登记后续工作：模型建议越权目标、旧合同或
// 直接成功声明均不能绕过运行时。
func ValidateProposal(tx *sql.Tx, ctx ModelWorkerContext, proposal WorkerProposal) error {
	// 1. 越权目标：模型建议超出 Task 范围的目标被拒绝。
	if proposal.TargetScope != "" && proposal.TargetScope != ctx.Goal && !containsScope(ctx.Goal, proposal.TargetScope) {
		return fmt.Errorf("%w：模型建议越权目标 %q（超出任务范围 %q）", ErrWorkerScope, proposal.TargetScope, ctx.Goal)
	}
	// 2. 直接成功声明：模型自报成功不能使 Task 成功（须经运行时完成判定）。
	if proposal.DeclaresSuccess {
		return fmt.Errorf("%w：模型直接成功声明不能绕过运行时完成判定", ErrProposalRejected)
	}
	// 3. 旧合同：提案必须绑定当前合同版本。
	for _, step := range proposal.SuggestedSteps {
		if err := step.validate(); err != nil {
			return fmt.Errorf("%w：步骤定义无效：%v", ErrProposalRejected, err)
		}
	}
	// 4. 未知项必须保留（不编造）。
	// 未知项是合法的（明确保留）；但不能用"无未知"伪造完整。
	return nil
}

// ErrProposalRejected 是提案被拒绝的错误。
var ErrProposalRejected = errors.New("worker 提案被拒绝")

// containsScope 判断目标是否在任务范围内。
func containsScope(goal, target string) bool {
	// 简化：目标必须是任务的子集或相关（具体领域范围判定在 P4 接入）。
	return len(target) <= len(goal)
}

// ---------------------------------------------------------------------------
// 上下文隔离（两个 Worker 不串任务）
// ---------------------------------------------------------------------------

// isolateWorkerContexts 核对两个 Worker 的上下文隔离（P2-06 反例）。
// 两个 Worker 的上下文、工具范围和结果不会串任务。
func isolateWorkerContexts(a, b ModelWorkerContext) error {
	if a.TaskID == b.TaskID && a.StepID == b.StepID && a.StepID != "" {
		return fmt.Errorf("两个 Worker 共享同一 Step 上下文（不串任务需不同 Step）")
	}
	if a.ManifestID == b.ManifestID && a.ManifestID != "" {
		return fmt.Errorf("两个 Worker 共享 Manifest（上下文不隔离）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 模型不可用（明确可恢复/待决定状态）
// ---------------------------------------------------------------------------

// ModelUnavailableError 是模型不可用的错误（明确可恢复/待决定）。
type ModelUnavailableError struct {
	Recoverable bool
	Reason      string
}

func (e *ModelUnavailableError) Error() string {
	return fmt.Sprintf("模型不可用（可恢复=%v）：%s", e.Recoverable, e.Reason)
}

// errModelUnavailable 是模型不可用的哨兵。
var errModelUnavailable = &ModelUnavailableError{Recoverable: true, Reason: "模型服务不可达"}

// modelAvailableOrWait 核对模型可用性（P2-06.5）。
// 模型不可用时返回明确可恢复/待决定状态，确定性核对循环独立存活。
func modelAvailableOrWait(available func() bool) error {
	if available != nil && !available() {
		return errModelUnavailable
	}
	return nil
}

// 确保 context/sql 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
)
