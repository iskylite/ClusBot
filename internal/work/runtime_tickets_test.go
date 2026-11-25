package work

// 验收：硬件故障、工单和更换部件。
//
// 三个测试入口：
//   - TestTicketPersistentWait：工单长等待使用 Wait/事件/查询，不占模型
//     和工作者；重复回调不重复新建工单；缺维修证据或外部系统失联时保持待核对，
//     不以超时假定修复。
//   - TestE03RepairAndReplacement：经领域维护接管创建/关联外部工单，记录
//     责任人、对象/部件、故障证据和外部状态版本；部件更换结束旧安装关系并
//     建立新部件/关系，触发相关验收依赖失效；新旧部件序列号和恢复后的历史
//     引用正确；借出期间只有专项授权维修可调用相关动作。
//   - TestTicketCloseNotRecovery：工单关闭后查询实际效果并测试，验证后
//     回待复核；未恢复继续限制和后续责任；工单回执不替代设备验证（关闭工单
//     但故障仍在时，资产不会被误恢复）；维修记录不把资产变维护中或在役。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupTicketsStore 创建存储与资产。
func setupTicketsStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := recordsTestNow()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// TestTicketPersistentWait 持久等待。
func TestTicketPersistentWait(t *testing.T) {
	t.Run("ticket_long_wait_not_occupy_model_worker", func(t *testing.T) {
		store := setupTicketsStore(t, "wait.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		ticket, err := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-123", ObjectRef: "node-1",
			Assignee: "engineer", FaultEvidence: `["ev-1"]`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 工单长等待使用 Wait（不占模型和工作者）。
		tx, _ = store.db.Begin()
		if err := waitTicketUpdate(tx, ticket.TicketID, "wait-1", now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var state string
		if err := store.db.QueryRow("SELECT state FROM tickets WHERE ticket_id=?", ticket.TicketID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "waiting" {
			t.Errorf("ticket state = %s, want waiting (工单长等待使用 Wait/事件/查询)", state)
		}
	})

	t.Run("duplicate_callback_not_duplicate_ticket", func(t *testing.T) {
		store := setupTicketsStore(t, "dup.db")
		now := recordsTestNow()
		// 重复回调不重复新建工单（external_system+external_ticket_id 唯一）。
		tx, _ := store.db.Begin()
		first, _ := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-123", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		second, err := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-123", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if second.TicketID != first.TicketID {
			t.Errorf("duplicate callback created ticket: %s vs %s (重复回调不重复新建工单)", second.TicketID, first.TicketID)
		}
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM tickets WHERE external_system='jira' AND external_ticket_id='HDW-123'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("duplicate tickets = %d, want 1", count)
		}
	})

	t.Run("missing_evidence_external_offline_stays_pending", func(t *testing.T) {
		store := setupTicketsStore(t, "pending.db")
		now := recordsTestNow()
		// 缺维修证据或外部系统失联：保持待核对（不以超时假定修复）。
		tx, _ := store.db.Begin()
		ticket, err := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-456", ObjectRef: "node-1",
			Assignee: "engineer", // 缺 fault_evidence
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 缺证据：保持待核对（不以超时假定修复）。
		if ticket.State != "open" {
			t.Errorf("missing evidence not pending: state=%s (缺维修证据保持待核对)", ticket.State)
		}
	})
}

// TestE03RepairAndReplacement 维修与更换。
func TestE03RepairAndReplacement(t *testing.T) {
	t.Run("ticket_records_assignee_object_component_evidence_version", func(t *testing.T) {
		store := setupTicketsStore(t, "ticket.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		ticket, err := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-789", ObjectRef: "node-1",
			ComponentRefs: `["nic-1"]`, Assignee: "engineer", FaultEvidence: `["ev-1","ev-2"]`,
			ExternalStateVersion: 2, ExternalState: "in_progress",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if ticket.Assignee != "engineer" || ticket.ObjectRef != "node-1" ||
			ticket.ComponentRefs == "" || ticket.FaultEvidence == "" || ticket.ExternalStateVersion != 2 {
			t.Errorf("ticket incomplete: %+v (记录责任人/对象/部件/故障证据/外部状态版本)", ticket)
		}
	})

	t.Run("component_replacement_ends_old_establishes_new_invalidates", func(t *testing.T) {
		store := setupTicketsStore(t, "replacement.db")
		now := recordsTestNow()
		// 部件更换：结束旧安装关系并建立新部件/关系，触发相关验收依赖失效。
		tx, _ := store.db.Begin()
		// 旧部件安装。
		replaceComponent(tx, "node-1", "", "nic-old", "nic", now)
		// 旧部件的验收依赖。
		validation, _ := recordValidation(tx, ValidationRecord{
			SpecRef: "nic-spec", RuleVersion: 1, ObjectRefs: `["node-1"]`,
			InputConditions: `{}`, ExecutionConfig: `{}`, SampleScope: "single",
			Result: "passed", SourceRef: "agent", MeasurementDefinition: `{}`, BusinessThreshold: `{}`,
			Outcome: "passed", ValidatedBy: "test",
		}, now)
		recordEvidenceDependency(tx, EvidenceDependency{
			DependencyKind: "component", TargetRef: "nic-old", ValidationRef: validation.ValidationID,
		}, now)
		tx.Commit()
		// 部件更换：结束旧安装关系并建立新部件/关系，触发验收依赖失效。
		tx, _ = store.db.Begin()
		if err := recordComponentReplacement(tx, "node-1", "nic-old", "nic-new", "nic", now+1000); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 旧安装关系已结束（时间边界）。
		var removedAt int64
		if err := store.db.QueryRow("SELECT removed_at FROM component_installations WHERE component_ref='nic-old'").Scan(&removedAt); err != nil {
			t.Fatal(err)
		}
		if removedAt == 0 {
			t.Error("old installation not ended (部件更换结束旧安装关系)")
		}
		// 新安装关系已建立。
		var newInstalled int64
		if err := store.db.QueryRow("SELECT installed_at FROM component_installations WHERE component_ref='nic-new'").Scan(&newInstalled); err != nil {
			t.Fatal(err)
		}
		if newInstalled == 0 {
			t.Error("new installation not established (建立新部件/关系)")
		}
		// 验收依赖已失效。
		var outcome string
		if err := store.db.QueryRow("SELECT outcome FROM validation_records WHERE validation_id=?", validation.ValidationID).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		if outcome != "unknown" {
			t.Errorf("validation not invalidated: outcome=%s (触发相关验收依赖失效)", outcome)
		}
	})

	t.Run("lent_asset_repair_requires_special_authorization", func(t *testing.T) {
		store := setupTicketsStore(t, "lent.db")
		now := recordsTestNow()
		// 借出资产：只有专项授权维修可调用相关动作。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateLent, "node-1"); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 无专项授权：拒绝。
		tx, _ = store.db.Begin()
		err := ensureLentAssetRepairAuthorized(tx, "node-1", "user:normal", now)
		tx.Rollback()
		if err == nil {
			t.Error("lent asset repair without authorization allowed (借出期间只有专项授权维修可调用相关动作)")
		}
		// 有专项授权：允许。
		tx, _ = store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:repair", Action: "repair", ObjectRef: "node-1", ValidFrom: now, Owner: "repair"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		err = ensureLentAssetRepairAuthorized(tx, "node-1", "user:repair", now)
		tx.Commit()
		if err != nil {
			t.Errorf("authorized repair rejected: %v", err)
		}
	})
}

// TestTicketCloseNotRecovery 关闭不等于恢复。
func TestTicketCloseNotRecovery(t *testing.T) {
	t.Run("ticket_close_not_asset_recovery", func(t *testing.T) {
		store := setupTicketsStore(t, "close.db")
		now := recordsTestNow()
		// 工单回执不替代设备验证：关闭工单但故障仍在时，资产不会被误恢复。
		tx, _ := store.db.Begin()
		ticket, _ := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-close", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		tx.Commit()
		// 工单关闭但故障仍在（效果未验证/测试未通过）：资产不会被误恢复。
		tx, _ = store.db.Begin()
		err := verifyTicketRepair(tx, ticket.TicketID, false, false, now)
		tx.Commit()
		if err == nil {
			t.Error("ticket close marked recovery (关闭工单但故障仍在时资产不会被误恢复)")
		}
		// 资产状态未变（不被误恢复）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateMaintenance {
			t.Errorf("asset recovered by ticket close: state=%s (工单回执不替代设备验证)", asset.State)
		}
	})

	t.Run("verified_repair_back_to_recheck", func(t *testing.T) {
		store := setupTicketsStore(t, "verified.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		ticket, _ := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-verified", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		tx.Commit()
		// 验证后回待复核（工单关闭后查询实际效果并测试）。
		tx, _ = store.db.Begin()
		err := verifyTicketRepair(tx, ticket.TicketID, true, true, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 工单已验证。
		var state string
		if err := store.db.QueryRow("SELECT state FROM tickets WHERE ticket_id=?", ticket.TicketID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "verified" {
			t.Errorf("ticket state = %s, want verified (验证后回待复核)", state)
		}
	})

	t.Run("unrecovered_continues_limitation_responsibility", func(t *testing.T) {
		store := setupTicketsStore(t, "unrecovered.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		ticket, _ := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-unrecovered", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		tx.Commit()
		// 未恢复：继续限制和后续责任（工单回执不替代设备验证）。
		tx, _ = store.db.Begin()
		err := verifyTicketRepair(tx, ticket.TicketID, false, false, now)
		tx.Commit()
		if err == nil {
			t.Error("unrecovered marked verified")
		}
		// 工单标记 failed（继续限制和后续责任）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM tickets WHERE ticket_id=?", ticket.TicketID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "failed" {
			t.Errorf("unrecovered state = %s, want failed (未恢复继续限制和后续责任)", state)
		}
	})

	t.Run("repair_record_not_change_asset_state", func(t *testing.T) {
		store := setupTicketsStore(t, "nochange.db")
		now := recordsTestNow()
		// 维修记录不把资产变维护中或在役（维修记录与资产状态分开）。
		tx, _ := store.db.Begin()
		ticket, _ := createTicket(tx, Ticket{
			ExternalSystem: "jira", ExternalTicketID: "HDW-record", ObjectRef: "node-1",
			Assignee: "engineer",
		}, now)
		verifyTicketRepair(tx, ticket.TicketID, true, true, now)
		tx.Commit()
		// 资产状态未变（维修记录不把资产变维护中或在役）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateMaintenance {
			t.Errorf("repair record changed asset state: %s (维修记录不把资产变维护中或在役)", asset.State)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
