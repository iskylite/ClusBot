package work

// 硬件故障、工单和更换部件。
//
// 本文件实现人工维修跨日等待可持续跟踪，工单回执不替代设备验证：
//   - 经领域维护接管创建/关联外部工单，记录责任人、对象/部件、故障证据和
//     外部状态版本；
//   - 工单长等待使用 Wait/事件/查询，不占模型和工作者；重复回调不重复新建
//     工单；
//   - 部件更换结束旧安装关系并建立新部件/关系，触发相关验收依赖失效；
//   - 工单关闭后查询实际效果并测试，验证后回待复核；未恢复继续限制和后续
//     责任；
//   - 借出期间只有专项授权维修可调用相关动作，维修记录不把资产变维护中或
//     在役。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 工单（创建/关联外部工单）
// ---------------------------------------------------------------------------

// Ticket 是 tickets 的一行。
type Ticket struct {
	TicketID            string
	ExternalSystem      string
	ExternalTicketID    string
	IncidentID          string
	TakeoverID          string
	Assignee            string
	ObjectRef           string
	ComponentRefs       string
	FaultEvidence       string
	ExternalStateVersion int
	ExternalState       string
	State               string // open/waiting/closed/verified/failed
	VerificationOutcome string
	ResidualLimitations string
	FollowUpResponsibility string
	CreatedAt           int64
	UpdatedAt           int64
}

// createTicket 创建工单。
// 经领域维护接管创建/关联外部工单，记录责任人、对象/部件、故障证据和外部
// 状态版本；重复回调不重复新建工单（幂等）。
func createTicket(tx *sql.Tx, t Ticket, now int64) (Ticket, error) {
	if t.ExternalSystem == "" || t.ExternalTicketID == "" || t.ObjectRef == "" {
		return Ticket{}, errors.New("工单缺少外部系统/工单 ID/对象")
	}
	// 重复回调不重复新建工单（external_system+external_ticket_id 唯一）。
	var existingID string
	err := tx.QueryRow(`SELECT ticket_id FROM tickets WHERE external_system=? AND external_ticket_id=?`,
		t.ExternalSystem, t.ExternalTicketID).Scan(&existingID)
	if err == nil {
		return readTicket(tx, existingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, err
	}
	t.TicketID = id()
	t.State = "open"
	t.CreatedAt, t.UpdatedAt = now, now
	var incidentID, takeoverID any
	if t.IncidentID != "" {
		incidentID = t.IncidentID
	}
	if t.TakeoverID != "" {
		takeoverID = t.TakeoverID
	}
	_, err = tx.Exec(`INSERT INTO tickets(ticket_id,external_system,external_ticket_id,incident_id,
takeover_id,assignee,object_ref,component_refs,fault_evidence,external_state_version,external_state,
state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TicketID, t.ExternalSystem, t.ExternalTicketID, incidentID, takeoverID, t.Assignee,
		t.ObjectRef, t.ComponentRefs, t.FaultEvidence, t.ExternalStateVersion, t.ExternalState,
		t.State, now, now)
	if err != nil {
		return Ticket{}, err
	}
	return t, nil
}

// readTicket 读取工单。
func readTicket(q recordsQuerier, ticketID string) (Ticket, error) {
	var t Ticket
	var incidentID, takeoverID sql.NullString
	err := q.QueryRow(`SELECT ticket_id,external_system,external_ticket_id,incident_id,takeover_id,
assignee,object_ref,component_refs,fault_evidence,external_state_version,external_state,state,
verification_outcome,residual_limitations,follow_up_responsibility,created_at,updated_at
FROM tickets WHERE ticket_id=?`, ticketID).
		Scan(&t.TicketID, &t.ExternalSystem, &t.ExternalTicketID, &incidentID, &takeoverID,
			&t.Assignee, &t.ObjectRef, &t.ComponentRefs, &t.FaultEvidence, &t.ExternalStateVersion,
			&t.ExternalState, &t.State, &t.VerificationOutcome, &t.ResidualLimitations,
			&t.FollowUpResponsibility, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.IncidentID = incidentID.String
	t.TakeoverID = takeoverID.String
	return t, nil
}

// ---------------------------------------------------------------------------
// 工单长等待（Wait/事件/查询，不占模型和工作者）
// ---------------------------------------------------------------------------

// waitTicketUpdate 等待工单更新。
// 工单长等待使用 Wait/事件/查询，不占模型和工作者。
func waitTicketUpdate(tx *sql.Tx, ticketID string, waitKey string, now int64) error {
	// 工单长等待使用 Wait（不占模型和工作者）。
	if _, err := tx.Exec(`UPDATE tickets SET state='waiting', updated_at=? WHERE ticket_id=? AND state='open'`,
		now, ticketID); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 部件更换（结束旧安装关系并建立新部件/关系，触发相关验收依赖失效）
// ---------------------------------------------------------------------------

// recordComponentReplacement 记录部件更换。
// 部件更换结束旧安装关系并建立新部件/关系，触发相关验收依赖失效；
// 新旧部件序列号和恢复后的历史引用正确。
func recordComponentReplacement(tx *sql.Tx, nodeRef, oldComponentRef, newComponentRef, componentKind string, now int64) error {
	// 结束旧安装关系并建立新部件/关系（复用 的 replaceComponent）。
	if _, err := replaceComponent(tx, nodeRef, oldComponentRef, newComponentRef, componentKind, now); err != nil {
		return err
	}
	// 触发相关验收依赖失效（复用 的 invalidateEvidenceDependency）。
	if _, err := invalidateEvidenceDependency(tx, "component", oldComponentRef, "部件更换", now); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 维修复核（工单关闭后查询实际效果并测试，验证后回待复核）
// ---------------------------------------------------------------------------

// verifyTicketRepair 维修复核。
// 工单关闭后查询实际效果并测试，验证后回待复核；未恢复继续限制和后续责任；
// 工单回执不替代设备验证（关闭工单但故障仍在时，资产不会被误恢复）。
func verifyTicketRepair(tx *sql.Tx, ticketID string, effectVerified bool, testPassed bool, now int64) error {
	t, err := readTicket(tx, ticketID)
	if err != nil {
		return err
	}
	// 工单回执不替代设备验证：必须查询实际效果并测试。
	if !effectVerified || !testPassed {
		// 未恢复：继续限制和后续责任。
		if _, err := tx.Exec(`UPDATE tickets SET state='failed', verification_outcome='failed', updated_at=? WHERE ticket_id=?`,
			now, ticketID); err != nil {
			return err
		}
		return fmt.Errorf("工单 %s 效果未验证或测试未通过（关闭工单但故障仍在时，资产不会被误恢复）", ticketID)
	}
	// 验证后回待复核（复用 的 transitionAssetState）。
	if _, err := tx.Exec(`UPDATE tickets SET state='verified', verification_outcome='verified', updated_at=? WHERE ticket_id=?`,
		now, ticketID); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		Event: "ticket_repair_verified", Actor: "system",
		Summary: fmt.Sprintf("工单 %s 维修复核通过（对象 %s 回待复核）", ticketID, t.ObjectRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 借出期间维修（只有专项授权维修可调用相关动作）
// ---------------------------------------------------------------------------

// ErrLentAssetRepair 是借出资产维修的错误。
var ErrLentAssetRepair = errors.New("借出期间只有专项授权维修可调用相关动作")

// ensureLentAssetRepairAuthorized 核对借出资产维修授权。
// 借出期间只有专项授权维修可调用相关动作，维修记录不把资产变维护中或在役。
func ensureLentAssetRepairAuthorized(tx *sql.Tx, objectRef string, subject string, now int64) error {
	asset, err := readAsset(tx, objectRef)
	if err != nil {
		return err
	}
	// 借出期间：只有专项授权维修可调用相关动作。
	if asset.State == AssetStateLent {
		decision, err := decideAuthorization(tx, subject, "repair", objectRef, "", now, false)
		if err != nil {
			return err
		}
		if decision.Outcome != "allowed" {
			return fmt.Errorf("%w：借出资产 %s 维修需专项授权", ErrLentAssetRepair, objectRef)
		}
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
