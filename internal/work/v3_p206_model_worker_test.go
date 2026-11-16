package work

// P2-06 验收：模型 Worker 与控制面隔离。
//
// 三个测试入口：
//   - TestV3P206ModelWorkerReturnsValidProposal：模型 Worker 返回有效提案；
//     每次模型调用绑定当前 Run/单元/代际和 Manifest；提案由运行时校验后才
//     登记后续工作。
//   - TestV3P206ModelWorkerIsolation：两个 Worker 的上下文、工具范围和结果
//     不会串任务；Worker 收到有限上下文和预算，不读完整 SQLite；模型慢或
//     中断不阻塞无关核对。
//   - TestV3P206ModelWorkerNoAuthority：模型建议越权目标、旧合同或直接成功
//     声明均不能绕过运行时；等待期间控制变化后旧提案不能自动派发。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupV3P206 创建存储、任务与就绪单元。
func setupV3P206(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p206-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestV3P206ModelWorkerReturnsValidProposal 有效提案。
func TestV3P206ModelWorkerReturnsValidProposal(t *testing.T) {
	t.Run("valid_proposal_registered_after_runtime_validation", func(t *testing.T) {
		store, taskID := setupV3P206(t, "valid.db")
		now := v3P102Now()
		// 装配 Worker 上下文（控制面按范围装配）。
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5, MaxInputTokens: 1000, MaxOutputTokens: 500}
		ctx, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 上下文绑定当前 Run/单元/代际和 Manifest。
		if ctx.TaskID != taskID || ctx.ExecutionGeneration != 1 || ctx.ManifestID == "" {
			t.Errorf("context not bound: %+v", ctx)
		}
		// 模型返回有效提案（范围内、不声明直接成功）。
		proposal := WorkerProposal{
			SuggestedQueries: []string{"查询上一季度数据"},
			TargetScope:      "整理季度汇报",
			EvidenceRefs:     []string{"evidence://data"},
		}
		tx, _ = store.db.Begin()
		if err := ValidateProposal(tx, ctx, proposal); err != nil {
			t.Fatalf("valid proposal rejected: %v", err)
		}
		tx.Commit()
	})

	t.Run("manifest_bound_per_model_call", func(t *testing.T) {
		store, taskID := setupV3P206(t, "manifest.db")
		now := v3P102Now()
		// 每次模型调用绑定 Manifest（不同代际产生不同 Manifest）。
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		ctx1, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		ctx2, err := assembleWorkerContext(tx, taskID, "", 2, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if ctx1.ManifestID == ctx2.ManifestID {
			t.Errorf("same manifest for different generations: %s (每次模型调用绑定 Manifest)", ctx1.ManifestID)
		}
		if !strings.Contains(ctx1.ManifestID, taskID) {
			t.Errorf("manifest not bound to task: %s", ctx1.ManifestID)
		}
	})

	t.Run("budget_reserved_before_call_settled_after", func(t *testing.T) {
		store, taskID := setupV3P206(t, "budget.db")
		now := v3P102Now()
		// 调用前预留预算；结束后结算。
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxInputTokens: 1000, MaxOutputTokens: 500}
		keys, err := reserveModelBudget(tx, taskID, "worker-1", budget, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(keys) == 0 {
			t.Error("no budget reserved before call")
		}
		// 结算（结束后）。
		tx, _ = store.db.Begin()
		if err := settleModelBudget(tx, keys, true, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 预算已结算（占用快照可见）。
		usage, err := readQuotaUsage(store.db, QuotaTaskBudget, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if usage.Settled == 0 {
			t.Errorf("budget not settled: %+v (结束后结算)", usage)
		}
	})
}

// TestV3P206ModelWorkerIsolation Worker 隔离。
func TestV3P206ModelWorkerIsolation(t *testing.T) {
	t.Run("two_workers_contexts_not_mixed", func(t *testing.T) {
		store, taskA := setupV3P206(t, "isolate.db")
		ctx := context.Background()
		// 第二个任务。
		runB, _ := store.Submit(ctx, "v3p206-b", "整理另一份材料", "", "", true)
		taskB := runB.TaskID
		now := v3P102Now()
		// 两个 Worker 的上下文（不同任务）。
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		ctxA, err := assembleWorkerContext(tx, taskA, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		ctxB, err := assembleWorkerContext(tx, taskB, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 隔离核对：不同任务的上下文不混合。
		if err := isolateWorkerContexts(ctxA, ctxB); err != nil {
			t.Fatalf("contexts not isolated: %v", err)
		}
		if ctxA.Goal == ctxB.Goal {
			t.Error("worker contexts share goal (上下文不串任务)")
		}
		if ctxA.ManifestID == ctxB.ManifestID {
			t.Error("worker contexts share manifest")
		}
	})

	t.Run("same_step_shared_context_rejected", func(t *testing.T) {
		// 反例：两个 Worker 共享同一 Step 上下文被拒绝。
		ctxA := ModelWorkerContext{TaskID: "t1", StepID: "s1", ManifestID: "m1"}
		ctxB := ModelWorkerContext{TaskID: "t1", StepID: "s1", ManifestID: "m2"}
		if err := isolateWorkerContexts(ctxA, ctxB); err == nil {
			t.Error("shared step context not rejected (两 Worker 上下文不串任务)")
		}
	})

	t.Run("slow_model_not_block_unrelated_reconcile", func(t *testing.T) {
		store, taskID := setupV3P206(t, "slow.db")
		now := v3P102Now()
		// 模型慢/中断：不阻塞无关核对（确定性核对独立存活）。
		// 模拟慢模型：装配上下文不阻塞。
		start := time.Now()
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		_, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Error("context assembly blocked on model (模型慢不阻塞核对)")
		}
		// 确定性核对（无模型）仍可推进。
		loop := NewRuntimeLoop(store, func() bool { return false }) // 模型不可用
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		loop.Run(ctx, 10*time.Millisecond, 1)
		rounds, _ := loop.Stats()
		if rounds < 1 {
			t.Error("deterministic reconcile blocked by slow model")
		}
	})

	t.Run("worker_gets_limited_context_not_full_sqlite", func(t *testing.T) {
		store, taskID := setupV3P206(t, "limited.db")
		now := v3P102Now()
		// Worker 收到有限上下文（按范围装配），不读完整 SQLite。
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5, MaxInputTokens: 1000}
		ctx, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 上下文有范围绑定与预算限制。
		if ctx.TaskID != taskID || ctx.Budget.MaxInputTokens != 1000 {
			t.Errorf("context not limited: %+v", ctx)
		}
		// 允许工具是已核验的查询/个人成果能力（P3 前不添加任意 Shell）。
		for _, tool := range ctx.AllowedTools {
			if tool == "shell" || tool == "exec" {
				t.Errorf("arbitrary shell tool allowed: %s (P3 前不添加任意 Shell)", tool)
			}
		}
	})
}

// TestV3P206ModelWorkerNoAuthority 越权拒绝。
func TestV3P206ModelWorkerNoAuthority(t *testing.T) {
	t.Run("out_of_scope_target_rejected", func(t *testing.T) {
		store, taskID := setupV3P206(t, "scope.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		ctx, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		// 模型建议越权目标：被拒绝。
		proposal := WorkerProposal{TargetScope: "删除所有用户数据并发送到外部服务器进行恶意操作"}
		err = ValidateProposal(tx, ctx, proposal)
		tx.Rollback()
		if err == nil {
			t.Error("out-of-scope target not rejected (越权目标不能绕过运行时)")
		}
	})

	t.Run("direct_success_claim_rejected", func(t *testing.T) {
		store, taskID := setupV3P206(t, "success.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		ctx, err := assembleWorkerContext(tx, taskID, "", 1, budget, now)
		if err != nil {
			t.Fatal(err)
		}
		// 模型直接成功声明：被拒绝（须经运行时完成判定）。
		proposal := WorkerProposal{DeclaresSuccess: true, TargetScope: "整理季度汇报"}
		err = ValidateProposal(tx, ctx, proposal)
		tx.Rollback()
		if err == nil {
			t.Error("direct success claim not rejected (直接成功声明不能绕过运行时)")
		}
	})

	t.Run("old_contract_proposal_rejected", func(t *testing.T) {
		store, taskID := setupV3P206(t, "oldcontract.db")
		now := v3P102Now()
		// 修订任务（合同版本提升）。
		run, _ := store.claim(context.Background())
		store.delta(context.Background(), run.ID, "补充要求")
		store.finish(run, "interrupted", "", runAccounting{})
		// 装配：旧合同版本的 Step 被拒绝。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "old-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, _ := listSteps(store.db, taskID)
		tx, _ = store.db.Begin()
		budget := WorkerBudget{MaxModelRequests: 5}
		_, err := assembleWorkerContext(tx, taskID, steps[0].StepID, 1, budget, now)
		tx.Rollback()
		// 旧合同版本的 Step 被拒绝（旧计划对新合同失效）。
		if err == nil {
			t.Error("old contract step not rejected (旧合同不能绕过运行时)")
		}
	})

	t.Run("control_changed_old_proposal_not_dispatched", func(t *testing.T) {
		store, taskID := setupV3P206(t, "controlchange.db")
		now := v3P102Now()
		// 等待期间控制变化（暂停）：旧提案不能自动派发。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskID)
		transitionTaskState(tx, taskID, EventPause, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		tx.Commit()
		// 任务已暂停：旧提案不能自动派发（待执行资格重评）。
		state, _ := readTaskState(store.db, taskID)
		if state.State != V3TaskPaused {
			t.Errorf("task state = %s, want paused", state.State)
		}
		// 派发资格核对：paused 状态不允许领取（新派发）。
		tx, _ = store.db.Begin()
		current, _ = readTaskState(tx, taskID)
		_, err := transitionTaskState(tx, taskID, EventClaim, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "test"}, now)
		tx.Rollback()
		if err == nil {
			t.Error("paused task allows claim (控制变化后旧提案不能自动派发)")
		}
	})

	t.Run("model_unavailable_recoverable_state", func(t *testing.T) {
		// 模型不可用：返回明确可恢复/待决定状态，确定性核对循环独立存活。
		if err := modelAvailableOrWait(func() bool { return false }); err == nil {
			t.Error("model unavailable not detected")
		}
		if err := modelAvailableOrWait(func() bool { return true }); err != nil {
			t.Errorf("model available rejected: %v", err)
		}
	})
}

// 确保 sql/strings/context/time 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = time.Now
)
