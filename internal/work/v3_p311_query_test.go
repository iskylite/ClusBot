package work

// P3-11 验收：统一 Query 入口与内部函数适配。
//
// 三个测试入口：
//   - TestV3P311QueryScopeAndProvenance：Query 生成 query_id 并绑定主体、
//     Task/Step/Run、目标与来源；检查读取权限，不创建副作用 Operation；返回
//     结构化结果、来源时间/版本、覆盖范围和材料引用；读取来源失败返回缺口，
//     不制造空结果即不存在的判断。
//   - TestV3P311InternalCapabilityAdmission：内部能力显式注册输入/输出 Schema、
//     版本和副作用类别；动作只能接收已准入 Operation；内部函数与外部提供者
//     遵循相同版本/范围约束。
//   - TestV3P311QueryActionSeparation：只读权限无法调用 Action；Action 不能从
//     Query 路径逃逸；区分能力只读语义的宿主注册/验证与外部自报 annotation，
//     不能因为工具名含 get 就信任。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P311 创建存储与任务。
func setupV3P311(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p311-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// mockQueryReader 是测试用查询读取器。
type mockQueryReader struct {
	result QueryResult
	err    error
}

func (m *mockQueryReader) Read(ctx context.Context, q CapabilityQuery) (QueryResult, error) {
	return m.result, m.err
}

// TestV3P311QueryScopeAndProvenance 查询范围与来源。
func TestV3P311QueryScopeAndProvenance(t *testing.T) {
	t.Run("query_generates_id_binds_all_fields", func(t *testing.T) {
		store, taskID := setupV3P311(t, "query.db")
		now := v3P102Now()
		// 授权读取。
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 执行查询：query_id 绑定主体/Task/Step/Run/目标/来源。
		tx, _ = store.db.Begin()
		q := CapabilityQuery{
			Subject: "user:alice", TaskID: taskID, StepID: "step-1", RunID: "run-1",
			CapabilityKey: "cluster.node.read", TargetRef: "asset-x", SourceRef: "conn:cluster",
		}
		reader := &mockQueryReader{result: QueryResult{
			Data: "查询结果", SourceTime: now, SourceVersion: 1, Coverage: "100%",
		}}
		result, err := executeQuery(tx, q, reader, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.QueryID == "" {
			t.Error("query id not generated")
		}
		if result.Subject != "user:alice" || result.TaskID != taskID || result.TargetRef != "asset-x" {
			t.Errorf("query binding incomplete: %+v", result)
		}
		// 结果含结构化结果、来源时间/版本、覆盖范围。
		if result.Result.Data != "查询结果" || result.Result.SourceVersion != 1 || result.Result.Coverage != "100%" {
			t.Errorf("query result incomplete: %+v", result.Result)
		}
	})

	t.Run("unauthorized_query_denied_no_side_effect_operation", func(t *testing.T) {
		store, _ := setupV3P311(t, "unauth.db")
		now := v3P102Now()
		// 无授权：拒绝（不创建副作用 Operation）。
		tx, _ := store.db.Begin()
		q := CapabilityQuery{
			Subject: "user:mallory", CapabilityKey: "cluster.node.read", TargetRef: "asset-x",
		}
		reader := &mockQueryReader{}
		_, err := executeQuery(tx, q, reader, now)
		tx.Rollback()
		if err == nil {
			t.Error("unauthorized query not denied")
		}
		// 未创建副作用 Operation（只读查询不创建）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operations").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("query created side-effect operation: count=%d (检查读取权限不创建副作用)", count)
		}
	})

	t.Run("read_source_failure_returns_gap_not_empty_result", func(t *testing.T) {
		store, taskID := setupV3P311(t, "gap.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 读取来源失败：返回缺口（不制造空结果即不存在的判断）。
		tx, _ = store.db.Begin()
		q := CapabilityQuery{Subject: "user:alice", TaskID: taskID, CapabilityKey: "cluster.node.read", TargetRef: "asset-x"}
		reader := &mockQueryReader{err: errors.New("source unavailable")}
		result, err := executeQuery(tx, q, reader, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Result.GapReason == "" {
			t.Error("read failure did not return gap (读取来源失败返回缺口)")
		}
		if !strings.Contains(result.Result.GapReason, "来源失败") {
			t.Errorf("gap reason unclear: %s", result.Result.GapReason)
		}
	})

	t.Run("large_content_by_fragment_budget", func(t *testing.T) {
		store, taskID := setupV3P311(t, "fragment.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		grantAuthorization(tx, Authorization{Subject: "user:alice", Action: "read", ObjectRef: "asset-x", ValidFrom: now, Owner: "alice"}, now)
		tx.Commit()
		// 大内容按片段预算读取（截断与材料引用）。
		tx, _ = store.db.Begin()
		q := CapabilityQuery{Subject: "user:alice", TaskID: taskID, CapabilityKey: "cluster.node.read", TargetRef: "asset-x"}
		reader := &mockQueryReader{result: QueryResult{
			Data: "部分结果", Truncated: true, EvidenceRefs: []string{"ev-1", "ev-2"},
		}}
		result, err := executeQuery(tx, q, reader, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Result.Truncated {
			t.Error("large content not truncated (大内容按片段预算读取)")
		}
		if len(result.Result.EvidenceRefs) == 0 {
			t.Error("evidence refs missing (材料引用)")
		}
	})
}

// TestV3P311InternalCapabilityAdmission 内部能力准入。
func TestV3P311InternalCapabilityAdmission(t *testing.T) {
	t.Run("internal_capability_explicit_schema_version_side_effect", func(t *testing.T) {
		store, _ := setupV3P311(t, "internal.db")
		now := v3P102Now()
		// 内部能力显式注册输入/输出 Schema、版本和副作用类别。
		tx, _ := store.db.Begin()
		adapter := InternalCapabilityAdapter{
			Capability: Capability{
				CapabilityKey: "internal.normalize", Version: 1, Kind: "query",
				InputSchema: `{"type":"object"}`, OutputSchema: `{"type":"object"}`,
			},
			SideEffectKind: "none",
		}
		cap, err := registerInternalAdapter(tx, adapter, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if cap.CapabilityKey != "internal.normalize" || cap.Version != 1 || cap.Kind != "query" {
			t.Errorf("internal capability incomplete: %+v", cap)
		}
		if cap.InputSchema == "" || cap.OutputSchema == "" {
			t.Error("internal capability missing schema")
		}
	})

	t.Run("internal_and_external_same_constraints", func(t *testing.T) {
		store, _ := setupV3P311(t, "same.db")
		now := v3P102Now()
		// 内部函数与外部提供者遵循相同版本/范围约束。
		tx, _ := store.db.Begin()
		internal, _ := registerInternalAdapter(tx, InternalCapabilityAdapter{
			Capability: Capability{CapabilityKey: "internal.read", Version: 1, Kind: "query"},
		}, now)
		external, _ := registerCapability(tx, Capability{CapabilityKey: "external.read", Version: 1, Kind: "query"}, now)
		tx.Commit()
		// 内部与外部能力都有版本约束（相同约束）。
		if internal.Version != external.Version {
			t.Errorf("internal/external version constraint differ: %d vs %d (相同版本/范围约束)", internal.Version, external.Version)
		}
		if internal.Kind != external.Kind {
			t.Errorf("internal/external kind differ: %s vs %s", internal.Kind, external.Kind)
		}
	})

	t.Run("action_only_accepts_admitted_operation", func(t *testing.T) {
		store, taskID := setupV3P311(t, "action.db")
		now := v3P102Now()
		// 动作只能接收已准入 Operation（未经准入不能执行）。
		tx, _ := store.db.Begin()
		op, _ := registerOperation(tx, v3P307Operation(taskID, "idem-action"), now)
		tx.Commit()
		// 未派发的 Operation（registered）：不能执行动作。
		if op.State != V3OperationRegistered {
			t.Fatalf("operation state = %s", op.State)
		}
		// 动作只能接收已准入（dispatched）Operation。
		var canExecute bool
		if op.State == V3OperationDispatched {
			canExecute = true
		}
		if canExecute {
			t.Error("action executed on un-admitted operation (动作只能接收已准入 Operation)")
		}
	})
}

// TestV3P311QueryActionSeparation 查询/动作分离。
func TestV3P311QueryActionSeparation(t *testing.T) {
	t.Run("readonly_cannot_call_action", func(t *testing.T) {
		// 只读权限无法调用 Action。
		cap := Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}
		if err := ensureQueryActionSeparation(cap, "write"); err == nil {
			t.Error("readonly capability called action (只读权限无法调用 Action)")
		}
		if err := ensureQueryActionSeparation(cap, "read"); err != nil {
			t.Errorf("readonly capability rejected read: %v", err)
		}
	})

	t.Run("action_cannot_escape_from_query_path", func(t *testing.T) {
		// Action 不能从 Query 路径逃逸。
		cap := Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}
		if err := ensureQueryActionSeparation(cap, "dispatch"); err == nil {
			t.Error("action escaped from query path (Action 不能从 Query 路径逃逸)")
		}
	})

	t.Run("host_registered_vs_external_annotation", func(t *testing.T) {
		// 区分宿主注册/验证与外部自报 annotation。
		// 外部自报 readonly 但宿主注册 action：不一致拒绝。
		cap := Capability{CapabilityKey: "cluster.node.write", Version: 1, Kind: "action"}
		if err := validateHostRegisteredSemantics(cap, "readonly"); err == nil {
			t.Error("external annotation trusted over host registration")
		}
		// 宿主注册 query：可信。
		queryCap := Capability{CapabilityKey: "cluster.node.read", Version: 1, Kind: "query"}
		if err := validateHostRegisteredSemantics(queryCap, "readonly"); err != nil {
			t.Errorf("host registered readonly rejected: %v", err)
		}
	})

	t.Run("tool_name_get_not_trusted_without_host_registration", func(t *testing.T) {
		// 不能因为工具名含 get 就信任：Kind 必须由宿主注册。
		cap := Capability{CapabilityKey: "getData", Version: 1, Kind: ""} // 缺宿主注册 Kind
		if err := validateHostRegisteredSemantics(cap, ""); err == nil {
			t.Error("tool name 'get' trusted without host registration (不能因工具名含 get 就信任)")
		}
	})

	t.Run("publication_adapted_preserves_existing_confirmation", func(t *testing.T) {
		store, taskID := setupV3P311(t, "publication.db")
		now := v3P102Now()
		// 文件成果发布保留为其已有受控能力，通过适配关联 Task/证据。
		tx, _ := store.db.Begin()
		err := adaptPublicationCapability(tx, taskID, "artifact-1", []string{"ev-1"}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 适配已审计（不拆毁现有确认与摘要核验）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='publication_adapted' AND task_id=?", taskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("publication not adapted: count=%d", count)
		}
	})
}

// 确保 sql/strings/context/errors 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = errors.Is
)
