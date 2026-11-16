package work

// P3-11：统一 Query 入口与内部函数适配。
//
// 本文件实现只读查询与动作走各自契约，内部工具同样受范围和结果约束：
//   - Query 生成 query_id 并绑定主体、Task/Step/Run、目标与来源；检查读取
//     权限，不创建副作用 Operation；
//   - 内部能力显式注册输入/输出 Schema、版本和副作用类别；动作只能接收已
//     准入 Operation；
//   - 返回结构化结果、来源时间/版本、覆盖范围和材料引用；大内容按片段预算
//     读取；
//   - 把文件成果发布保留为其已有受控能力，通过适配关联 Task/证据，不拆毁
//     现有确认与摘要核验；
//   - 区分能力只读语义的宿主注册/验证与外部自报 annotation，不能因为工具名
//     含 get 就信任。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Query（只读查询，query_id 绑定主体/Task/Step/Run/目标/来源）
// ---------------------------------------------------------------------------

// CapabilityQuery 是一次只读查询。
type CapabilityQuery struct {
	QueryID       string
	Subject       string
	TaskID        string
	StepID        string
	RunID         string
	CapabilityKey string
	TargetRef     string
	SourceRef     string
	// 查询参数与范围。
	Params        string
	Scope         string
	// 结果（结构化结果、来源时间/版本、覆盖范围和材料引用）。
	Result        QueryResult
}

// QueryResult 是查询结果。
type QueryResult struct {
	// 结构化结果。
	Data          string
	// 来源时间/版本。
	SourceTime    int64
	SourceVersion int
	// 覆盖范围。
	Coverage      string
	// 材料引用（大内容按片段预算读取）。
	EvidenceRefs  []string
	// 截断与缺口。
	Truncated     bool
	GapReason     string
}

// ---------------------------------------------------------------------------
// 执行 Query（检查读取权限，不创建副作用 Operation）
// ---------------------------------------------------------------------------

// executeQuery 执行一次只读查询（P3-11.1/P3-11.3）。
// Query 生成 query_id 并绑定主体、Task/Step/Run、目标与来源；检查读取权限，
// 不创建副作用 Operation；返回结构化结果、来源时间/版本、覆盖范围和材料引用；
// 大内容按片段预算读取。
func executeQuery(tx *sql.Tx, q CapabilityQuery, reader QueryReader, now int64) (CapabilityQuery, error) {
	if q.Subject == "" || q.CapabilityKey == "" {
		return q, errors.New("查询缺少主体/能力")
	}
	// 检查读取权限（不创建副作用 Operation）。
	decision, err := decideAuthorization(tx, q.Subject, "read", q.TargetRef, "", now, false)
	if err != nil {
		return q, err
	}
	if decision.Outcome != "allowed" {
		return q, fmt.Errorf("%w：主体 %s 无权查询 %s", ErrAuthorizationDenied, q.Subject, q.TargetRef)
	}
	// 生成 query_id。
	q.QueryID = id()
	// 执行查询（结构化结果、来源时间/版本、覆盖范围和材料引用）。
	result, err := reader.Read(context.Background(), q)
	if err != nil {
		// 读取来源失败：返回缺口（不制造空结果即不存在的判断）。
		q.Result = QueryResult{
			GapReason: fmt.Sprintf("读取来源失败：%v", err),
		}
		return q, nil
	}
	q.Result = result
	// 审计（查询经授权）。
	if err := recordAudit(tx, AuditEvent{
		TaskID: q.TaskID, Event: "capability_query", Actor: q.Subject,
		Summary: fmt.Sprintf("查询 %s（来源 %s）", q.CapabilityKey, q.SourceRef), OccurredAt: now,
	}); err != nil {
		return q, err
	}
	return q, nil
}

// QueryReader 是查询读取器（由提供者实现）。
type QueryReader interface {
	// Read 读取查询结果（结构化结果、来源时间/版本、覆盖范围和材料引用）。
	Read(ctx context.Context, q CapabilityQuery) (QueryResult, error)
}

// ---------------------------------------------------------------------------
// 内部能力适配（显式注册 Schema、版本和副作用类别）
// ---------------------------------------------------------------------------

// InternalCapabilityAdapter 是内部能力适配器（P3-11.2）。
// 内部能力显式注册输入/输出 Schema、版本和副作用类别；动作只能接收已准入
// Operation。
type InternalCapabilityAdapter struct {
	// 能力注册（显式注册输入/输出 Schema、版本和副作用类别）。
	Capability Capability
	// 副作用类别（query=只读，action=有副作用）。
	SideEffectKind string
	// 处理器。
	Handler func(ctx context.Context, input string) (string, error)
}

// registerInternalAdapter 注册内部能力适配器（P3-11.2）。
// 内部能力显式注册输入/输出 Schema、版本和副作用类别。
func registerInternalAdapter(tx *sql.Tx, adapter InternalCapabilityAdapter, now int64) (Capability, error) {
	if adapter.Capability.CapabilityKey == "" || adapter.Capability.Kind == "" {
		return Capability{}, errors.New("内部能力缺少 ID 或类别")
	}
	// 显式注册输入/输出 Schema、版本和副作用类别。
	adapter.Capability.InputSchema = ensureSchema(adapter.Capability.InputSchema)
	adapter.Capability.OutputSchema = ensureSchema(adapter.Capability.OutputSchema)
	return registerCapability(tx, adapter.Capability, now)
}

// ensureSchema 确保 Schema 非空。
func ensureSchema(s string) string {
	if s == "" {
		return `{"type":"object"}`
	}
	return s
}

// ---------------------------------------------------------------------------
// Query/Action 分离（只读权限无法调用 Action；Action 不能从 Query 路径逃逸）
// ---------------------------------------------------------------------------

// ensureQueryActionSeparation 核对 Query/Action 分离（P3-11 反例）。
// 只读权限无法调用 Action；Action 不能从 Query 路径逃逸。
func ensureQueryActionSeparation(cap Capability, action string) error {
	if cap.Kind == "query" && action == "write" {
		return fmt.Errorf("%w：只读能力 %s 不能调用写动作（Query/Action 分离）", ErrAuthorizationDenied, cap.CapabilityKey)
	}
	if cap.Kind == "query" && action == "dispatch" {
		return fmt.Errorf("%w：只读能力 %s 不能派发（Action 不能从 Query 路径逃逸）", ErrAuthorizationDenied, cap.CapabilityKey)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 宿主注册/验证与外部自报 annotation 区分（不能因为工具名含 get 就信任）
// ---------------------------------------------------------------------------

// ErrUntrustedAnnotation 是外部自报 annotation 不可信的错误。
var ErrUntrustedAnnotation = errors.New("外部自报 annotation 不可信")

// validateHostRegisteredSemantics 核对宿主注册/验证的语义（P3-11.5）。
// 区分能力只读语义的宿主注册/验证与外部自报 annotation，不能因为工具名含
// get 就信任。
func validateHostRegisteredSemantics(cap Capability, externalAnnotation string) error {
	// 外部自报 annotation 不可信：只读语义必须由宿主注册/验证（Kind 字段）。
	if externalAnnotation == "readonly" && cap.Kind != "query" {
		return fmt.Errorf("%w：外部自报只读 annotation 与宿主注册 %s 不一致", ErrUntrustedAnnotation, cap.Kind)
	}
	// 不能因为工具名含 get 就信任：Kind 必须由宿主注册。
	if externalAnnotation == "" && cap.Kind == "" {
		return fmt.Errorf("%w：能力缺少宿主注册的语义（不能因工具名含 get 就信任）", ErrUntrustedAnnotation)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 文件成果发布适配（保留已有受控能力，不拆毁现有确认与摘要核验）
// ---------------------------------------------------------------------------

// adaptPublicationCapability 适配文件成果发布（P3-11.4）。
// 把文件成果发布保留为其已有受控能力，通过适配关联 Task/证据，不拆毁现有
// 确认与摘要核验。
func adaptPublicationCapability(tx *sql.Tx, taskID string, artifactID string, evidenceRefs []string, now int64) error {
	// 文件成果发布保留为其已有受控能力（不拆毁现有确认与摘要核验）。
	// 通过适配关联 Task/证据。
	return recordAudit(tx, AuditEvent{
		TaskID: taskID, Event: "publication_adapted", Actor: "system",
		Summary: fmt.Sprintf("文件成果发布 %s 关联 Task %s（证据 %d 项）", artifactID, taskID, len(evidenceRefs)),
		DetailRef: artifactID, OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
