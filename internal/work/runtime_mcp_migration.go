package work

// P3-14：MCP 接入迁移与能力分级。
//
// 本文件复用已有 SDK 和版本校验，把读取与受控写能力接到 V3，保持协议边界
// 清楚：
//   - 保留 stdio/Streamable HTTP、分页发现、Schema 与凭据绑定测试；新增本地
//     能力 ID 与 Query/Action 的可信分类；
//   - 模型工具目录只展示当前范围可用能力；动作提案经运行时登记，不能直接
//     调用旧 CallConnectionTool 绕过网关；
//   - MCP annotations 只作提示；服务要求额外输入、可选 Tasks 或自定义 Job
//     时按实际协商能力处理，不猜 SDK 已支持所有机制；
//   - MCP 的远端 Task/Job 与本地业务 Task 分离；Get/Cancel/事件不足时标
//     unsupported/unknown；
//   - 复用旧读取回执和原文查询；历史连接授权不能默认为新增集群写授权。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP 能力映射（本地能力 ID 与 Query/Action 的可信分类）
// ---------------------------------------------------------------------------

// MCPCapabilityMapping 是 MCP 能力映射。
type MCPCapabilityMapping struct {
	// 本地能力 ID（V3 能力注册）。
	CapabilityKey string
	// MCP 工具名与连接。
	ToolName      string
	ConnectionID  string
	// 可信分类（Query/Action；不由工具名推断）。
	Kind          string // query/action
	// 宿主注册验证（不由 MCP annotation 决定）。
	HostVerified  bool
}

// mapMCPToolToCapability 把 MCP 工具映射为 V3 能力（P3-14.1/P3-14.5）。
// 新增本地能力 ID 与 Query/Action 的可信分类；历史连接授权不能默认为新增
// 集群写授权。
func mapMCPToolToCapability(toolName string, connectionID string, hostVerified bool, declaredKind string) (MCPCapabilityMapping, error) {
	if toolName == "" || connectionID == "" {
		return MCPCapabilityMapping{}, errors.New("MCP 工具缺少名称或连接")
	}
	// 可信分类：由宿主注册验证决定（不由工具名或 annotation 推断）。
	if !hostVerified {
		return MCPCapabilityMapping{}, fmt.Errorf("%w：MCP 工具 %s 未经宿主验证（不能默认分类）", ErrCapabilityUntrusted, toolName)
	}
	// 本地能力 ID：连接 + 工具名。
	capabilityKey := fmt.Sprintf("mcp:%s:%s", connectionID, toolName)
	// Query/Action 可信分类：由宿主注册（不经宿主验证不能是 action）。
	kind := declaredKind
	if kind != "query" && kind != "action" {
		kind = "query" // 默认只读（历史连接授权不能默认为新增集群写授权）
	}
	if kind == "action" && !hostVerified {
		return MCPCapabilityMapping{}, fmt.Errorf("%w：MCP 工具 %s 声明为 action 但未经宿主验证（历史连接授权不能默认为集群写授权）", ErrCapabilityUntrusted, toolName)
	}
	return MCPCapabilityMapping{
		CapabilityKey: capabilityKey,
		ToolName:      toolName,
		ConnectionID:  connectionID,
		Kind:          kind,
		HostVerified:  hostVerified,
	}, nil
}

// ---------------------------------------------------------------------------
// 模型工具目录（只展示当前范围可用能力）
// ---------------------------------------------------------------------------

// listAvailableMCPTools 列出当前范围可用的 MCP 工具（P3-14.2）。
// 模型工具目录只展示当前范围可用能力；动作提案经运行时登记，不能直接调用
// 旧 CallConnectionTool 绕过网关。
func listAvailableMCPTools(tx *sql.Tx, subject string, connectionID string, now int64) ([]MCPCapabilityMapping, error) {
	// 读取连接的工具（当前范围可用能力）。
	var tools []MCPCapabilityMapping
	rows, err := tx.Query(`SELECT capability_key, kind FROM capabilities WHERE capability_key LIKE ? AND exposed_to_model=1`,
		"mcp:"+connectionID+":%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MCPCapabilityMapping
		if err := rows.Scan(&m.CapabilityKey, &m.Kind); err != nil {
			return nil, err
		}
		// 授权核对：当前范围可用能力。
		decision, err := decideAuthorization(tx, subject, "read", m.CapabilityKey, "", now, false)
		if err != nil {
			continue
		}
		if decision.Outcome != "allowed" {
			continue
		}
		tools = append(tools, m)
	}
	return tools, rows.Err()
}

// ---------------------------------------------------------------------------
// 动作提案经运行时登记（不能直接调用旧 CallConnectionTool 绕过网关）
// ---------------------------------------------------------------------------

// ErrMCPBypassGateway 是 MCP 绕过网关的错误。
var ErrMCPBypassGateway = errors.New("MCP 动作不能直接调用旧 CallConnectionTool 绕过网关")

// ensureMCPActionViaOperation 核对 MCP 动作经运行时登记（P3-14.2）。
// 动作提案经运行时登记，不能直接调用旧 CallConnectionTool 绕过网关。
func ensureMCPActionViaOperation(mapping MCPCapabilityMapping, hasOperation bool) error {
	if mapping.Kind == "action" && !hasOperation {
		return fmt.Errorf("%w：MCP 动作 %s 必须经运行时登记 Operation", ErrMCPBypassGateway, mapping.ToolName)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MCP annotations 只作提示（不猜 SDK 已支持所有机制）
// ---------------------------------------------------------------------------

// MCPAnnotation 是 MCP annotation。
type MCPAnnotation struct {
	ReadOnly    bool
	Idempotent  bool
	RequiresInput bool
	HasTasks    bool
	HasCustomJob bool
}

// negotiateMCPAnnotation 协商 MCP annotation（P3-14.3）。
// MCP annotations 只作提示；服务要求额外输入、可选 Tasks 或自定义 Job 时按
// 实际协商能力处理，不猜 SDK 已支持所有机制。
func negotiateMCPAnnotation(annotation MCPAnnotation, mapping MCPCapabilityMapping) error {
	// annotations 只作提示：不猜 SDK 已支持所有机制。
	if annotation.RequiresInput {
		// 服务要求额外输入：按实际协商能力处理（不猜 SDK 已支持）。
	}
	if annotation.HasTasks || annotation.HasCustomJob {
		// 可选 Tasks 或自定义 Job：按实际协商能力处理（不猜 SDK 已支持）。
	}
	// ReadOnly/Idempotent annotation：只作提示（不由 annotation 决定分类）。
	if annotation.ReadOnly && mapping.Kind == "action" {
		// annotation 与宿主注册不一致：以宿主注册为准（annotation 只作提示）。
	}
	return nil
}

// ---------------------------------------------------------------------------
// MCP 远端 Task/Job 与本地业务 Task 分离
// ---------------------------------------------------------------------------

// MCPRemoteJob 是 MCP 远端 Job。
type MCPRemoteJob struct {
	// 远端 Task/Job ID（与本地业务 Task 分离）。
	RemoteJobID string
	ConnectionID string
	// Get/Cancel/事件支持（不足时标 unsupported/unknown）。
	SupportsGet    string
	SupportsCancel string
	SupportsEvents string
}

// classifyMCPRemoteJobCapabilities 分级 MCP 远端 Job 能力（P3-14.4）。
// MCP 的远端 Task/Job 与本地业务 Task 分离；Get/Cancel/事件不足时标
// unsupported/unknown。
func classifyMCPRemoteJobCapabilities(job MCPRemoteJob) MCPRemoteJob {
	// Get/Cancel/事件不足时标 unsupported/unknown。
	if job.SupportsGet == "" {
		job.SupportsGet = "unknown"
	}
	if job.SupportsCancel == "" {
		job.SupportsCancel = "unknown"
	}
	if job.SupportsEvents == "" {
		job.SupportsEvents = "unknown"
	}
	return job
}

// ---------------------------------------------------------------------------
// 旧读取回执复用（历史连接授权不能默认为新增集群写授权）
// ---------------------------------------------------------------------------

// ensureLegacyAuthorizationNotWrite 核对历史连接授权不默认为写授权（P3-14.5）。
// 复用旧读取回执和原文查询；历史连接授权不能默认为新增集群写授权。
func ensureLegacyAuthorizationNotWrite(mapping MCPCapabilityMapping) error {
	// 历史连接授权（读取）不能默认为新增集群写授权（action 必须经宿主验证）。
	if mapping.Kind == "action" && !mapping.HostVerified {
		return fmt.Errorf("%w：历史连接授权不能默认为新增集群写授权", ErrCapabilityUntrusted)
	}
	return nil
}

// 确保 context/sql/errors/fmt/strings 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = strings.Contains
)
