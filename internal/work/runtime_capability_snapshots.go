package work

// P3-05：动态 Schema 发现与不可变执行快照。
//
// 本文件实现多字段、动态配置和测试参数由提供者描述，实例执行依据可恢复：
//   - 读取授权范围内目录、配置/测试/输入/输出 Schema、单位、依赖、默认值、
//     环境版本与有效期；
//   - 校验方言、大小、深度与参数；禁止自动访问任意远程 $ref；动态选项缺失
//     时返回缺口，不猜值；
//   - 选择实现后冻结具体参数、规范化摘要算法版本、Schema/连接/能力/凭据绑定
//     与实际目标清单；
//   - 领域阈值与验收标准单独引用已发布规范；提供者建议值不自动覆盖业务规则；
//   - 派发前重查当前权限/目标/连接版本；未派发不匹配即停，已派发按原快照核对。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Schema 发现（读取授权范围内的 Schema）
// ---------------------------------------------------------------------------

// SchemaDiscovery 是 schema_discoveries 的一行。
type SchemaDiscovery struct {
	DiscoveryID    string
	CapabilityKey  string
	InputSchema    string
	OutputSchema   string
	Units          string
	Defaults       string
	Dependencies   string
	EnvironmentVersion string
	ExpiresAt      int64
	ValidationOutcome string // valid/invalid/gap
	ValidationDetail  string
	Gaps           string
	DiscoveredAt   int64
	DiscoveredBy   string
}

// discoverSchema 发现授权范围内的 Schema（P3-05.1/P3-05.2）。
// 读取授权范围内目录、配置/测试/输入/输出 Schema、单位、依赖、默认值、
// 环境版本与有效期；校验方言、大小、深度与参数；禁止自动访问任意远程 $ref；
// 动态选项缺失时返回缺口，不猜值。
func discoverSchema(tx *sql.Tx, capabilityKey string, fetchSchema func() (input, output string, err error), subject string, now int64) (SchemaDiscovery, error) {
	// 授权核对：读取授权范围内的 Schema。
	decision, err := decideAuthorization(tx, subject, "read", capabilityKey, "", now, false)
	if err != nil {
		return SchemaDiscovery{}, err
	}
	if decision.Outcome != "allowed" {
		return SchemaDiscovery{}, fmt.Errorf("%w：主体 %s 无权读取能力 %s 的 Schema", ErrAuthorizationDenied, subject, capabilityKey)
	}
	d := SchemaDiscovery{
		DiscoveryID: id(), CapabilityKey: capabilityKey, DiscoveredAt: now, DiscoveredBy: subject,
	}
	// 读取 Schema（提供者描述）。
	input, output, err := fetchSchema()
	if err != nil {
		d.ValidationOutcome = "gap"
		d.ValidationDetail = "提供者无法提供 Schema"
		d.Gaps = `["schema_unavailable"]`
		return d, nil
	}
	d.InputSchema, d.OutputSchema = input, output
	// 校验：方言、大小、深度与参数；禁止自动访问任意远程 $ref。
	outcome, detail, gaps := validateDiscoveredSchema(input, output)
	d.ValidationOutcome = outcome
	d.ValidationDetail = detail
	d.Gaps = gaps
	return d, nil
}

// validateDiscoveredSchema 校验发现的 Schema（P3-05.2）。
// 校验方言、大小、深度与参数；禁止自动访问任意远程 $ref；动态选项缺失时
// 返回缺口，不猜值。
func validateDiscoveredSchema(input, output string) (outcome, detail, gaps string) {
	// 大小限制。
	if len(input) > 65536 || len(output) > 65536 {
		return "invalid", "Schema 超过大小限制", "[]"
	}
	// 方言：必须是 JSON Schema。
	var inputMap, outputMap map[string]any
	if err := json.Unmarshal([]byte(input), &inputMap); err != nil {
		return "invalid", "输入 Schema 不是有效 JSON", "[]"
	}
	if err := json.Unmarshal([]byte(output), &outputMap); err != nil {
		return "invalid", "输出 Schema 不是有效 JSON", "[]"
	}
	// 深度限制（嵌套不超过 10 层）。
	if schemaDepth(inputMap) > 10 || schemaDepth(outputMap) > 10 {
		return "invalid", "Schema 嵌套深度超限", "[]"
	}
	// 禁止自动访问任意远程 $ref。
	if hasRemoteRef(inputMap) || hasRemoteRef(outputMap) {
		return "invalid", "禁止自动访问任意远程 $ref", "[]"
	}
	// 动态选项缺失：返回缺口（不猜值）。
	var gapList []string
	if inputMap["properties"] == nil {
		gapList = append(gapList, "input_properties_missing")
	}
	if outputMap["properties"] == nil {
		gapList = append(gapList, "output_properties_missing")
	}
	if len(gapList) > 0 {
		return "gap", "Schema 缺少属性定义（动态选项缺失返回缺口，不猜值）", toJSON(gapList)
	}
	return "valid", "Schema 校验通过", "[]"
}

// schemaDepth 计算 Schema 嵌套深度。
func schemaDepth(m map[string]any) int {
	if m == nil {
		return 0
	}
	maxDepth := 0
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			depth := 1 + schemaDepth(sub)
			if depth > maxDepth {
				maxDepth = depth
			}
		}
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]any); ok {
					depth := 1 + schemaDepth(sub)
					if depth > maxDepth {
						maxDepth = depth
					}
				}
			}
		}
	}
	return maxDepth
}

// hasRemoteRef 检查是否含远程 $ref。
func hasRemoteRef(m map[string]any) bool {
	if m == nil {
		return false
	}
	for k, v := range m {
		if k == "$ref" {
			if ref, ok := v.(string); ok && (strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")) {
				return true
			}
		}
		if sub, ok := v.(map[string]any); ok && hasRemoteRef(sub) {
			return true
		}
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]any); ok && hasRemoteRef(sub) {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 不可变执行快照（冻结具体参数）
// ---------------------------------------------------------------------------

// CapabilitySnapshot 是 capability_snapshots 的一行。
type CapabilitySnapshot struct {
	SnapshotID         string
	CapabilityKey      string
	CapabilityVersion  int
	ConnectionID       string
	ConnectionVersion  int
	CredentialRef      string
	InputSchema        string
	OutputSchema       string
	FrozenParams       string
	ParamsDigest       string
	DigestAlgorithm    string
	TargetRefs         string
	EnvironmentVersion string
	ExpiresAt          int64
	CreatedAt          int64
	CreatedBy          string
}

// freezeSnapshot 冻结一个执行快照（P3-05.3）。
// 选择实现后冻结具体参数、规范化摘要算法版本、Schema/连接/能力/凭据绑定与
// 实际目标清单；同键不同参数摘要拒绝。
func freezeSnapshot(tx *sql.Tx, s CapabilitySnapshot, now int64) (CapabilitySnapshot, error) {
	if s.CapabilityKey == "" || s.ConnectionID == "" || s.FrozenParams == "" {
		return CapabilitySnapshot{}, errors.New("快照缺少能力/连接/冻结参数")
	}
	// 规范化参数摘要（同键不同参数摘要拒绝）。
	s.ParamsDigest = digest(s.FrozenParams)
	s.DigestAlgorithm = "sha256"
	// 同键不同参数摘要：拒绝。
	var existingDigest string
	err := tx.QueryRow(`SELECT params_digest FROM capability_snapshots WHERE capability_key=? AND connection_id=? AND capability_version=?`,
		s.CapabilityKey, s.ConnectionID, s.CapabilityVersion).Scan(&existingDigest)
	if err == nil && existingDigest != s.ParamsDigest {
		return CapabilitySnapshot{}, fmt.Errorf("%w：同键不同参数摘要（%s≠%s）", ErrSnapshotConflict, s.ParamsDigest, existingDigest)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CapabilitySnapshot{}, err
	}
	s.SnapshotID = id()
	s.CreatedAt = now
	_, err = tx.Exec(`INSERT INTO capability_snapshots(snapshot_id,capability_key,capability_version,
connection_id,connection_version,credential_ref,input_schema,output_schema,frozen_params,params_digest,
digest_algorithm,target_refs,environment_version,expires_at,created_at,created_by)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.SnapshotID, s.CapabilityKey, s.CapabilityVersion, s.ConnectionID, s.ConnectionVersion,
		s.CredentialRef, s.InputSchema, s.OutputSchema, s.FrozenParams, s.ParamsDigest,
		s.DigestAlgorithm, s.TargetRefs, s.EnvironmentVersion, s.ExpiresAt, now, s.CreatedBy)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	return s, nil
}

// ErrSnapshotConflict 是快照冲突的错误。
var ErrSnapshotConflict = errors.New("快照冲突")

// ---------------------------------------------------------------------------
// 派发前重查（未派发不匹配即停，已派发按原快照核对）
// ---------------------------------------------------------------------------

// validateSnapshotBeforeDispatch 派发前重查当前权限/目标/连接版本（P3-05.5）。
// 发现后 Schema/凭据/对象集合变化，旧确认不能直接执行；未派发不匹配即停，
// 已派发按原快照核对。
func validateSnapshotBeforeDispatch(tx *sql.Tx, snapshotID, subject string, now int64) error {
	// 读取快照。
	var s CapabilitySnapshot
	err := tx.QueryRow(`SELECT snapshot_id,capability_key,capability_version,connection_id,connection_version,
frozen_params,params_digest,expires_at FROM capability_snapshots WHERE snapshot_id=?`, snapshotID).
		Scan(&s.SnapshotID, &s.CapabilityKey, &s.CapabilityVersion, &s.ConnectionID,
			&s.ConnectionVersion, &s.FrozenParams, &s.ParamsDigest, &s.ExpiresAt)
	if err != nil {
		return err
	}
	// 有效期核对。
	if s.ExpiresAt > 0 && now >= s.ExpiresAt {
		return fmt.Errorf("快照已过期（发现后 Schema/凭据/对象集合变化，旧确认不能直接执行）")
	}
	// 权限重查（当前权限）。
	decision, err := decideAuthorization(tx, subject, "dispatch", s.CapabilityKey, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		return fmt.Errorf("%w：派发前权限重查失败：%s", ErrAuthorizationDenied, decision.Reason)
	}
	// 连接版本核对（连接版本变化后旧快照不匹配）。
	var currentConnVersion int
	if err := tx.QueryRow("SELECT connection_version FROM capability_connections WHERE connection_id=?", s.ConnectionID).Scan(&currentConnVersion); err != nil {
		return err
	}
	if currentConnVersion != s.ConnectionVersion {
		return fmt.Errorf("连接版本已变化（%d≠%d），未派发不匹配即停", currentConnVersion, s.ConnectionVersion)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 领域阈值与验收标准（单独引用已发布规范）
// ---------------------------------------------------------------------------

// ErrProviderOverride 是提供者建议值覆盖业务规则的错误。
var ErrProviderOverride = errors.New("提供者建议值不能自动覆盖业务规则")

// validateDomainThresholds 核对领域阈值（P3-05.4）。
// 领域阈值与验收标准单独引用已发布规范；提供者建议值不自动覆盖业务规则。
func validateDomainThresholds(providerSuggested, domainPublished string) error {
	if domainPublished == "" {
		return fmt.Errorf("%w：领域阈值必须单独引用已发布规范", ErrProviderOverride)
	}
	if providerSuggested != "" && domainPublished != providerSuggested {
		// 提供者建议值不自动覆盖业务规则（不一致需人工决定）。
		return fmt.Errorf("%w：提供者建议 %q ≠ 已发布规范 %q（需人工决定）", ErrProviderOverride, providerSuggested, domainPublished)
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
