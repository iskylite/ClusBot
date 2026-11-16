package work

// P0-02 验收：固定状态、版本和旧新映射。
//
// 三个测试入口：
//   - TestV3P002StateMapping：V3 Task/Operation 迁移表的结构正反例，
//     以及旧五态逐条映射（含未知输入的拒绝/待核对）。
//   - TestV3P002TerminalTaskCreatesSuccessor：终态后续请求判定为
//     "建立关联新 Task"，旧暂停判定为可恢复；并对真实库中的
//     cancelled/completed/interrupted(paused) 记录验证判定与持久状态一致。
//   - TestV3P002RevisionMeanings：五种修订/代际含义分别独立，合同变化
//     不产生新业务合同、历史输入仍可解释。

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestV3P002StateMapping(t *testing.T) {
	t.Run("task_terminal_states_never_revive", func(t *testing.T) {
		for _, terminal := range []V3TaskState{V3TaskSucceeded, V3TaskFailed, V3TaskCancelled} {
			if !terminal.IsTerminal() {
				t.Errorf("%s must be terminal", terminal)
			}
			for _, target := range []V3TaskState{
				V3TaskPending, V3TaskRunning, V3TaskWaiting, V3TaskPaused, V3TaskAcceptance, V3TaskFinalizing,
			} {
				if CanTransitionTask(terminal, target) {
					t.Errorf("terminal %s must not transition to %s", terminal, target)
				}
			}
		}
	})

	t.Run("task_structural_transitions_match_spec", func(t *testing.T) {
		// §5.2 明确列出的迁移必须存在。
		allowed := [][2]V3TaskState{
			{V3TaskPending, V3TaskRunning},
			{V3TaskPending, V3TaskWaiting},
			{V3TaskPending, V3TaskPaused},
			{V3TaskPending, V3TaskFinalizing},
			{V3TaskRunning, V3TaskWaiting},
			{V3TaskRunning, V3TaskPaused},
			{V3TaskRunning, V3TaskAcceptance},
			{V3TaskRunning, V3TaskFinalizing},
			{V3TaskWaiting, V3TaskPending},
			{V3TaskWaiting, V3TaskPaused},
			{V3TaskWaiting, V3TaskFinalizing},
			{V3TaskPaused, V3TaskPending},
			{V3TaskPaused, V3TaskWaiting},
			{V3TaskPaused, V3TaskFinalizing},
			{V3TaskAcceptance, V3TaskPending},
			{V3TaskAcceptance, V3TaskPaused},
			{V3TaskAcceptance, V3TaskFinalizing},
			{V3TaskFinalizing, V3TaskSucceeded},
			{V3TaskFinalizing, V3TaskFailed},
			{V3TaskFinalizing, V3TaskCancelled},
		}
		for _, pair := range allowed {
			if !CanTransitionTask(pair[0], pair[1]) {
				t.Errorf("spec transition %s -> %s missing", pair[0], pair[1])
			}
		}
		// 未列出的跨态必须拒绝（反例抽样）。
		forbidden := [][2]V3TaskState{
			{V3TaskPending, V3TaskSucceeded},   // 不能跳过执行/验收/收尾直接成功
			{V3TaskRunning, V3TaskSucceeded},   // 模型/步骤完成不能直接成功
			{V3TaskWaiting, V3TaskSucceeded},   // 等待不能成功
			{V3TaskPaused, V3TaskRunning},      // 恢复须经待执行重评，不直接回执行中
			{V3TaskAcceptance, V3TaskRunning},  // 验收不直接回执行
			{V3TaskSucceeded, V3TaskRunning},   // 终态不复活
			{V3TaskCancelled, V3TaskPending},   // 终态不复活
			{V3TaskRunning, V3TaskPending},     // 执行中不直接回待执行（经等待/暂停/验收/收尾）
		}
		for _, pair := range forbidden {
			if CanTransitionTask(pair[0], pair[1]) {
				t.Errorf("transition %s -> %s must be rejected", pair[0], pair[1])
			}
		}
	})

	t.Run("operation_transitions_match_spec", func(t *testing.T) {
		allowed := [][2]V3OperationState{
			{V3OperationRegistered, V3OperationDispatched},
			{V3OperationRegistered, V3OperationCancelled},
			{V3OperationDispatched, V3OperationRunning},
			{V3OperationDispatched, V3OperationSucceeded},
			{V3OperationDispatched, V3OperationFailed},
			{V3OperationDispatched, V3OperationReconcile},
			{V3OperationDispatched, V3OperationCancelled},
			{V3OperationRunning, V3OperationSucceeded},
			{V3OperationRunning, V3OperationFailed},
			{V3OperationRunning, V3OperationReconcile},
			{V3OperationRunning, V3OperationCancelled},
			{V3OperationReconcile, V3OperationRegistered}, // 确认未执行后重派
			{V3OperationReconcile, V3OperationSucceeded},
			{V3OperationReconcile, V3OperationFailed},
			{V3OperationReconcile, V3OperationCancelled},
		}
		for _, pair := range allowed {
			if !CanTransitionOperation(pair[0], pair[1]) {
				t.Errorf("spec operation transition %s -> %s missing", pair[0], pair[1])
			}
		}
		forbidden := [][2]V3OperationState{
			{V3OperationRegistered, V3OperationSucceeded},  // 登记不能直接成功
			{V3OperationRegistered, V3OperationRunning},    // 必须先派发
			{V3OperationDispatched, V3OperationRegistered}, // 已派发不能退回登记（经待核对）
			{V3OperationSucceeded, V3OperationRegistered},  // 终态不复活
			{V3OperationCancelled, V3OperationDispatched},  // 终态不复活
			{V3OperationRunning, V3OperationRegistered},    // 执行中不能退回登记
		}
		for _, pair := range forbidden {
			if CanTransitionOperation(pair[0], pair[1]) {
				t.Errorf("operation transition %s -> %s must be rejected", pair[0], pair[1])
			}
		}
	})

	t.Run("legacy_task_mapping_examples", func(t *testing.T) {
		cases := []struct {
			name      string
			snapshot  LegacyTaskSnapshot
			wantState V3TaskState
			wantReconcile bool
		}{
			{"completed_maps_succeeded", LegacyTaskSnapshot{Status: "completed"}, V3TaskSucceeded, false},
			{"cancelled_maps_cancelled", LegacyTaskSnapshot{Status: "cancelled"}, V3TaskCancelled, false},
			{"waiting_user_maps_acceptance", LegacyTaskSnapshot{Status: "waiting_user", StatusReason: "acceptance_required"}, V3TaskAcceptance, false},
			{"waiting_user_pending_publication_reconciles", LegacyTaskSnapshot{Status: "waiting_user", HasPendingPublication: true}, V3TaskAcceptance, true},
			{"active_unknown_read_reconciles", LegacyTaskSnapshot{Status: "active", HasUnknownRead: true}, V3TaskRunning, true},
			{"active_queued_maps_pending", LegacyTaskSnapshot{Status: "active", StatusReason: "queued"}, V3TaskPending, false},
			{"active_waiting_dependencies_maps_waiting", LegacyTaskSnapshot{Status: "active", StatusReason: "waiting_dependencies"}, V3TaskWaiting, false},
			{"active_unrecognized_reason_reconciles", LegacyTaskSnapshot{Status: "active", StatusReason: "mystery"}, V3TaskPending, true},
			{"interrupted_paused_maps_paused", LegacyTaskSnapshot{Status: "interrupted", StatusReason: "paused"}, V3TaskPaused, false},
			{"interrupted_reconciliation_required_reconciles", LegacyTaskSnapshot{Status: "interrupted", StatusReason: "reconciliation_required"}, V3TaskRunning, true},
			{"interrupted_process_reconciles", LegacyTaskSnapshot{Status: "interrupted", StatusReason: "process_interrupted"}, V3TaskRunning, true},
			{"interrupted_unknown_reason_reconciles", LegacyTaskSnapshot{Status: "interrupted", StatusReason: "mystery"}, V3TaskRunning, true},
			// 反例：未识别旧状态必须保留待核对，绝不自动成功或重派。
			{"unrecognized_status_reconciles", LegacyTaskSnapshot{Status: "vanished"}, V3TaskRunning, true},
		}
		for _, c := range cases {
			got := MapLegacyTask(c.snapshot)
			if got.State != c.wantState || got.ReconcileRequired != c.wantReconcile {
				t.Errorf("%s: MapLegacyTask = %+v, want state=%s reconcile=%v",
					c.name, got, c.wantState, c.wantReconcile)
			}
			if got.Reason == "" {
				t.Errorf("%s: mapping must record a reason", c.name)
			}
		}
	})

	t.Run("asset_states_nine_unique_and_managed_group", func(t *testing.T) {
		all := []V3AssetState{
			V3AssetCandidate, V3AssetReview, V3AssetPreDebug, V3AssetDebugging,
			V3AssetPreDelivery, V3AssetActive, V3AssetMaintenance, V3AssetLent, V3AssetDecommission,
		}
		if len(all) != 9 {
			t.Fatalf("asset states = %d, want exactly 9", len(all))
		}
		managed := 0
		for _, s := range all {
			if s.Managed() {
				managed++
			}
		}
		if managed != 6 {
			t.Errorf("managed group size = %d, want 6 (在管是归组而非第十态)", managed)
		}
		if V3AssetLent.Managed() || V3AssetCandidate.Managed() || V3AssetDecommission.Managed() {
			t.Error("lent/candidate/decommission must not be in the managed group")
		}
	})
}

func TestV3P002TerminalTaskCreatesSuccessor(t *testing.T) {
	t.Run("decision_table", func(t *testing.T) {
		cases := []struct {
			state V3TaskState
			want  SuccessorDecision
		}{
			{V3TaskSucceeded, SuccessorCreateNew},
			{V3TaskFailed, SuccessorCreateNew},
			{V3TaskCancelled, SuccessorCreateNew},
			{V3TaskPaused, SuccessorResumeOriginal},
			{V3TaskState("mystery"), SuccessorRejected},
		}
		for _, c := range cases {
			if got := DecideTaskSuccessor(c.state); got != c.want {
				t.Errorf("DecideTaskSuccessor(%s) = %s, want %s", c.state, got, c.want)
			}
		}
	})

	// 对真实持久记录验证：映射结论与 DecideTaskSuccessor 共同作用，
	// cancelled/completed 得到"建立关联新 Task"，paused 得到"恢复原任务"。
	t.Run("persisted_records_drive_decision", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "successor.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()

		// cancelled：创建后直接 stop（queued 取消）。
		cancelled, err := store.Submit(ctx, "v3p002-cancelled", "临时事项", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		d, err := store.Detail(cancelled.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Control(ctx, cancelled.TaskID, "stop", d.Task.Revision); err != nil {
			t.Fatal(err)
		}

		// completed：完成并验收。
		completed, err := store.Submit(ctx, "v3p002-completed", "整理材料", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		store.execute(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
			if err := emit("# 成果\n"); err != nil {
				return nil, err
			}
			input, output := int64(8), int64(4)
			return &TokenUsage{InputTokens: &input, OutputTokens: &output}, nil
		}), run, RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 512})
		store.reconcileFinalizations(ctx)
		d, err = store.Detail(completed.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Accept(ctx, completed.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
			t.Fatal(err)
		}

		// paused：领取后暂停。
		paused, err := store.Submit(ctx, "v3p002-paused", "会议纪要", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil || claimed.ID != paused.ID {
			t.Fatalf("claim paused run: %+v %v", claimed, err)
		}
		d, err = store.Detail(paused.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Control(ctx, paused.TaskID, "pause", d.Task.Revision); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(claimed, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}

		// 逐条读取持久状态 → 映射 → 判定。
		for name, tc := range map[string]struct {
			taskID       string
			wantDecision SuccessorDecision
		}{
			"cancelled": {cancelled.TaskID, SuccessorCreateNew},
			"completed": {completed.TaskID, SuccessorCreateNew},
			"paused":    {paused.TaskID, SuccessorResumeOriginal},
		} {
			detail, err := store.Detail(tc.taskID, true)
			if err != nil {
				t.Fatalf("detail %s: %v", name, err)
			}
			mapping := MapLegacyTask(LegacyTaskSnapshot{Status: detail.Task.Status, StatusReason: detail.Task.StatusReason})
			decision := DecideTaskSuccessor(mapping.State)
			if decision != tc.wantDecision {
				t.Errorf("%s: persisted status %q/%q mapped to %s, decision %s, want %s",
					name, detail.Task.Status, detail.Task.StatusReason, mapping.State, decision, tc.wantDecision)
			}
		}

		// 反例：已取消 Task 的旧客户端重复 Resume 不能复活原任务
		// （正式 Resume 入口切换在 P1-11 验收；本项先固定判定）。
		if got := DecideTaskSuccessor(MapLegacyTask(LegacyTaskSnapshot{Status: "cancelled"}).State); got != SuccessorCreateNew {
			t.Errorf("cancelled task follow-up must create a successor, got %s", got)
		}
	})
}

func TestV3P002RevisionMeanings(t *testing.T) {
	t.Run("five_revision_kinds_are_distinct", func(t *testing.T) {
		kinds := []RevisionKind{
			RevisionStorageRow, RevisionWorkContract, RevisionControl,
			RevisionAssignmentGeneration, RevisionExecutionGeneration,
		}
		seen := map[RevisionKind]bool{}
		for _, k := range kinds {
			if seen[k] {
				t.Errorf("duplicate revision kind %q", k)
			}
			seen[k] = true
			if k == "" {
				t.Error("revision kind must have a stable serialization")
			}
		}
		if len(seen) != 5 {
			t.Errorf("revision kinds = %d, want 5", len(seen))
		}
	})

	// 状态变化（storage_row 递增）不制造新的业务合同（work_contract 快照不变）；
	// 合同修订使旧计划失效但历史仍可解释。
	t.Run("state_change_does_not_create_new_contract", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "contract.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()

		created, err := store.Submit(ctx, "v3p002-contract", "整理季度汇报", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// 首次绑定产生合同快照（work_contract 首个版本）与分配/执行代际。
		// 现状：创建时的受理已使 tasks.revision 递增一次，故首个绑定记录的是
		// 当前 storage_row revision（此处为 2），而非从 1 开始——这正是
		// P0-02.2 要区分的两种版本含义。
		var contractRevision, assignmentGen, executionGen int
		if err := store.db.QueryRow(`SELECT b.task_revision,b.assignment_generation,b.generation
FROM run_bindings b WHERE b.run_id=?`, run.ID).Scan(&contractRevision, &assignmentGen, &executionGen); err != nil {
			t.Fatalf("scan run binding: %v", err)
		}
		if contractRevision < 1 || assignmentGen != 1 || executionGen != 0 {
			t.Fatalf("initial binding = contract v%d assignment g%d execution g%d, want contract>=v1/g1/g0",
				contractRevision, assignmentGen, executionGen)
		}

		// 控制动作（暂停）使 storage_row 递增，但合同快照保持不变。
		d, err := store.Detail(created.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		beforeRevision := d.Task.Revision
		if err := store.Control(ctx, created.TaskID, "pause", beforeRevision); err != nil {
			t.Fatal(err)
		}
		d, err = store.Detail(created.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if d.Task.Revision <= beforeRevision {
			t.Errorf("storage_row revision did not increase: %d -> %d", beforeRevision, d.Task.Revision)
		}
		// 状态变化（暂停）后：合同快照仍只有"创建 + 首次绑定"两条（创建时
		// 触发器写入 v1，bindRun 写入当前 revision 的绑定快照）；控制动作
		// 不产生新业务合同快照。
		var snapshotCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM task_revisions WHERE task_id=?", created.TaskID).Scan(&snapshotCount); err != nil {
			t.Fatal(err)
		}
		if snapshotCount != 2 {
			t.Errorf("state change produced %d contract snapshots, want exactly 2 (创建+绑定；历史输入仍可解释)", snapshotCount)
		}

		// 合同修订（补充要求改变目标）使 storage_row 再次递增；
		// 旧快照保留，新运行必须按新合同重新绑定（正式切换在 P1 各项）。
		if _, err := store.Resume(ctx, created.TaskID, "v3p002-resume", d.Task.Revision); err == nil {
			// Resume 产生新 Run 与新一代际；历史 binding 不被改写。
			var bindings int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM run_bindings WHERE run_id=?", run.ID).Scan(&bindings); err != nil {
				t.Fatal(err)
			}
			if bindings != 1 {
				t.Errorf("resume rewrote history: old run has %d bindings, want 1", bindings)
			}
		}
	})

	t.Run("control_revision_tracks_stale_manifests", func(t *testing.T) {
		// control_revision 记录装配时的 tasks.revision；pause/stop/revise 使其过期。
		// 这里固定该语义的常量化引用，装配核对逻辑在 P1/P5 实现。
		if RevisionControl == RevisionStorageRow || RevisionControl == RevisionWorkContract {
			t.Error("control revision must be a distinct kind from storage row and work contract")
		}
		if RevisionExecutionGeneration == RevisionAssignmentGeneration {
			t.Error("execution generation and assignment generation are different dimensions")
		}
	})
}
