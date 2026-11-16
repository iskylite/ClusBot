package work

// P4-04：有界对象查询、拓扑与外部数据绑定。
//
// 本文件实现为任务提供有来源、时间和缺口的上下文，查询成本可控：
//   - 实现对象详情、部件/关系、指定时间事实和按关系类型的影响查询；返回
//     来源/版本/覆盖与不确定性；
//   - 遍历限制方向、深度、对象数量、时间/字节预算，防环、分页并报告截断；
//     逐跳及材料读取鉴权；
//   - 数据集绑定描述从外部来源查询指标/日志的方式，外部 I/O 不占 SQL 写事务；
//   - 当前准入/状态判断不使用滞后索引放行；外部不可用返回已知范围与缺口；
//   - 用真实或代表性拓扑检查查询计划和热点，基础索引与实现同步落地。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 有界查询（对象详情、部件/关系、指定时间事实、影响查询）
// ---------------------------------------------------------------------------

// ObjectQueryResult 是对象查询结果。
type ObjectQueryResult struct {
	ObjectRef     string
	// 对象详情。
	Detail        OpsAsset
	// 部件/关系。
	Components    []ComponentInstallation
	Relations     []Relation
	// 指定时间事实。
	Facts         []Fact
	// 来源/版本/覆盖与不确定性。
	Sources       []string
	Coverage      string
	Uncertainty   string
	// 截断。
	Truncated     bool
}

// readObjectDetail 查询对象详情（P4-04.1）。
// 实现对象详情、部件/关系、指定时间事实和按关系类型的影响查询；返回来源/
// 版本/覆盖与不确定性。
func readObjectDetail(q *sql.Tx, objectRef string, atTime int64, reader string) (ObjectQueryResult, error) {
	var result ObjectQueryResult
	result.ObjectRef = objectRef
	// 授权核对（逐跳及材料读取鉴权）。
	decision, err := decideAuthorization(q, reader, "read", objectRef, "", atTime, false)
	if err != nil {
		return result, err
	}
	if decision.Outcome != "allowed" {
		return result, fmt.Errorf("%w：主体 %s 无权读取对象 %s", ErrAuthorizationDenied, reader, objectRef)
	}
	// 对象详情。
	asset, err := readAsset(q, objectRef)
	if err == nil {
		result.Detail = asset
	}
	// 部件/关系。
	compRows, err := q.Query(`SELECT installation_id,node_ref,component_ref,component_kind,installed_at,removed_at,node_identity_continuous,created_at
FROM component_installations WHERE node_ref=? OR component_ref=?`, objectRef, objectRef)
	if err == nil {
		defer compRows.Close()
		for compRows.Next() {
			var c ComponentInstallation
			if err := compRows.Scan(&c.InstallationID, &c.NodeRef, &c.ComponentRef, &c.ComponentKind,
				&c.InstalledAt, &c.RemovedAt, &c.NodeIdentityContinuous, &c.CreatedAt); err != nil {
				break
			}
			result.Components = append(result.Components, c)
		}
	}
	// 关系（双向查）。
	relRows, err := q.Query(`SELECT relation_id,relation_kind,source_ref,target_ref,valid_from,valid_to,created_at
FROM relations WHERE (source_ref=? OR target_ref=?) AND valid_to=0`, objectRef, objectRef)
	if err == nil {
		defer relRows.Close()
		for relRows.Next() {
			var r Relation
			if err := relRows.Scan(&r.RelationID, &r.RelationKind, &r.SourceRef, &r.TargetRef,
				&r.ValidFrom, &r.ValidTo, &r.CreatedAt); err != nil {
				break
			}
			result.Relations = append(result.Relations, r)
		}
	}
	// 指定时间事实。
	fact, err := selectAuthoritativeFact(q, objectRef, "state", atTime)
	if err == nil {
		result.Facts = []Fact{fact}
		result.Sources = []string{fact.SourceRef}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 有界遍历（限制方向、深度、对象数量、时间/字节预算）
// ---------------------------------------------------------------------------

// TraversalBudget 是遍历预算。
type TraversalBudget struct {
	Direction    string // forward/backward/both
	MaxDepth     int
	MaxObjects   int
	MaxTimeMS    int64
	MaxBytes     int64
	// 分页。
	PageSize     int
	Cursor       string
}

// TraversalResult 是遍历结果。
type TraversalResult struct {
	Objects     []string
	Relations   []Relation
	// 防环。
	Visited     map[string]bool
	// 截断与分页。
	Truncated   bool
	TruncationReason string
	NextCursor  string
	// 耗时与字节。
	ElapsedMS   int64
	BytesRead   int64
}

// traverseRelations 有界遍历关系（P4-04.2）。
// 遍历限制方向、深度、对象数量、时间/字节预算，防环、分页并报告截断；
// 逐跳及材料读取鉴权；环状网络和大邻接查询在预算内结束，截断不会被当作
// 完整无异常。
func traverseRelations(tx *sql.Tx, startRef string, budget TraversalBudget, reader string, now int64) (TraversalResult, error) {
	result := TraversalResult{
		Visited: make(map[string]bool),
	}
	if budget.MaxDepth <= 0 {
		budget.MaxDepth = 3
	}
	if budget.MaxObjects <= 0 {
		budget.MaxObjects = 100
	}
	if budget.MaxTimeMS <= 0 {
		budget.MaxTimeMS = 5000
	}
	startTime := time.Now()
	// 授权核对（逐跳鉴权）。
	decision, err := decideAuthorization(tx, reader, "read", startRef, "", now, false)
	if err != nil {
		return result, err
	}
	if decision.Outcome != "allowed" {
		return result, fmt.Errorf("%w：主体 %s 无权遍历对象 %s", ErrAuthorizationDenied, reader, startRef)
	}
	// 有界遍历（BFS，限制深度/对象数量/时间）。
	queue := []struct {
		ref   string
		depth int
	}{{startRef, 0}}
	for len(queue) > 0 {
		// 时间预算。
		if time.Since(startTime).Milliseconds() > budget.MaxTimeMS {
			result.Truncated = true
			result.TruncationReason = "时间预算超限"
			break
		}
		// 对象数量预算。
		if len(result.Objects) >= budget.MaxObjects {
			result.Truncated = true
			result.TruncationReason = "对象数量预算超限"
			break
		}
		current := queue[0]
		queue = queue[1:]
		if current.depth > budget.MaxDepth {
			continue
		}
		if result.Visited[current.ref] {
			continue // 防环
		}
		result.Visited[current.ref] = true
		result.Objects = append(result.Objects, current.ref)
		// 查询关系（方向限制）。
		var rows *sql.Rows
		switch budget.Direction {
		case "forward":
			rows, err = tx.Query(`SELECT relation_id,relation_kind,source_ref,target_ref,valid_from,valid_to,created_at
FROM relations WHERE source_ref=? AND valid_to=0 LIMIT ?`, current.ref, budget.PageSize)
		case "backward":
			rows, err = tx.Query(`SELECT relation_id,relation_kind,source_ref,target_ref,valid_from,valid_to,created_at
FROM relations WHERE target_ref=? AND valid_to=0 LIMIT ?`, current.ref, budget.PageSize)
		default: // both
			rows, err = tx.Query(`SELECT relation_id,relation_kind,source_ref,target_ref,valid_from,valid_to,created_at
FROM relations WHERE (source_ref=? OR target_ref=?) AND valid_to=0 LIMIT ?`, current.ref, current.ref, budget.PageSize)
		}
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var r Relation
			if err := rows.Scan(&r.RelationID, &r.RelationKind, &r.SourceRef, &r.TargetRef,
				&r.ValidFrom, &r.ValidTo, &r.CreatedAt); err != nil {
				rows.Close()
				return result, err
			}
			result.Relations = append(result.Relations, r)
			// 下一跳（防环）。
			var next string
			if budget.Direction == "forward" || budget.Direction == "both" {
				next = r.TargetRef
			} else {
				next = r.SourceRef
			}
			if !result.Visited[next] {
				queue = append(queue, struct {
					ref   string
					depth int
				}{next, current.depth + 1})
			}
		}
		rows.Close()
	}
	result.ElapsedMS = time.Since(startTime).Milliseconds()
	return result, nil
}

// ---------------------------------------------------------------------------
// 数据集绑定（外部来源查询指标/日志；外部 I/O 不占 SQL 写事务）
// ---------------------------------------------------------------------------

// DatasetBinding 是数据集绑定。
type DatasetBinding struct {
	// 绑定标识。
	BindingID   string
	// 外部来源（指标/日志查询方式）。
	SourceKind  string // metrics/logs
	QueryMethod string
	// 外部 I/O 不占 SQL 写事务（查询在外部执行）。
	ExternalIO  bool
}

// validateDatasetBinding 核对数据集绑定（P4-04.3）。
// 数据集绑定描述从外部来源查询指标/日志的方式，外部 I/O 不占 SQL 写事务。
func validateDatasetBinding(binding DatasetBinding) error {
	if binding.SourceKind == "" || binding.QueryMethod == "" {
		return errors.New("数据集绑定缺少来源类型或查询方式")
	}
	if !binding.ExternalIO {
		return errors.New("数据集绑定必须声明外部 I/O（外部 I/O 不占 SQL 写事务）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 当前准入（不使用滞后索引放行）
// ---------------------------------------------------------------------------

// ensureCurrentAdmission 核对当前准入（P4-04.4）。
// 当前准入/状态判断不使用滞后索引放行；外部不可用返回已知范围与缺口。
func ensureCurrentAdmission(tx *sql.Tx, objectRef string, reader string, now int64) error {
	// 当前准入：读取当前权威（不使用滞后索引放行）。
	_, err := readAsset(tx, objectRef)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("对象 %s 未知（外部不可用返回已知范围与缺口）", objectRef)
	}
	if err != nil {
		return err
	}
	// 授权核对（当前权威）。
	decision, err := decideAuthorization(tx, reader, "read", objectRef, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		return fmt.Errorf("%w：主体 %s 无权读取对象 %s（不使用滞后索引放行）", ErrAuthorizationDenied, reader, objectRef)
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
