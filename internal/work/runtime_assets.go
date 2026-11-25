package work

// 最小资产身份、版本与执行准入。
//
// 本文件实现任何集群写操作都能回答目标是谁、当前谁管理、是否允许执行：
//   - 资产保存稳定 ObjectRef、唯一当前末级状态、revision、ownership_epoch、
//     冻结和必要占用；未知身份不能写；
//   - 授权与领域准入分离的接口，返回允许/拒绝/待核对及来源版本；不能只用
//     字符串 scope_label；
//   - 最终派发和冻结共用同一资产权威记录与写事务顺序；批量对象逐一核对
//     版本/代际，不能用单个版本代表全部；
//   - 冻结先提交阻止新冲突写；已派发保留在途；此阶段只做协议级冻结测试；
//   - 无外部 fencing 时保留迟到外部效果可能发生的边界，SQL 版本不能冒充
//     设备侧撤销。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 资产身份（稳定 ObjectRef、唯一当前末级状态、revision、ownership_epoch）
// ---------------------------------------------------------------------------

// OpsAsset 是 ops_assets 的一行。
type OpsAsset struct {
	ObjectRef     string
	Kind          string
	Identity      string
	State         string // 当前末级状态
	Revision      int
	OwnershipEpoch int
	FrozenAt      int64
	FrozenBy      string
	FreezeReason  string
	CurrentOwner  string
	CreatedAt     int64
	UpdatedAt     int64
}

// upsertAsset 登记/更新一个资产。
// 资产保存稳定 ObjectRef、唯一当前末级状态、revision、ownership_epoch、冻结
// 和必要占用；未知身份不能写。
func upsertAsset(tx *sql.Tx, a OpsAsset, now int64) (OpsAsset, error) {
	if a.ObjectRef == "" || a.Identity == "" {
		return OpsAsset{}, errors.New("资产缺少稳定 ObjectRef 或唯一身份")
	}
	if a.Kind == "" || a.State == "" {
		return OpsAsset{}, errors.New("资产缺少种类或状态")
	}
	if a.Revision < 1 {
		a.Revision = 1
	}
	if a.OwnershipEpoch < 1 {
		a.OwnershipEpoch = 1
	}
	a.CreatedAt, a.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,revision,ownership_epoch,
current_owner,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(object_ref) DO UPDATE SET kind=excluded.kind,identity=excluded.identity,state=excluded.state,
revision=excluded.revision,ownership_epoch=excluded.ownership_epoch,current_owner=excluded.current_owner,updated_at=excluded.updated_at`,
		a.ObjectRef, a.Kind, a.Identity, a.State, a.Revision, a.OwnershipEpoch, a.CurrentOwner, now, now)
	if err != nil {
		return OpsAsset{}, err
	}
	// 状态历史（状态迁移留证）。
	if _, err := tx.Exec(`INSERT INTO asset_state_history(object_ref,state,revision,changed_by,reason,occurred_at)
VALUES(?,?,?,?,?,?)`, a.ObjectRef, a.State, a.Revision, a.CurrentOwner, "登记/更新", now); err != nil {
		return OpsAsset{}, err
	}
	return a, nil
}

// readAsset 读取一个资产（唯一当前末级身份）。
func readAsset(q recordsQuerier, objectRef string) (OpsAsset, error) {
	var a OpsAsset
	err := q.QueryRow(`SELECT object_ref,kind,identity,state,revision,ownership_epoch,frozen_at,frozen_by,
freeze_reason,current_owner,created_at,updated_at FROM ops_assets WHERE object_ref=?`, objectRef).
		Scan(&a.ObjectRef, &a.Kind, &a.Identity, &a.State, &a.Revision, &a.OwnershipEpoch,
			&a.FrozenAt, &a.FrozenBy, &a.FreezeReason, &a.CurrentOwner, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// 领域准入（授权与领域准入分离）
// ---------------------------------------------------------------------------

// AssetAdmissionResult 是领域准入结果。
type AssetAdmissionResult struct {
	Allowed         bool
	Outcome         string // allowed/denied/reconcile
	Reason          string
	AssetRevision   int
	OwnershipEpoch  int
}

// checkAssetAdmission 核对资产领域准入。
// 授权与领域准入分离：授权回答"谁可以做"，领域准入回答"当前资产状态和证据
// 下能不能做"；不能只用字符串 scope_label；未知身份不能写。
func checkAssetAdmission(tx *sql.Tx, subject, action, objectRef string, now int64) (AssetAdmissionResult, error) {
	// 1. 授权核对（谁可以做）。
	authDecision, err := decideAuthorization(tx, subject, action, objectRef, "", now, false)
	if err != nil {
		return AssetAdmissionResult{}, err
	}
	if authDecision.Outcome != "allowed" {
		return AssetAdmissionResult{Outcome: "denied", Reason: "授权拒绝：" + authDecision.Reason}, nil
	}
	// 2. 领域准入（当前资产状态和证据下能不能做）。
	asset, err := readAsset(tx, objectRef)
	if errors.Is(err, sql.ErrNoRows) {
		// 未知身份不能写。
		return AssetAdmissionResult{Outcome: "denied", Reason: "未知资产身份，不能写"}, nil
	}
	if err != nil {
		return AssetAdmissionResult{}, err
	}
	// 冻结：阻止新冲突写。
	if asset.FrozenAt > 0 {
		return AssetAdmissionResult{
			Outcome: "denied", Reason: fmt.Sprintf("资产已冻结（%s），阻止新冲突写", asset.FreezeReason),
			AssetRevision: asset.Revision, OwnershipEpoch: asset.OwnershipEpoch,
		}, nil
	}
	// 借出/退役/身份冲突：默认不允许普通运维写。
	switch asset.State {
	case "借出", AssetStateRetired, "身份冲突":
		return AssetAdmissionResult{
			Outcome: "denied", Reason: fmt.Sprintf("资产状态 %s 不允许普通运维写", asset.State),
			AssetRevision: asset.Revision, OwnershipEpoch: asset.OwnershipEpoch,
		}, nil
	}
	// 待核对：状态不明或效果未知。
	if asset.State == "" || asset.State == "待核对" {
		return AssetAdmissionResult{
			Outcome: "reconcile", Reason: "资产状态待核对",
			AssetRevision: asset.Revision, OwnershipEpoch: asset.OwnershipEpoch,
		}, nil
	}
	// 允许（在管/就绪等正常状态）。
	return AssetAdmissionResult{
		Allowed: true, Outcome: "allowed", Reason: "授权与领域准入均通过",
		AssetRevision: asset.Revision, OwnershipEpoch: asset.OwnershipEpoch,
	}, nil
}

// ---------------------------------------------------------------------------
// 冻结（冻结先提交阻止新冲突写；已派发保留在途）
// ---------------------------------------------------------------------------

// freezeAsset 冻结一个资产。
// 冻结先提交阻止新冲突写；已派发保留在途；此阶段只做协议级冻结测试，不
// 假装完整借还业务已实现。
func freezeAsset(tx *sql.Tx, objectRef, frozenBy, reason string, now int64) error {
	res, err := tx.Exec(`UPDATE ops_assets SET frozen_at=?, frozen_by=?, freeze_reason=?, revision=revision+1, updated_at=?
WHERE object_ref=? AND frozen_at=0`, now, frozenBy, reason, now, objectRef)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("资产 %s 不存在或已冻结", objectRef)
	}
	if err := recordAudit(tx, AuditEvent{
		Event: "asset_frozen", Actor: frozenBy, Summary: fmt.Sprintf("冻结资产 %s", objectRef), OccurredAt: now,
	}); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 批量对象逐一核对版本/代际
// ---------------------------------------------------------------------------

// checkBatchVersions 批量核对对象版本/代际。
// 批量对象逐一核对版本/代际，不能用单个版本代表全部。
func checkBatchVersions(tx *sql.Tx, objects []string, expectedRevisions map[string]int, now int64) (allMatch bool, mismatches []string, err error) {
	for _, obj := range objects {
		asset, err := readAsset(tx, obj)
		if errors.Is(err, sql.ErrNoRows) {
			mismatches = append(mismatches, fmt.Sprintf("%s:未知身份", obj))
			continue
		}
		if err != nil {
			return false, nil, err
		}
		expected, ok := expectedRevisions[obj]
		if !ok || asset.Revision != expected {
			mismatches = append(mismatches, fmt.Sprintf("%s:revision=%d≠%d", obj, asset.Revision, expected))
		}
	}
	return len(mismatches) == 0, mismatches, nil
}

// ---------------------------------------------------------------------------
// 无外部 fencing 的边界（SQL 版本不能冒充设备侧撤销）
// ---------------------------------------------------------------------------

// ErrNoExternalFence 是无外部 fencing 的错误。
var ErrNoExternalFence = errors.New("无外部 fencing：迟到外部效果可能发生，SQL 版本不能冒充设备侧撤销")

// recordExternalEffectUncertainty 记录无外部 fencing 的迟到效果边界。
func recordExternalEffectUncertainty(tx *sql.Tx, objectRef, effectRef string, now int64) error {
	return recordAudit(tx, AuditEvent{
		Event: "external_effect_uncertain", Actor: "system",
		Summary: fmt.Sprintf("资产 %s 的外部效果 %s 无 fencing，迟到效果可能发生", objectRef, effectRef),
		DetailRef: objectRef, OccurredAt: now,
	})
}

// 确保 context/sql/errors 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
)
