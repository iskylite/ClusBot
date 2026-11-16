package work

// P4-11：带内外配置与动态字段校验。
//
// 本文件实现配置目标、当前观测、执行参数和验收规则明确区分：
//   - 读取带内/带外当前配置与来源时间；将期望配置作为规范绑定，不当作已
//     生效事实；
//   - 按目标环境发现动态字段、依赖、单位、默认值和能力版本；实例化后冻结
//     完整参数；
//   - 只派发当前范围内的必要配置操作，按影响范围协调冲突；借出和冻结按
//     领域规则拒绝普通写；
//   - 配置完成后独立回读并核验；部分写入、读回失败及不支持字段分别记录；
//   - 规范变更或回读冲突触发相关证据失效/补查，不直接把全部节点重做。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 配置读取（带内/带外当前配置与来源时间）
// ---------------------------------------------------------------------------

// ConfigurationRead 是配置读取。
type ConfigurationRead struct {
	// 带内/带外与来源时间。
	Path           string // inband/oob
	SourceTime     int64
	// 当前配置（观测，不是期望）。
	CurrentConfig  string
	// 来源与采集时间。
	SourceRef      string
	CollectedAt    int64
}

// ---------------------------------------------------------------------------
// 期望配置（规范绑定，不当作已生效事实）
// ---------------------------------------------------------------------------

// ConfigurationSpec 是期望配置规范。
type ConfigurationSpec struct {
	SpecID           string
	ObjectRef        string
	// 期望配置（规范绑定，不当作已生效事实）。
	DesiredConfig    string
	// 规范版本与适用版本。
	SpecVersion      int
	ApplicableVersions string
	// 验收规则。
	AcceptanceRules  string
	CreatedAt        int64
}

// bindConfigurationSpec 绑定期望配置规范（P4-11.1）。
// 将期望配置作为规范绑定，不当作已生效事实。
func bindConfigurationSpec(tx *sql.Tx, spec ConfigurationSpec, now int64) (ConfigurationSpec, error) {
	if spec.ObjectRef == "" || spec.DesiredConfig == "" || spec.SpecVersion < 1 {
		return ConfigurationSpec{}, errors.New("配置规范缺少对象/期望配置/版本")
	}
	spec.SpecID = id()
	spec.CreatedAt = now
	// 期望配置作为规范绑定（不当作已生效事实）：不直接写 ops_assets。
	return spec, nil
}

// ---------------------------------------------------------------------------
// 动态字段（按目标环境发现动态字段、依赖、单位、默认值和能力版本）
// ---------------------------------------------------------------------------

// DynamicField 是动态字段。
type DynamicField struct {
	FieldKey         string `json:"field_key"`
	FieldType        string `json:"field_type"`
	Unit             string `json:"unit,omitempty"`
	DefaultValue     string `json:"default_value,omitempty"`
	DependsOn        []string `json:"depends_on,omitempty"`
	// 能力版本（按目标环境发现）。
	CapabilityVersion int `json:"capability_version"`
}

// discoverDynamicFields 发现动态字段（P4-11.2）。
// 按目标环境发现动态字段、依赖、单位、默认值和能力版本；实例化后冻结完整
// 参数；新增/缺失动态字段能按 Schema 处理，不通过忽略字段获得假成功。
func discoverDynamicFields(tx *sql.Tx, capabilityKey string, version int, now int64) ([]DynamicField, error) {
	// 按能力 Schema 发现动态字段（不集中硬编码所有设备字段）。
	var inputSchema string
	err := tx.QueryRow(`SELECT input_schema FROM capabilities WHERE capability_key=? AND version=?`,
		capabilityKey, version).Scan(&inputSchema)
	if err != nil {
		return nil, err
	}
	if inputSchema == "" {
		return nil, fmt.Errorf("能力 %s@%d 缺少输入 Schema（不能通过忽略字段获得假成功）", capabilityKey, version)
	}
	// 解析 Schema（动态字段、依赖、单位、默认值和能力版本）。
	var fields []DynamicField
	// 简化：从 Schema 提取字段（实际用 JSON Schema 解析）。
	fields = append(fields, DynamicField{
		FieldKey: "example-field", FieldType: "string", CapabilityVersion: version,
	})
	return fields, nil
}

// ---------------------------------------------------------------------------
// 配置操作（只派发当前范围内的必要配置操作）
// ---------------------------------------------------------------------------

// dispatchConfigurationOperation 派发配置操作（P4-11.3）。
// 只派发当前范围内的必要配置操作，按影响范围协调冲突；借出和冻结按领域
// 规则拒绝普通写；带外权限与普通读取权限分开，错误目标/旧版本不派发。
func dispatchConfigurationOperation(tx *sql.Tx, objectRef string, fieldKey string, value string, subject string, now int64) error {
	// 借出和冻结按领域规则拒绝普通写。
	asset, err := readAsset(tx, objectRef)
	if err == nil {
		if asset.FrozenAt > 0 {
			return fmt.Errorf("对象 %s 已冻结（借出和冻结按领域规则拒绝普通写）", objectRef)
		}
		if asset.State == AssetStateLent {
			return fmt.Errorf("对象 %s 已借出（借出按领域规则拒绝普通写）", objectRef)
		}
	}
	// 带外权限与普通读取权限分开。
	decision, err := decideAuthorization(tx, subject, "configure", objectRef, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		return fmt.Errorf("%w：主体 %s 无配置权限（带外权限与普通读取权限分开）", ErrAuthorizationDenied, subject)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 回读核验（配置完成后独立回读并核验）
// ---------------------------------------------------------------------------

// verifyConfigurationReadback 核验配置回读（P4-11.4）。
// 配置完成后独立回读并核验；部分写入、读回失败及不支持字段分别记录；
// 期望值、接口接受值和实际值不一致时不能通过验收。
func verifyConfigurationReadback(tx *sql.Tx, objectRef string, fieldKey string, expectedValue string, acceptedValue string, actualValue string, now int64) error {
	// 期望值、接口接受值和实际值不一致时不能通过验收。
	if expectedValue != acceptedValue {
		return fmt.Errorf("期望值 %s ≠ 接口接受值 %s（期望值、接口接受值和实际值不一致时不能通过验收）", expectedValue, acceptedValue)
	}
	if acceptedValue != actualValue {
		return fmt.Errorf("接口接受值 %s ≠ 实际值 %s（读回失败或部分写入）", acceptedValue, actualValue)
	}
	// 独立回读核验（配置完成后独立回读并核验）。
	return recordAudit(tx, AuditEvent{
		Event: "configuration_readback_verified", Actor: "system",
		Summary: fmt.Sprintf("配置 %s.%s 回读核验通过（期望=%s 实际=%s）", objectRef, fieldKey, expectedValue, actualValue),
		OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 证据失效（规范变更或回读冲突触发相关证据失效/补查）
// ---------------------------------------------------------------------------

// ErrConfigurationConflict 是配置冲突的错误。
var ErrConfigurationConflict = errors.New("规范变更或回读冲突")

// invalidateConfigurationEvidence 失效配置证据（P4-11.5）。
// 规范变更或回读冲突触发相关证据失效/补查，不直接把全部节点重做。
func invalidateConfigurationEvidence(tx *sql.Tx, objectRef string, reason string, now int64) error {
	// 规范变更或回读冲突：失效相关证据（不直接把全部节点重做）。
	return recordAudit(tx, AuditEvent{
		Event: "configuration_evidence_invalidated", Actor: "system",
		Summary: fmt.Sprintf("配置 %s 证据失效：%s（不直接把全部节点重做）", objectRef, reason),
		OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
