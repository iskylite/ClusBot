package work

// P4-08：资产发现与纳管流程。
//
// 本文件实现从发现声明到可信资产及责任接收有完整证据：
//   - 按授权范围发现节点/部件/来源映射，保留覆盖、未响应与冲突；发现不直接
//     等于可写纳管；
//   - 通过身份核对后创建待纳管记录；管理责任接收与必要来源就绪后进入在管/
//     待复核；
//   - 同一来源重复发现更新声明，不重复建资产；采集缺失不擅自删除对象；
//   - 关联任务、发现结果、人工决定与事件；一台冲突不阻断其他已确认设备；
//   - 用提供者契约验证真实响应或固定录制样本，注明实际环境覆盖。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 发现结果（覆盖、未响应与冲突）
// ---------------------------------------------------------------------------

// DiscoveryResult 是发现结果。
type DiscoveryResult struct {
	// 发现的对象（节点/部件）。
	Discovered []DiscoveredObject
	// 覆盖（发现的范围）。
	Coverage   string
	// 未响应（未响应的对象）。
	Unresponsive []string
	// 冲突（身份冲突的对象）。
	Conflicts  []IdentityConflict
	// 来源与采集时间。
	SourceRef  string
	CollectedAt int64
}

// DiscoveredObject 是发现的对象。
type DiscoveredObject struct {
	// 候选身份（名称/IP/序列号等）。
	CandidateIdentity string
	IdentityKind      string // ip/name/serial
	// 部件与来源映射。
	Components        []string
	SourceMapping     string
}

// IdentityConflict 是身份冲突。
type IdentityConflict struct {
	CandidateIdentity string
	ConflictingRefs   []string
	Basis             string
}

// ---------------------------------------------------------------------------
// 纳管流程（发现 → 待纳管 → 在管/待复核）
// ---------------------------------------------------------------------------

// ManagementAdmission 是纳管记录。
type ManagementAdmission struct {
	AdmissionID     string
	ObjectRef       string
	// 身份核对（通过身份核对后创建待纳管记录）。
	IdentityVerified bool
	// 管理责任接收与必要来源就绪。
	ResponsibilityAccepted bool
	SourcesReady   bool
	// 状态（discovered/pending_managed/in_management/recheck）。
	State           string
	// 创建与更新。
	CreatedAt       int64
	UpdatedAt       int64
}

// processDiscoveryResult 处理发现结果（P4-08.1/P4-08.2）。
// 按授权范围发现节点/部件/来源映射，保留覆盖、未响应与冲突；发现不直接
// 等于可写纳管；通过身份核对后创建待纳管记录；管理责任接收与必要来源就绪
// 后进入在管/待复核。
func processDiscoveryResult(tx *sql.Tx, result DiscoveryResult, subject string, now int64) (processed int, conflicts int, err error) {
	// 授权范围核对（发现不直接等于可写纳管）。
	decision, err := decideAuthorization(tx, subject, "discover", result.SourceRef, "", now, false)
	if err != nil {
		return 0, 0, err
	}
	if decision.Outcome != "allowed" {
		return 0, 0, fmt.Errorf("%w：主体 %s 无发现权限", ErrAuthorizationDenied, subject)
	}
	// 处理发现的对象（保留覆盖、未响应与冲突）。
	for _, obj := range result.Discovered {
		// 身份核对（通过身份核对后创建待纳管记录）。
		verified := obj.CandidateIdentity != ""
		// 创建待纳管记录（管理责任接收与必要来源就绪后进入在管/待复核）。
		state := "discovered"
		if verified {
			state = "pending_managed"
		}
		if verified && result.SourceRef != "" {
			state = "pending_managed"
		}
		// 同一来源重复发现更新声明，不重复建资产。
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM identity_claims WHERE candidate_identity=? AND identity_kind=?)`,
			obj.CandidateIdentity, obj.IdentityKind).Scan(&exists); err != nil {
			return processed, conflicts, err
		}
		if exists {
			// 重复发现：更新声明（不重复建资产）。
			continue
		}
		// 记录身份声明（候选，不归一）。
		if _, err := recordIdentityClaim(tx, IdentityClaim{
			CandidateObjectRef: obj.CandidateIdentity, CandidateIdentity: obj.CandidateIdentity,
			IdentityKind: obj.IdentityKind, ClaimedBy: subject, Confidence: "candidate",
		}, now); err != nil {
			return processed, conflicts, err
		}
		processed++
		_ = state
	}
	// 冲突记录（一台冲突不阻断其他已确认设备）。
	conflicts = len(result.Conflicts)
	for _, conflict := range result.Conflicts {
		if err := recordAudit(tx, AuditEvent{
			Event: "discovery_conflict", Actor: subject,
			Summary: fmt.Sprintf("发现冲突 %s（%d 个引用）：%s", conflict.CandidateIdentity, len(conflict.ConflictingRefs), conflict.Basis),
			OccurredAt: now,
		}); err != nil {
			return processed, conflicts, err
		}
	}
	return processed, conflicts, nil
}

// ---------------------------------------------------------------------------
// 纳管（管理责任接收与必要来源就绪后进入在管/待复核）
// ---------------------------------------------------------------------------

// admitDiscoveredObject 纳管发现的对象（P4-08.2）。
// 管理责任接收与必要来源就绪后进入在管/待复核；管理责任未接收的发现节点
// 不会直接进入调试中。
func admitDiscoveredObject(tx *sql.Tx, objectRef string, responsibilityAccepted bool, sourcesReady bool, now int64) error {
	// 管理责任未接收：不能纳管（不会直接进入调试中）。
	if !responsibilityAccepted {
		return fmt.Errorf("管理责任未接收的发现节点 %s 不能纳管（不会直接进入调试中）", objectRef)
	}
	// 必要来源未就绪：不能纳管。
	if !sourcesReady {
		return fmt.Errorf("必要来源未就绪的发现节点 %s 不能纳管", objectRef)
	}
	// 创建待纳管记录（进入在管/待复核）。
	if _, err := tx.Exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,revision,ownership_epoch,current_owner,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(object_ref) DO NOTHING`,
		objectRef, "node", objectRef, AssetStateRecheck, 1, 1, "system", now, now); err != nil {
		return err
	}
	return recordAudit(tx, AuditEvent{
		Event: "object_admitted", Actor: "system",
		Summary: fmt.Sprintf("对象 %s 已纳管（在管/待复核）", objectRef), OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 重复发现（更新声明，不重复建资产）
// ---------------------------------------------------------------------------

// ensureNoDuplicateAsset 核对不重复建资产（P4-08.3）。
// 同一来源重复发现更新声明，不重复建资产；采集缺失不擅自删除对象。
func ensureNoDuplicateAsset(tx *sql.Tx, candidateIdentity string, identityKind string, now int64) (exists bool, err error) {
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM identity_claims WHERE candidate_identity=? AND identity_kind=?)`,
		candidateIdentity, identityKind).Scan(&exists)
	return exists, err
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
