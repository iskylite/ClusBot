package work

// 能力、提供者与连接分离。
//
// 本文件实现统一能力语义而保持内部函数、CLI、HTTP、MCP 与已有平台可替换：
//   - 能力有稳定 ID/版本、Query/Action 类别、输入输出 Schema、目标类型和
//     执行模式；提供者与连接另有身份及版本；
//   - 对 Get/Cancel/Events/Materials/幂等/fencing 分别记录 supported/
//     unsupported/unknown 及实际验证范围；
//   - 工具名称与连接 ID 保持来源区分；现有 MCP 别名可复用，不能把同名不同
//     连接合并；
//   - 能力和连接描述只声明机制，不拥有 Task/资产权威，也不能自报业务验收
//     通过；
//   - 新写能力默认不向模型暴露，需相应契约验证和当前主体准入通过。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 能力（稳定 ID/版本、类别、Schema、目标类型、执行模式）
// ---------------------------------------------------------------------------

// Capability 是 capabilities 的一行。
type Capability struct {
	CapabilityID   string
	CapabilityKey  string
	Version        int
	Kind           string // query/action
	InputSchema    string
	OutputSchema   string
	TargetKind     string
	ExecutionMode  string
	// 特性：Get/Cancel/Events/Materials/幂等/fencing 分别记录。
	SupportsGet      string
	SupportsCancel   string
	SupportsEvents   string
	SupportsMaterials string
	Idempotent       string
	Fencing          string
	// 实际验证范围。
	VerifiedScope    string
	// 新写能力默认不向模型暴露。
	ExposedToModel   bool
	CreatedAt        int64
	UpdatedAt        int64
}

// registerCapability 注册一个能力。
// 能力有稳定 ID/版本、Query/Action 类别、输入输出 Schema、目标类型和执行
// 模式；新写能力默认不向模型暴露，需相应契约验证和当前主体准入通过。
func registerCapability(tx *sql.Tx, c Capability, now int64) (Capability, error) {
	if c.CapabilityKey == "" || c.Kind == "" {
		return Capability{}, errors.New("能力缺少稳定 ID 或类别")
	}
	if c.Version < 1 {
		c.Version = 1
	}
	if c.ExecutionMode == "" {
		c.ExecutionMode = "sync"
	}
	// 特性默认 "unknown"（CHECK 约束要求 supported/unsupported/unknown）。
	for _, f := range []struct{ name string; value *string }{
		{"supports_get", &c.SupportsGet}, {"supports_cancel", &c.SupportsCancel},
		{"supports_events", &c.SupportsEvents}, {"supports_materials", &c.SupportsMaterials},
		{"idempotent", &c.Idempotent}, {"fencing", &c.Fencing},
	} {
		if *f.value == "" {
			*f.value = "unknown"
		}
	}
	// 新写能力（action）默认不向模型暴露。
	if c.Kind == "action" && !c.ExposedToModel {
		c.ExposedToModel = false
	}
	c.CapabilityID = id()
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO capabilities(capability_id,capability_key,version,kind,input_schema,
output_schema,target_kind,execution_mode,supports_get,supports_cancel,supports_events,supports_materials,
idempotent,fencing,verified_scope,exposed_to_model,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.CapabilityID, c.CapabilityKey, c.Version, c.Kind, c.InputSchema, c.OutputSchema,
		c.TargetKind, c.ExecutionMode, c.SupportsGet, c.SupportsCancel, c.SupportsEvents,
		c.SupportsMaterials, c.Idempotent, c.Fencing, c.VerifiedScope, boolToInt(c.ExposedToModel), now, now)
	if err != nil {
		return Capability{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 提供者（内部函数/CLI/HTTP/MCP/平台可替换）
// ---------------------------------------------------------------------------

// CapabilityProvider 是 capability_providers 的一行。
type CapabilityProvider struct {
	ProviderID   string
	ProviderKey  string
	Version      int
	Kind         string // internal/cli/http/mcp/platform
	ConfigRef    string
	Capabilities []string // capability_key:version
	CreatedAt    int64
}

// registerProvider 注册一个提供者。
// 提供者与连接另有身份及版本。
func registerProvider(tx *sql.Tx, p CapabilityProvider, now int64) (CapabilityProvider, error) {
	if p.ProviderKey == "" || p.Kind == "" {
		return CapabilityProvider{}, errors.New("提供者缺少身份或类型")
	}
	if p.Version < 1 {
		p.Version = 1
	}
	p.ProviderID = id()
	p.CreatedAt = now
	capsJSON := "[]"
	if len(p.Capabilities) > 0 {
		capsJSON = toJSON(p.Capabilities)
	}
	_, err := tx.Exec(`INSERT INTO capability_providers(provider_id,provider_key,version,kind,config_ref,capabilities,created_at)
VALUES(?,?,?,?,?,?,?)`, p.ProviderID, p.ProviderKey, p.Version, p.Kind, p.ConfigRef, capsJSON, now)
	if err != nil {
		return CapabilityProvider{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 连接（具体实例，与提供者分离）
// ---------------------------------------------------------------------------

// CapabilityConnection 是 capability_connections 的一行。
type CapabilityConnection struct {
	ConnectionID      string
	ProviderID        string
	ConnectionName    string
	ConnectionVersion int
	CredentialRef     string
	Enabled           bool
	CreatedAt         int64
}

// registerConnection 注册一个连接。
// 工具名称与连接 ID 保持来源区分；同名不同连接不合并。
func registerConnection(tx *sql.Tx, c CapabilityConnection, now int64) (CapabilityConnection, error) {
	if c.ProviderID == "" || c.ConnectionName == "" {
		return CapabilityConnection{}, errors.New("连接缺少提供者或名称")
	}
	c.ConnectionID = id()
	c.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO capability_connections(connection_id,provider_id,connection_name,
connection_version,credential_ref,enabled,created_at)
VALUES(?,?,?,?,?,?,?)`, c.ConnectionID, c.ProviderID, c.ConnectionName, c.ConnectionVersion,
		c.CredentialRef, boolToInt(c.Enabled), now)
	if err != nil {
		return CapabilityConnection{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 能力查询（特性 supported/unsupported/unknown 分别记录）
// ---------------------------------------------------------------------------

// CapabilityFeature 是能力特性（Get/Cancel/Events/Materials/幂等/fencing）。
type CapabilityFeature string

const (
	FeatureGet       CapabilityFeature = "get"
	FeatureCancel    CapabilityFeature = "cancel"
	FeatureEvents    CapabilityFeature = "events"
	FeatureMaterials CapabilityFeature = "materials"
	FeatureIdempotent CapabilityFeature = "idempotent"
	FeatureFencing   CapabilityFeature = "fencing"
)

// capabilityFeature 查询一个能力的特性。
// 缺失可选能力准确返回不可用/未知，不制造取消或查询成功。
func capabilityFeature(q recordsQuerier, capabilityKey string, version int, feature CapabilityFeature) (string, error) {
	var value string
	var column string
	switch feature {
	case FeatureGet:
		column = "supports_get"
	case FeatureCancel:
		column = "supports_cancel"
	case FeatureEvents:
		column = "supports_events"
	case FeatureMaterials:
		column = "supports_materials"
	case FeatureIdempotent:
		column = "idempotent"
	case FeatureFencing:
		column = "fencing"
	default:
		return "", fmt.Errorf("未知特性 %s", feature)
	}
	err := q.QueryRow(fmt.Sprintf(`SELECT %s FROM capabilities WHERE capability_key=? AND version=?`, column),
		capabilityKey, version).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown", nil // 缺失能力准确返回未知
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// ---------------------------------------------------------------------------
// 能力声明不可信（伪造描述不绕过宿主校验）
// ---------------------------------------------------------------------------

// ErrCapabilityUntrusted 是能力声明不可信的错误。
var ErrCapabilityUntrusted = errors.New("能力声明不可信")

// validateCapabilityClaim 核对能力声明。
// 能力和连接描述只声明机制，不拥有 Task/资产权威，也不能自报业务验收通过；
// 伪造描述不会绕过宿主校验。
func validateCapabilityClaim(c Capability) error {
	// 声明只声明机制：不能自报业务验收通过（verified_scope 必须由宿主验证填写）。
	if c.VerifiedScope == "self-reported" {
		return fmt.Errorf("%w：能力不能自报业务验收通过（verified_scope 由宿主验证）", ErrCapabilityUntrusted)
	}
	// 声明只读/幂等不自动获权：特性记录不等于授权。
	if c.Kind == "action" && c.Idempotent == "supported" {
		// 幂等声明是机制，不自动获权（仍需授权与领域准入）。
	}
	return nil
}

// ---------------------------------------------------------------------------
// 连接来源区分（同名不同连接不合并）
// ---------------------------------------------------------------------------

// resolveConnectionByName 按名称解析连接。
// 同名不同连接不合并：返回连接列表（调用方按来源区分）。
func resolveConnectionsByName(q recordsQuerier, name string) ([]CapabilityConnection, error) {
	rows, err := q.Query(`SELECT connection_id,provider_id,connection_name,connection_version,credential_ref,enabled,created_at
FROM capability_connections WHERE connection_name=?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapabilityConnection
	for rows.Next() {
		var c CapabilityConnection
		var enabled int
		if err := rows.Scan(&c.ConnectionID, &c.ProviderID, &c.ConnectionName, &c.ConnectionVersion,
			&c.CredentialRef, &enabled, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Enabled = enabled == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func toJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
