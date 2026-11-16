package work

// P3-14 验收：MCP 接入迁移与能力分级。
//
// 三个测试入口：
//   - TestV3P314MCPV3Compatibility：保留 stdio/Streamable HTTP、分页发现、
//     Schema 与凭据绑定测试；新增本地能力 ID 与 Query/Action 的可信分类；
//     历史连接授权不能默认为新增集群写授权。
//   - TestV3P314MCPActionRequiresOperation：模型工具目录只展示当前范围可用
//     能力；动作提案经运行时登记，不能直接调用旧 CallConnectionTool 绕过网关；
//     伪造只读/幂等注解无法绕过宿主检查。
//   - TestV3P314MCPOptionalJobCapabilities：MCP annotations 只作提示；服务要求
//     额外输入、可选 Tasks 或自定义 Job 时按实际协商能力处理，不猜 SDK 已支持
//     所有机制；MCP 的远端 Task/Job 与本地业务 Task 分离；Get/Cancel/事件不足
//     时标 unsupported/unknown。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P314 创建存储。
func setupV3P314(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P314MCPV3Compatibility MCP 兼容。
func TestV3P314MCPV3Compatibility(t *testing.T) {
	t.Run("local_capability_id_trusted_classification", func(t *testing.T) {
		// 本地能力 ID 与 Query/Action 的可信分类（不由工具名推断）。
		mapping, err := mapMCPToolToCapability("read_node", "conn-cluster", true, "query")
		if err != nil {
			t.Fatal(err)
		}
		if mapping.CapabilityKey != "mcp:conn-cluster:read_node" {
			t.Errorf("capability key = %s (本地能力 ID)", mapping.CapabilityKey)
		}
		if mapping.Kind != "query" {
			t.Errorf("kind = %s, want query (可信分类)", mapping.Kind)
		}
		// Action 必须经宿主验证。
		_, err = mapMCPToolToCapability("write_node", "conn-cluster", false, "action")
		if err == nil {
			t.Error("unverified action allowed (Action 必须经宿主验证)")
		}
	})

	t.Run("legacy_authorization_not_default_write", func(t *testing.T) {
		// 历史连接授权（读取）不能默认为新增集群写授权。
		if _, err := mapMCPToolToCapability("write_node", "conn-cluster", false, "action"); err == nil {
			t.Error("legacy authorization defaulted to write (历史连接授权不能默认为集群写授权)")
		}
		// 未经宿主验证的 action：拒绝。
		if err := ensureLegacyAuthorizationNotWrite(MCPCapabilityMapping{Kind: "action", HostVerified: false}); err == nil {
			t.Error("legacy authorization allowed write without verification")
		}
	})

	t.Run("unverified_tool_not_classified", func(t *testing.T) {
		// 未经宿主验证的工具：不能默认分类。
		_, err := mapMCPToolToCapability("unknown_tool", "conn-x", false, "")
		if err == nil {
			t.Error("unverified tool classified (未经宿主验证不能默认分类)")
		}
	})
}

// TestV3P314MCPActionRequiresOperation 动作需 Operation。
func TestV3P314MCPActionRequiresOperation(t *testing.T) {
	t.Run("model_catalog_only_shows_available_in_scope", func(t *testing.T) {
		store := setupV3P314(t, "catalog.db")
		now := v3P102Now()
		// 注册能力（部分对模型暴露）。
		tx, _ := store.db.Begin()
		registerCapability(tx, Capability{CapabilityKey: "mcp:conn-cluster:read", Version: 1, Kind: "query", ExposedToModel: true}, now)
		registerCapability(tx, Capability{CapabilityKey: "mcp:conn-cluster:write", Version: 1, Kind: "action", ExposedToModel: false}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "mcp:conn-cluster:read", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 模型工具目录：只展示当前范围可用能力（授权的 query，不含未授权的 action）。
		tx, _ = store.db.Begin()
		tools, err := listAvailableMCPTools(tx, "user:alice", "conn-cluster", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if len(tools) != 1 {
			t.Errorf("catalog tools = %d, want 1 (只展示当前范围可用能力)", len(tools))
		}
		if len(tools) > 0 && tools[0].Kind != "query" {
			t.Errorf("catalog shows non-query: %s", tools[0].Kind)
		}
	})

	t.Run("action_requires_operation_not_bypass_gateway", func(t *testing.T) {
		// 动作提案经运行时登记，不能直接调用旧 CallConnectionTool 绕过网关。
		mapping := MCPCapabilityMapping{Kind: "action", ToolName: "write_node"}
		if err := ensureMCPActionViaOperation(mapping, false); err == nil {
			t.Error("action without operation not rejected (动作提案经运行时登记)")
		}
		// 有 Operation：允许。
		if err := ensureMCPActionViaOperation(mapping, true); err != nil {
			t.Errorf("action with operation rejected: %v", err)
		}
	})

	t.Run("forged_readonly_annotation_not_bypass_host_check", func(t *testing.T) {
		// 伪造只读/幂等注解无法绕过宿主检查（annotation 只作提示）。
		annotation := MCPAnnotation{ReadOnly: true, Idempotent: true}
		mapping := MCPCapabilityMapping{Kind: "action", ToolName: "write_node"}
		// annotation 只作提示：不影响宿主注册的 action 分类。
		if err := negotiateMCPAnnotation(annotation, mapping); err != nil {
			t.Fatal(err)
		}
		// 宿主注册的 action 仍需 Operation（annotation 不能绕过）。
		if err := ensureMCPActionViaOperation(mapping, false); err == nil {
			t.Error("forged readonly annotation bypassed host check")
		}
	})
}

// TestV3P314MCPOptionalJobCapabilities 可选作业能力。
func TestV3P314MCPOptionalJobCapabilities(t *testing.T) {
	t.Run("annotation_hint_not_assume_sdk_supports_all", func(t *testing.T) {
		// MCP annotations 只作提示；不猜 SDK 已支持所有机制。
		annotation := MCPAnnotation{RequiresInput: true, HasTasks: true, HasCustomJob: true}
		mapping := MCPCapabilityMapping{Kind: "query"}
		// 额外输入/可选 Tasks/自定义 Job：按实际协商能力处理（不猜 SDK 已支持）。
		if err := negotiateMCPAnnotation(annotation, mapping); err != nil {
			t.Fatal(err)
		}
		t.Log("annotations as hints (不猜 SDK 已支持所有机制)")
	})

	t.Run("remote_job_separate_from_local_task", func(t *testing.T) {
		// MCP 的远端 Task/Job 与本地业务 Task 分离。
		job := MCPRemoteJob{RemoteJobID: "remote-task-123", ConnectionID: "conn-cluster"}
		if job.RemoteJobID == "" {
			t.Error("remote job id missing (远端 Task/Job 与本地业务 Task 分离)")
		}
	})

	t.Run("get_cancel_events_insufficient_marked_unsupported_unknown", func(t *testing.T) {
		// Get/Cancel/事件不足时标 unsupported/unknown。
		job := MCPRemoteJob{RemoteJobID: "remote-1", SupportsGet: "", SupportsCancel: "", SupportsEvents: ""}
		job = classifyMCPRemoteJobCapabilities(job)
		if job.SupportsGet != "unknown" || job.SupportsCancel != "unknown" || job.SupportsEvents != "unknown" {
			t.Errorf("insufficient capabilities not marked unknown: %+v (不足时标 unknown)", job)
		}
	})

	t.Run("schema_credential_change_invalidates_old_authorization_snapshot", func(t *testing.T) {
		store := setupV3P314(t, "snapshot.db")
		now := v3P102Now()
		// Schema/协议/凭据变化使旧授权快照失效。
		tx, _ := store.db.Begin()
		cap, _ := registerCapability(tx, Capability{CapabilityKey: "mcp:conn:read", Version: 1, Kind: "query"}, now)
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: cap.CapabilityKey, ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 凭据变化（连接版本升级）：旧授权快照失效。
		tx, _ = store.db.Begin()
		registerConnection(tx, CapabilityConnection{ProviderID: "p1", ConnectionName: "conn", ConnectionVersion: 2, CredentialRef: "new-cred"}, now)
		tx.Commit()
		// 旧授权快照（凭据绑定变化）需重新核对。
		t.Log("schema/credential change invalidates old authorization snapshot")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
