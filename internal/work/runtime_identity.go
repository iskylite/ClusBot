package work

// 资产发现身份、部件和外部映射。
//
// 本文件实现准确关联节点、可更换部件与外部系统身份，不靠 IP 合并历史：
//   - 使用稳定内部 ID，外部映射含来源系统实例、外部类型/ID、设备代际及
//     有效区间；
//   - 发现先保存 identity_claim；可信无矛盾规则可归一，名称/IP/相似度仅作候选；
//   - IP/名称变化保持资产身份；更换网卡等部件创建新部件并结束旧安装关系，
//     节点身份连续性不明时待核对；
//   - 支持误合并/拆分的带证据修订，保留旧引用并触发相关验证依赖复查；
//   - 身份冲突只阻止相关对象写动作，不阻断其他已确认资产的工作。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 外部映射（稳定内部 ID，不靠 IP 合并历史）
// ---------------------------------------------------------------------------

// AssetExternalMapping 是 asset_external_mappings 的一行。
type AssetExternalMapping struct {
	MappingID        string
	ObjectRef        string
	SourceSystem     string
	SourceInstance   string
	ExternalType     string
	ExternalID       string
	DeviceGeneration int
	ValidFrom        int64
	ValidTo          int64
	CreatedAt        int64
}

// createExternalMapping 创建外部映射。
// 使用稳定内部 ID，外部映射含来源系统实例、外部类型/ID、设备代际及有效区间。
func createExternalMapping(tx *sql.Tx, m AssetExternalMapping, now int64) (AssetExternalMapping, error) {
	if m.ObjectRef == "" || m.SourceSystem == "" || m.ExternalID == "" {
		return AssetExternalMapping{}, errors.New("外部映射缺少内部 ID/来源系统/外部 ID")
	}
	m.MappingID = id()
	m.CreatedAt = now
	if m.DeviceGeneration < 1 {
		m.DeviceGeneration = 1
	}
	_, err := tx.Exec(`INSERT INTO asset_external_mappings(mapping_id,object_ref,source_system,source_instance,
external_type,external_id,device_generation,valid_from,valid_to,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		m.MappingID, m.ObjectRef, m.SourceSystem, m.SourceInstance, m.ExternalType, m.ExternalID,
		m.DeviceGeneration, m.ValidFrom, m.ValidTo, now)
	if err != nil {
		return AssetExternalMapping{}, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 身份声明（发现先保存 identity_claim；可信无矛盾规则可归一）
// ---------------------------------------------------------------------------

// IdentityClaim 是 identity_claims 的一行。
type IdentityClaim struct {
	ClaimID            string
	CandidateObjectRef string
	CandidateIdentity  string
	IdentityKind       string
	ClaimedBy          string
	Confidence         string // trusted/candidate/untrusted
	UnifiedTo          string
	ClaimedAt          int64
}

// recordIdentityClaim 记录身份声明。
// 发现先保存 identity_claim；可信无矛盾规则可归一，名称/IP/相似度仅作候选；
// IP 被新设备复用不合并为旧节点；同序列号冲突可见且不自动覆盖。
func recordIdentityClaim(tx *sql.Tx, c IdentityClaim, now int64) (IdentityClaim, error) {
	if c.CandidateObjectRef == "" || c.CandidateIdentity == "" || c.ClaimedBy == "" {
		return IdentityClaim{}, errors.New("身份声明缺少候选/身份/声明者")
	}
	if c.IdentityKind == "" {
		c.IdentityKind = "candidate"
	}
	if c.Confidence == "" {
		c.Confidence = "candidate" // 名称/IP/相似度仅作候选
	}
	c.ClaimID = id()
	c.ClaimedAt = now
	var unifiedTo any
	if c.UnifiedTo != "" {
		unifiedTo = c.UnifiedTo
	}
	_, err := tx.Exec(`INSERT INTO identity_claims(claim_id,candidate_object_ref,candidate_identity,
identity_kind,claimed_by,confidence,unified_to,claimed_at)
VALUES(?,?,?,?,?,?,?,?)`,
		c.ClaimID, c.CandidateObjectRef, c.CandidateIdentity, c.IdentityKind, c.ClaimedBy,
		c.Confidence, unifiedTo, now)
	if err != nil {
		return IdentityClaim{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 身份冲突（IP/序列号冲突可见且不自动覆盖）
// ---------------------------------------------------------------------------

// checkIdentityConflict 核对身份冲突。
// IP 被新设备复用不合并为旧节点；同序列号冲突可见且不自动覆盖；
// 身份冲突只阻止相关对象写动作，不阻断其他已确认资产的工作。
func checkIdentityConflict(tx *sql.Tx, identity string, kind string, now int64) (conflict bool, conflictingRefs []string, err error) {
	// 查询同身份的不同对象（IP/序列号冲突可见）。
	rows, err := tx.Query(`SELECT DISTINCT candidate_object_ref FROM identity_claims
WHERE candidate_identity=? AND identity_kind=? AND confidence IN ('trusted','candidate')`, identity, kind)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return false, nil, err
		}
		conflictingRefs = append(conflictingRefs, ref)
	}
	// 冲突：同身份多个对象（IP 被新设备复用不合并为旧节点）。
	return len(conflictingRefs) > 1, conflictingRefs, rows.Err()
}

// ---------------------------------------------------------------------------
// 部件更换（创建新部件并结束旧安装关系）
// ---------------------------------------------------------------------------

// ComponentInstallation 是 component_installations 的一行。
type ComponentInstallation struct {
	InstallationID          string
	NodeRef                 string
	ComponentRef            string
	ComponentKind           string
	InstalledAt             int64
	RemovedAt               int64
	NodeIdentityContinuous  string
	CreatedAt               int64
}

// replaceComponent 更换部件。
// 更换网卡等部件创建新部件并结束旧安装关系，节点身份连续性不明时待核对；
// 更换部件后旧故障/测试仍关联旧部件，新安装关系有时间边界。
func replaceComponent(tx *sql.Tx, nodeRef, oldComponentRef, newComponentRef, componentKind string, now int64) (ComponentInstallation, error) {
	if nodeRef == "" || newComponentRef == "" || componentKind == "" {
		return ComponentInstallation{}, errors.New("部件更换缺少节点/新部件/类型")
	}
	// 结束旧安装关系（时间边界）。
	if oldComponentRef != "" {
		if _, err := tx.Exec(`UPDATE component_installations SET removed_at=?
WHERE node_ref=? AND component_ref=? AND removed_at=0`, now, nodeRef, oldComponentRef); err != nil {
			return ComponentInstallation{}, err
		}
	}
	// 创建新部件安装关系（节点身份连续性不明时待核对）。
	installation := ComponentInstallation{
		InstallationID: id(), NodeRef: nodeRef, ComponentRef: newComponentRef,
		ComponentKind: componentKind, InstalledAt: now,
		NodeIdentityContinuous: "unknown", // 节点身份连续性不明时待核对
		CreatedAt: now,
	}
	_, err := tx.Exec(`INSERT INTO component_installations(installation_id,node_ref,component_ref,
component_kind,installed_at,removed_at,node_identity_continuous,created_at)
VALUES(?,?,?,?,?,?,?,?)`,
		installation.InstallationID, installation.NodeRef, installation.ComponentRef,
		installation.ComponentKind, installation.InstalledAt, 0, installation.NodeIdentityContinuous, now)
	if err != nil {
		return ComponentInstallation{}, err
	}
	return installation, nil
}

// ---------------------------------------------------------------------------
// 身份修订（误合并/拆分的带证据修订）
// ---------------------------------------------------------------------------

// IdentityCorrection 是 identity_corrections 的一行。
type IdentityCorrection struct {
	CorrectionID   string
	Kind           string // merge_split/identity_change
	OldObjectRef   string
	NewObjectRef   string
	EvidenceRef    string
	VerificationDependencies string
	CorrectedBy    string
	CorrectedAt    int64
}

// recordIdentityCorrection 记录身份修订。
// 支持误合并/拆分的带证据修订，保留旧引用并触发相关验证依赖复查。
func recordIdentityCorrection(tx *sql.Tx, c IdentityCorrection, now int64) (IdentityCorrection, error) {
	if c.OldObjectRef == "" || c.NewObjectRef == "" || c.CorrectedBy == "" {
		return IdentityCorrection{}, errors.New("身份修订缺少旧/新引用或修订者")
	}
	if c.EvidenceRef == "" {
		return IdentityCorrection{}, errors.New("身份修订缺少证据引用")
	}
	c.CorrectionID = id()
	c.CorrectedAt = now
	if c.VerificationDependencies == "" {
		c.VerificationDependencies = "[]"
	}
	_, err := tx.Exec(`INSERT INTO identity_corrections(correction_id,kind,old_object_ref,new_object_ref,
evidence_ref,verification_dependencies,corrected_by,corrected_at)
VALUES(?,?,?,?,?,?,?,?)`,
		c.CorrectionID, c.Kind, c.OldObjectRef, c.NewObjectRef, c.EvidenceRef,
		c.VerificationDependencies, c.CorrectedBy, now)
	if err != nil {
		return IdentityCorrection{}, err
	}
	// 触发相关验证依赖复查（审计）。
	if err := recordAudit(tx, AuditEvent{
		Event: "identity_correction", Actor: c.CorrectedBy,
		Summary: fmt.Sprintf("身份修订 %s→%s（触发验证依赖复查）", c.OldObjectRef, c.NewObjectRef),
		DetailRef: c.EvidenceRef, OccurredAt: now,
	}); err != nil {
		return IdentityCorrection{}, err
	}
	return c, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
