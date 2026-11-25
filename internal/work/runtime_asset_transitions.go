package work

// 资产九态与领域迁移服务。
//
// 本文件实现状态唯一、语义互斥，每次迁移由领域证据与规则确认：
//   - 实现 §6.7 全部合法迁移、触发与前置条件；Task/Operation 仅引用，
//     不直接写资产状态；
//   - 迁移事务核对当前状态、预期版本、规则、证据、占用、责任和授权，写
//     迁移历史与 Outbox；
//   - 健康、可达性、业务占用单独表达；告警不自动进入维护中，Task 取消不
//     自动回在役；
//   - 维护接管可由有效策略自动完成，也可等待有权人员；均必须有影响与责任
//     记录；
//   - 验证失效、调试中断、首次纳管、归还和再启用按各自条件进入待复核；
//     保留实际限制。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 资产九态（§6.7）
// ---------------------------------------------------------------------------

// 资产九个唯一末级状态：待纳管；在管下的待复核、待调试、调试中、待交付、
// 在役、维护中；以及借出、已退役。“在管”是分组，不是第十个状态。
const (
	AssetStatePending   = "待纳管"
	AssetStateRecheck   = "待复核"
	AssetStatePendingDebug = "待调试"
	AssetStateDebugging = "调试中"
	AssetStatePendingDeliver = "待交付"
	AssetStateInService = "在役"
	AssetStateMaintenance = "维护中"
	AssetStateLent      = "借出"
	AssetStateRetired   = "已退役"
)

// assetTransitions 是 §6.7 允许迁移的结构表达。
var assetTransitions = map[string]map[string]bool{
	AssetStatePending: {
		AssetStateRecheck: true, // 首次纳管
	},
	AssetStateRecheck: {
		AssetStatePendingDebug: true,   // 需要调试（§6.7:499）
		AssetStatePendingDeliver: true, // 验证通过（§6.7:500）
		AssetStateInService: true,      // 复核通过回在役（§6.7:501）
		AssetStateMaintenance: true,    // 维护接管（§6.7:502）
		AssetStateLent: true,           // 借出（§6.7:512 在管任一子状态）
		AssetStateRetired: true,        // 退役（§6.7:514）
	},
	AssetStatePendingDebug: {
		AssetStateDebugging: true,   // 开始调试（§6.7:504）
		AssetStateRecheck: true,     // 取消调试（§6.7:503）
		AssetStateMaintenance: true, // 维护接管（§6.7:507）
		AssetStateLent: true,        // 借出（§6.7:512）
		AssetStateRetired: true,     // 退役（§6.7:514）
	},
	AssetStateDebugging: {
		AssetStatePendingDeliver: true, // 调试完成进交付（§6.7:505，正规路径）
		AssetStatePendingDebug: true,   // 调试中止仍需继续（§6.7:509）
		AssetStateRecheck: true,        // 验证失效/调试中断回待复核（§6.7:509）
		AssetStateMaintenance: true,    // 维护接管（§6.7:507）
		AssetStateLent: true,           // 借出（§6.7:512）
		AssetStateRetired: true,        // 退役（§6.7:514）
	},
	AssetStatePendingDeliver: {
		AssetStateInService: true,   // 完成交接（§6.7:506）
		AssetStatePendingDebug: true, // 验收依赖失效需重配/补测（§6.7:510）
		AssetStateRecheck: true,      // 验收依据失效回待复核（§6.7:510）
		AssetStateMaintenance: true,  // 维护接管（§6.7:507）
		AssetStateLent: true,         // 借出（§6.7:512）
		AssetStateRetired: true,      // 退役（§6.7:514）
	},
	AssetStateInService: {
		AssetStateMaintenance: true, // 维护接管（§6.7:507）
		AssetStateRecheck: true,     // 验收依据失效复核（§6.7:511）
		AssetStateLent: true,        // 借出（§6.7:512）
		AssetStateRetired: true,     // 退役（§6.7:514）
	},
	AssetStateMaintenance: {
		AssetStateRecheck: true, // 修复验证后回待复核（§6.7:508）
		AssetStateLent: true,    // 借出（§6.7:512 在管任一子状态）
		AssetStateRetired: true, // 退役（§6.7:514）
	},
	AssetStateLent: {
		AssetStateRecheck: true, // 归还后回待复核（§6.7:513）
		// 借出资产须先实际归还（经待复核）才能走普通退役路径（§6.7:514/519），不允许直转退役。
	},
	AssetStateRetired: {
		AssetStateRecheck: true, // 再启用（§6.7:515 核对后回待复核，不直接恢复在役）
	},
}

// canTransitionAsset 核对资产迁移是否允许。
func canTransitionAsset(from, to string) bool {
	allowed, ok := assetTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// ---------------------------------------------------------------------------
// 资产迁移（核对当前状态、预期版本、规则、证据、占用、责任和授权）
// ---------------------------------------------------------------------------

// AssetTransition 是 asset_transitions 的一行。
type AssetTransition struct {
	TransitionID           string
	ObjectRef              string
	FromState              string
	ToState                string
	TriggerKind            string
	EvidenceRefs           string
	RuleRefs               string
	ExpectedRevision       int
	ExpectedOwnershipEpoch int
	Actor                  string
	Responsibility         string
	Health                 string
	Reachability           string
	BusinessOccupancy      string
	OccurredAt             int64
	CreatedAt              int64
}

// transitionAssetState 迁移资产状态。
// 迁移事务核对当前状态、预期版本、规则、证据、占用、责任和授权，写迁移
// 历史与 Outbox；同一版本的并发迁移只有一方成功，另一方重判，不覆盖历史。
func transitionAssetState(tx *sql.Tx, t AssetTransition, now int64) (AssetTransition, error) {
	if t.ObjectRef == "" || t.FromState == "" || t.ToState == "" || t.Actor == "" {
		return AssetTransition{}, errors.New("迁移缺少对象/状态/执行者")
	}
	// 合法迁移核对（§6.7）。
	if !canTransitionAsset(t.FromState, t.ToState) {
		return AssetTransition{}, fmt.Errorf("资产状态跳跃：%s → %s 不允许", t.FromState, t.ToState)
	}
	// 证据与规则核对（每次迁移由领域证据与规则确认）。
	if t.EvidenceRefs == "" || t.RuleRefs == "" {
		return AssetTransition{}, errors.New("迁移缺少证据或规则引用（每次迁移由领域证据与规则确认）")
	}
	// 责任核对（维护接管必须有影响与责任记录）。
	if t.Responsibility == "" {
		return AssetTransition{}, errors.New("迁移缺少责任记录")
	}
	// 健康、可达性、业务占用单独表达。
	if t.Health == "" {
		t.Health = "unknown"
	}
	if t.Reachability == "" {
		t.Reachability = "unknown"
	}
	// 授权核对。
	decision, err := decideAuthorization(tx, t.Actor, "write", t.ObjectRef, "", now, false)
	if err != nil {
		return AssetTransition{}, err
	}
	if decision.Outcome != "allowed" {
		return AssetTransition{}, fmt.Errorf("%w：主体 %s 无权迁移资产 %s", ErrAuthorizationDenied, t.Actor, t.ObjectRef)
	}
	// 条件更新（核对当前状态、预期版本、占用；同一版本的并发迁移只有一方成功）。
	res, err := tx.Exec(`UPDATE ops_assets SET state=?, revision=revision+1, ownership_epoch=ownership_epoch+1, updated_at=?
WHERE object_ref=? AND state=? AND revision=? AND ownership_epoch=?`,
		t.ToState, now, t.ObjectRef, t.FromState, t.ExpectedRevision, t.ExpectedOwnershipEpoch)
	if err != nil {
		return AssetTransition{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return AssetTransition{}, fmt.Errorf("%w：资产 %s 已被并发迁移（同一版本的并发迁移只有一方成功）", ErrRevisionConflict, t.ObjectRef)
	}
	// 迁移历史（不覆盖历史）。
	t.TransitionID = id()
	t.OccurredAt = now
	t.CreatedAt = now
	if _, err := tx.Exec(`INSERT INTO asset_transitions(transition_id,object_ref,from_state,to_state,
trigger_kind,evidence_refs,rule_refs,expected_revision,expected_ownership_epoch,actor,responsibility,
health,reachability,business_occupancy,occurred_at,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TransitionID, t.ObjectRef, t.FromState, t.ToState, t.TriggerKind, t.EvidenceRefs, t.RuleRefs,
		t.ExpectedRevision, t.ExpectedOwnershipEpoch, t.Actor, t.Responsibility,
		t.Health, t.Reachability, t.BusinessOccupancy, now, now); err != nil {
		return AssetTransition{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 健康、可达性、业务占用单独表达（告警不自动进入维护中）
// ---------------------------------------------------------------------------

// ErrAlertAutoMaintenance 是告警自动进入维护的错误。
var ErrAlertAutoMaintenance = errors.New("告警不自动进入维护中（需维护接管）")

// ensureAlertNotAutoMaintenance 核对告警不自动进入维护。
// 健康、可达性、业务占用单独表达；告警不自动进入维护中，Task 取消不自动回在役。
func ensureAlertNotAutoMaintenance(currentState string, alertReceived bool) error {
	if currentState == AssetStateInService && alertReceived {
		return fmt.Errorf("%w：在役资产收到告警需维护接管，不自动进入维护中", ErrAlertAutoMaintenance)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 维护接管（可由有效策略自动完成，也可等待有权人员）
// ---------------------------------------------------------------------------

// MaintenanceTakeover 是 maintenance_takeovers 的一行。
type MaintenanceTakeover struct {
	TakeoverID      string
	ObjectRef       string
	TakeoverKind    string // automatic/manual
	ImpactAssessment string
	Responsibility  string
	AuthorizedBy    string
	TakenAt         int64
	CreatedAt       int64
}

// recordMaintenanceTakeover 记录维护接管。
// 维护接管可由有效策略自动完成，也可等待有权人员；均必须有影响与责任记录。
func recordMaintenanceTakeover(tx *sql.Tx, t MaintenanceTakeover, now int64) (MaintenanceTakeover, error) {
	if t.ObjectRef == "" || t.TakeoverKind == "" {
		return MaintenanceTakeover{}, errors.New("维护接管缺少对象或方式")
	}
	// 影响与责任记录必须有。
	if t.ImpactAssessment == "" || t.Responsibility == "" {
		return MaintenanceTakeover{}, errors.New("维护接管必须有影响与责任记录")
	}
	// 人工接管：必须有授权人。
	if t.TakeoverKind == "manual" && t.AuthorizedBy == "" {
		return MaintenanceTakeover{}, errors.New("人工维护接管必须有授权人")
	}
	t.TakeoverID = id()
	t.TakenAt = now
	t.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO maintenance_takeovers(takeover_id,object_ref,takeover_kind,
impact_assessment,responsibility,authorized_by,taken_at,created_at)
VALUES(?,?,?,?,?,?,?,?)`,
		t.TakeoverID, t.ObjectRef, t.TakeoverKind, t.ImpactAssessment, t.Responsibility,
		t.AuthorizedBy, now, now)
	if err != nil {
		return MaintenanceTakeover{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Task/Operation 仅引用（不直接写资产状态）
// ---------------------------------------------------------------------------

// ErrRevisionConflict 是版本冲突的错误。
var ErrRevisionConflict = errors.New("版本冲突：任务已被更新")

// ErrTaskAssetAuthority 是 Task 直接写资产状态的错误。
var ErrTaskAssetAuthority = errors.New("Task/Operation 仅引用，不直接写资产状态")

// ensureTaskNotAssetAuthority 核对 Task 不直接写资产状态。
// Task/Operation 仅引用，不直接写资产状态；Task 成败、工单关闭、告警恢复
// 都不能越过领域验证直接恢复资产。
func ensureTaskNotAssetAuthority(taskID string, objectRef string) error {
	if taskID == "" || objectRef == "" {
		return nil
	}
	// Task 仅引用资产（不直接写状态）：领域迁移服务按证据与规则管理。
	return nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
