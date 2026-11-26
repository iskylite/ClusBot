package work

// 验收：绑定具体动作的确认与人工接管。
//
// 三个测试入口：
//   - TestApprovalBoundSnapshot：确认绑定主体、动作、实际对象、参数摘要、
//     合同/策略/资产版本、决定者和期限；重复点击只产生一次有效决定。
//   - TestApprovalDoesNotGrantPermission：过期、错对象/参数、无权确认均
//     拒绝；确认不能补足缺失权限；缺失权限时显示原因，不诱导用户用确认按钮
//     扩大权限。
//   - TestExplicitResidualHandover：人工接管未知效果须记录明确授权、
//     接收人、未决操作、隔离措施、残余风险和后续责任；Task 不能凭普通取消
//     确认丢弃未知外部效果；例外接管仍留证。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupApprovalsStore 创建存储。
func setupApprovalsStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// approvalFixture 构造一个确认请求。
func approvalFixture(requestKey, subject, action, objectRef string, now int64) Approval {
	return Approval{
		Subject: subject, Action: action, ObjectRef: objectRef,
		ParamsDigest: digest("参数"), ContractRevision: 1, PolicyVersion: 1, AssetRevision: 1,
		DecidedBy: "user:alice", ExpiresAt: now + 100000, RequestKey: requestKey,
	}
}

// TestApprovalBoundSnapshot 绑定快照。
func TestApprovalBoundSnapshot(t *testing.T) {
	t.Run("approval_bound_to_all_fields", func(t *testing.T) {
		store := setupApprovalsStore(t, "bound.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		approval, err := requestApproval(tx, approvalFixture("req-1", "user:alice", "write", "asset-x", now), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 确认绑定全部字段。
		if approval.Subject != "user:alice" || approval.Action != "write" || approval.ObjectRef != "asset-x" ||
			approval.ParamsDigest == "" || approval.ContractRevision != 1 || approval.DecidedBy != "user:alice" {
			t.Errorf("approval incomplete: %+v", approval)
		}
	})

	t.Run("duplicate_click_single_valid_decision", func(t *testing.T) {
		store := setupApprovalsStore(t, "dup.db")
		now := recordsTestNow()
		// 授权确认权限。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "confirm", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 请求确认。
		tx, _ = store.db.Begin()
		approval, err := requestApproval(tx, approvalFixture("req-dup", "user:alice", "write", "asset-x", now), now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 重复点击：只产生一次有效决定。
		tx, _ = store.db.Begin()
		first, err := decideApproval(tx, approval.RequestKey, "approved", "user:alice", Approval{ContractRevision: 1, PolicyVersion: 1, AssetRevision: 1}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		second, err := decideApproval(tx, approval.RequestKey, "rejected", "user:alice", Approval{}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if first.Decision != "approved" {
			t.Errorf("first decision = %s, want approved", first.Decision)
		}
		if second.Decision != "approved" {
			t.Errorf("second decision = %s, want approved (重复点击只产生一次有效决定)", second.Decision)
		}
		if second.ApprovalID != first.ApprovalID {
			t.Error("duplicate click created new approval")
		}
	})

	t.Run("content_version_change_invalidates_old_approval", func(t *testing.T) {
		store := setupApprovalsStore(t, "stale.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "confirm", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		approval, _ := requestApproval(tx, approvalFixture("req-stale", "user:alice", "write", "asset-x", now), now)
		tx.Commit()
		// 合同版本变化后：旧确认无效需重判。
		tx, _ = store.db.Begin()
		_, err := decideApproval(tx, approval.RequestKey, "approved", "user:alice", Approval{ContractRevision: 999}, now)
		tx.Rollback()
		if err == nil {
			t.Error("old approval not invalidated after version change (内容/权限/版本变化后旧确认无效)")
		}
		if !strings.Contains(err.Error(), "版本已变化") {
			t.Errorf("stale reason missing: %v", err)
		}
	})
}

// TestApprovalDoesNotGrantPermission 确认不授权限。
func TestApprovalDoesNotGrantPermission(t *testing.T) {
	t.Run("expired_approval_rejected", func(t *testing.T) {
		store := setupApprovalsStore(t, "expired.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "confirm", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 过期确认：拒绝。
		tx, _ = store.db.Begin()
		approval, _ := requestApproval(tx, Approval{
			Subject: "user:alice", Action: "write", ObjectRef: "asset-x", ParamsDigest: digest("参数"),
			ContractRevision: 1, DecidedBy: "user:alice", ExpiresAt: now - 1000, RequestKey: "req-expired",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		_, err := decideApproval(tx, approval.RequestKey, "approved", "user:alice", Approval{}, now+2000)
		tx.Rollback()
		if err == nil {
			t.Error("expired approval not rejected (过期确认拒绝)")
		}
	})

	t.Run("unauthorized_confirmer_rejected", func(t *testing.T) {
		store := setupApprovalsStore(t, "unauth.db")
		now := recordsTestNow()
		// 请求确认（无确认权限的决定者）。
		tx, _ := store.db.Begin()
		approval, _ := requestApproval(tx, approvalFixture("req-unauth", "user:alice", "write", "asset-x", now), now)
		tx.Commit()
		// 无权确认：拒绝（确认不能补足缺失权限）。
		tx, _ = store.db.Begin()
		_, err := decideApproval(tx, approval.RequestKey, "approved", "user:mallory", Approval{}, now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized confirmer not rejected (无权确认拒绝)")
		}
		if !strings.Contains(err.Error(), "无权确认") && !strings.Contains(err.Error(), "授权拒绝") {
			t.Errorf("unauthorized confirmer reason unclear: %v", err)
		}
	})

	t.Run("approval_does_not_substitute_missing_permission", func(t *testing.T) {
		store := setupApprovalsStore(t, "nosubstitute.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "confirm", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		approval, _ := requestApproval(tx, approvalFixture("req-nosub", "user:alice", "write", "asset-x", now), now)
		tx.Commit()
		// 确认通过：但仍需授权与领域准入（确认不补足缺失权限）。
		tx, _ = store.db.Begin()
		decided, err := decideApproval(tx, approval.RequestKey, "approved", "user:alice", Approval{ContractRevision: 1, PolicyVersion: 1, AssetRevision: 1}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if decided.Decision != "approved" {
			t.Fatal("approval not approved")
		}
		// 确认通过与实际派发分开：approved 不等于 Operation 成功。
		if err := confirmApprovedNotDispatch(decided); err != nil {
			t.Fatalf("approved confirmation rejected: %v", err)
		}
		// 确认通过后仍需授权核对（user:alice 无 write 权限 → 派发拒绝）。
		tx, _ = store.db.Begin()
		authDecision, _ := decideAuthorization(tx, "user:alice", "write", "asset-x", "", now, false)
		tx.Commit()
		if authDecision.Outcome == "allowed" {
			t.Error("approval granted write permission (确认不能补足缺失权限)")
		}
	})

	t.Run("missing_permission_shows_reason_not_prompt_expand", func(t *testing.T) {
		store := setupApprovalsStore(t, "reason.db")
		now := recordsTestNow()
		// 缺失权限时显示原因（不诱导用户用确认按钮扩大权限）。
		tx, _ := store.db.Begin()
		decision, _ := decideAuthorization(tx, "user:mallory", "write", "asset-x", "", now, false)
		tx.Commit()
		if decision.Outcome != "denied" {
			t.Fatal("unauthorized not denied")
		}
		if decision.Reason == "" {
			t.Error("denial reason missing (缺失权限时显示原因)")
		}
		// 原因不含"确认即可"诱导（不诱导扩大权限）。
		if strings.Contains(decision.Reason, "确认即可") || strings.Contains(decision.Reason, "点击确认") {
			t.Errorf("denial reason induces permission expansion: %s", decision.Reason)
		}
	})
}

// TestExplicitResidualHandover 明确残余接管。
func TestExplicitResidualHandover(t *testing.T) {
	t.Run("takeover_records_all_responsibility_fields", func(t *testing.T) {
		store := setupApprovalsStore(t, "takeover.db")
		now := recordsTestNow()
		// 人工接管未知效果：记录明确授权、接收人、未决操作、隔离措施、残余风险、后续责任。
		tx, _ := store.db.Begin()
		takeover, err := recordManualTakeover(tx, ManualTakeover{
			TaskID:                 "task-unknown",
			AuthorizedBy:           "admin",
			TakenOverBy:            "user:ops",
			PendingOperations:      "操作123未完成",
			IsolationMeasures:      "已隔离设备",
			ResidualRisks:          "可能存在残留写",
			FollowUpResponsibility: "人工核对并清理",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 接管记录含全部责任字段。
		if takeover.AuthorizedBy != "admin" || takeover.TakenOverBy != "user:ops" ||
			takeover.PendingOperations == "" || takeover.IsolationMeasures == "" ||
			takeover.ResidualRisks == "" || takeover.FollowUpResponsibility == "" {
			t.Errorf("takeover incomplete: %+v", takeover)
		}
		// 接管经审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='manual_takeover' AND task_id='task-unknown'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("takeover not audited: count=%d (例外接管留证)", count)
		}
	})

	t.Run("takeover_missing_responsibility_rejected", func(t *testing.T) {
		store := setupApprovalsStore(t, "noresponsibility.db")
		now := recordsTestNow()
		// 缺后续责任：拒绝。
		tx, _ := store.db.Begin()
		_, err := recordManualTakeover(tx, ManualTakeover{
			TaskID: "task-x", AuthorizedBy: "admin", TakenOverBy: "user:ops",
		}, now)
		tx.Rollback()
		if err == nil {
			t.Error("takeover without responsibility not rejected (接管须记录后续责任)")
		}
	})

	t.Run("task_cannot_discard_unknown_effect_by_cancel", func(t *testing.T) {
		store := setupApprovalsStore(t, "unknown.db")
		now := recordsTestNow()
		ctx := context.Background()
		run, _ := store.Submit(ctx, "v3p303-unknown", "整理汇报", "", "", true)
		taskID := run.TaskID
		// 建立任务与未知效果（待核对结果）。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
		input, output := int64(10), int64(5)
		recordReport(tx, ResultReport{ResultKey: "unknown-result", TaskID: taskID, Summary: "未知", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
		tx.Commit()
		// 普通取消：不能丢弃未知外部效果（收尾记录保留待核对）。
		tx, _ = store.db.Begin()
		current, _ := readTaskState(tx, taskID)
		req := ControlRequest{RequestKey: "cancel-unknown", TaskID: taskID, DecidedBy: "user", Kind: "cancel", Decision: "cancel"}
		result, err := cancelTask(tx, req, TaskGuard{ExpectedStorageRevision: current.StorageRevision, Actor: "user"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 收尾记录保留待核对（Task 不能凭普通取消确认丢弃未知外部效果）。
		if result.Finalization.PendingDecisions != 1 {
			t.Errorf("pending decisions = %d, want 1 (取消不能丢弃未知外部效果)", result.Finalization.PendingDecisions)
		}
	})

	t.Run("pending_approvals_via_responsibility_entry", func(t *testing.T) {
		store := setupApprovalsStore(t, "pending.db")
		now := recordsTestNow()
		// 通过责任入口返回待决定事项。
		tx, _ := store.db.Begin()
		requestApproval(tx, approvalFixture("req-p1", "user:alice", "write", "asset-x", now), now)
		requestApproval(tx, approvalFixture("req-p2", "user:alice", "write", "asset-y", now), now)
		tx.Commit()
		pending, err := listPendingApprovals(store.db, "user:alice", now)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 2 {
			t.Errorf("pending approvals = %d, want 2 (责任入口返回待决定事项)", len(pending))
		}
		for _, p := range pending {
			if p.Reason == "" {
				t.Error("pending reason missing")
			}
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
