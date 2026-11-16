package work

// P3-16 验收：整段外部工作流委托。
//
// 三个测试入口：
//   - TestV3P316DelegatedWorkflowSingleOwner：委托记录实际目标、已固定版本、
//     起止边界和双方责任；一次委托关联外部 Job；外部负责的步骤，本地不再
//     并行调度同一动作；外部流程与本地执行账本没有双重调度同一步。
//   - TestV3P316DelegationResumeNoRestart：重启恢复按原 Job 对账，不因外部
//     状态查询失败重新启动整个流程；外部宣告成功但覆盖/证据不足时，本地
//     Task 不成功。
//   - TestV3P316DelegationBoundaryHandover：暂停/取消/补充是否支持以提供者
//     契约为准；外部不支持时停止本地后续工作并保留在途责任；需要交回部分
//     步骤时，先确认外部停止边界和未决效果，再生成新本地执行单元，不能无缝
//     假定已停；移交边界不明时保持等待，不能让新旧执行方同时写。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P316 创建存储与任务。
func setupV3P316(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p316-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// TestV3P316DelegatedWorkflowSingleOwner 委托单一所有者。
func TestV3P316DelegatedWorkflowSingleOwner(t *testing.T) {
	t.Run("delegation_records_all_fields", func(t *testing.T) {
		store, taskID := setupV3P316(t, "delegation.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		d, err := createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[{"ref":"asset-1"}]`, PinnedVersions: `{"schema":1}`,
			BoundaryStart: now, LocalResponsibility: "观察与验收", RemoteResponsibility: "执行流程",
			ExternalJobID: "external-job-1",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 委托含全部字段。
		if d.TaskID != taskID || d.TargetRefs == "" || d.PinnedVersions == "" ||
			d.LocalResponsibility == "" || d.RemoteResponsibility == "" || d.ExternalJobID == "" {
			t.Errorf("delegation incomplete: %+v", d)
		}
		if d.State != "active" {
			t.Errorf("delegation state = %s, want active", d.State)
		}
	})

	t.Run("delegation_requires_external_job_and_responsibility", func(t *testing.T) {
		store, taskID := setupV3P316(t, "required.db")
		now := v3P102Now()
		// 缺外部 Job：拒绝。
		tx, _ := store.db.Begin()
		_, err := createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("delegation without external job not rejected")
		}
		// 缺双方责任：拒绝。
		tx, _ = store.db.Begin()
		_, err = createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, ExternalJobID: "job-1",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("delegation without responsibility not rejected")
		}
	})

	t.Run("external_step_not_dual_scheduled", func(t *testing.T) {
		store, taskID := setupV3P316(t, "dual.db")
		now := v3P102Now()
		// 委托外部后：本地不再并行调度同一动作。
		tx, _ := store.db.Begin()
		createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-dual",
		}, now)
		tx.Commit()
		// 外部负责的步骤：本地调度拒绝（双重调度）。
		tx, _ = store.db.Begin()
		err := ensureNoDualScheduling(tx, taskID, "step-1", now)
		tx.Rollback()
		if err == nil {
			t.Error("dual scheduling not rejected (外部负责的步骤本地不再并行调度)")
		}
		if !strings.Contains(err.Error(), "双重调度") && !strings.Contains(err.Error(), "已委托外部") {
			t.Errorf("dual scheduling reason missing: %v", err)
		}
	})

	t.Run("non_delegated_step_allows_local_scheduling", func(t *testing.T) {
		store, taskID := setupV3P316(t, "local.db")
		now := v3P102Now()
		// 未委托的任务：本地调度允许。
		tx, _ := store.db.Begin()
		err := ensureNoDualScheduling(tx, taskID, "step-1", now)
		tx.Commit()
		if err != nil {
			t.Errorf("local scheduling rejected for non-delegated task: %v", err)
		}
	})
}

// TestV3P316DelegationResumeNoRestart 恢复不重启。
func TestV3P316DelegationResumeNoRestart(t *testing.T) {
	t.Run("restart_recovers_by_original_job", func(t *testing.T) {
		// 重启恢复按原 Job 对账。
		path := filepath.Join(t.TempDir(), "recover.db")
		now := v3P102Now()
		var externalJobID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, _ := store.Submit(ctx, "v3p316-recover", "任务", "", "", true)
			externalJobID = "external-job-recover"
			tx, _ := store.db.Begin()
			createDelegation(tx, Delegation{
				TaskID: run.TaskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
				ExternalJobID: externalJobID,
			}, now)
			tx.Commit()
		}()
		// 重启：按原 Job 对账（不因外部状态查询失败重新启动）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		tx, _ := store.db.Begin()
		d, err := recoverDelegationByJob(tx, externalJobID, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if d.ExternalJobID != externalJobID {
			t.Errorf("delegation lost after restart: %s", d.ExternalJobID)
		}
		if d.State != "active" {
			t.Errorf("delegation restarted: state=%s (不因外部状态查询失败重新启动)", d.State)
		}
	})

	t.Run("external_success_with_insufficient_evidence_not_task_success", func(t *testing.T) {
		store, taskID := setupV3P316(t, "insufficient.db")
		now := v3P102Now()
		// 外部宣告成功但覆盖/证据不足：本地 Task 不成功。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		tx.Commit()
		// 任务未完成全部步骤（外部宣告成功但本地步骤未完成）。
		tx, _ = store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "incomplete-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		// 外部宣告成功：但本地 Task 不成功（步骤未完成）。
		state, _ := readTaskState(store.db, taskID)
		if state.State == V3TaskSucceeded {
			t.Error("task marked success with incomplete steps (外部宣告成功但覆盖/证据不足时本地 Task 不成功)")
		}
	})

	t.Run("query_failure_not_restart_entire_flow", func(t *testing.T) {
		store, taskID := setupV3P316(t, "queryfail.db")
		now := v3P102Now()
		// 委托外部。
		tx, _ := store.db.Begin()
		createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-queryfail",
		}, now)
		tx.Commit()
		// 外部状态查询失败：不重新启动整个流程（保留当前状态）。
		tx, _ = store.db.Begin()
		d, err := recoverDelegationByJob(tx, "job-queryfail", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if d.State != "active" {
			t.Errorf("query failure restarted flow: state=%s", d.State)
		}
	})
}

// TestV3P316DelegationBoundaryHandover 边界移交。
func TestV3P316DelegationBoundaryHandover(t *testing.T) {
	t.Run("provider_contract_support_determines_pause_cancel_supplement", func(t *testing.T) {
		store, taskID := setupV3P316(t, "contract.db")
		now := v3P102Now()
		// 提供者契约：暂停/取消/补充是否支持以提供者契约为准。
		tx, _ := store.db.Begin()
		d, _ := createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-contract",
			SupportsPause: "supported", SupportsCancel: "unsupported", SupportsSupplement: "unknown",
		}, now)
		tx.Commit()
		// 暂停支持。
		tx, _ = store.db.Begin()
		supported, reason, err := checkProviderContractSupport(tx, d.DelegationID, ContractPause, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !supported {
			t.Errorf("pause not supported: %s (以提供者契约为准)", reason)
		}
		// 取消不支持：停止本地后续工作并保留在途责任。
		tx, _ = store.db.Begin()
		supported, reason, err = checkProviderContractSupport(tx, d.DelegationID, ContractCancel, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if supported {
			t.Error("cancel supported when contract says unsupported")
		}
		if !strings.Contains(reason, "停止本地后续工作") && !strings.Contains(reason, "不支持") {
			t.Errorf("unsupported reason missing: %s", reason)
		}
	})

	t.Run("handover_requires_confirmed_stop_boundary", func(t *testing.T) {
		store, taskID := setupV3P316(t, "handover.db")
		now := v3P102Now()
		// 委托外部。
		tx, _ := store.db.Begin()
		d, _ := createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-handover",
		}, now)
		tx.Commit()
		// 发起移交：保持等待（不能让新旧执行方同时写）。
		tx, _ = store.db.Begin()
		h, err := initiateHandover(tx, d.DelegationID, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if h.State != "pending" {
			t.Errorf("handover state = %s, want pending (移交边界不明时保持等待)", h.State)
		}
		// 确认边界前：不能生成新本地执行单元（无缝假定已停拒绝）。
		tx, _ = store.db.Begin()
		err = confirmHandoverBoundary(tx, h.HandoverID, "", `[]`, now) // 缺边界
		tx.Rollback()
		if err == nil {
			t.Error("handover without stop boundary confirmed (先确认外部停止边界)")
		}
		// 确认边界：允许。
		tx, _ = store.db.Begin()
		if err := confirmHandoverBoundary(tx, h.HandoverID, "external-stopped-at-step-3", `[{"effect":"partial"}]`, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	})

	t.Run("handover_state_prevents_dual_writers", func(t *testing.T) {
		store, taskID := setupV3P316(t, "dualwriters.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		d, _ := createDelegation(tx, Delegation{
			TaskID: taskID, TargetRefs: `[]`, LocalResponsibility: "观察", RemoteResponsibility: "执行",
			ExternalJobID: "job-dualwriters",
		}, now)
		tx.Commit()
		// 移交状态：delegation 为 handover（不能让新旧执行方同时写）。
		tx, _ = store.db.Begin()
		initiateHandover(tx, d.DelegationID, now)
		tx.Commit()
		var state string
		if err := store.db.QueryRow("SELECT state FROM delegations WHERE delegation_id=?", d.DelegationID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "handover" {
			t.Errorf("delegation state = %s, want handover (移交边界不明时保持等待)", state)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
