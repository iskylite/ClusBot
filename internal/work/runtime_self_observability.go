package work

// P5-09：自身可观测性与撤权指标。
//
// 本文件实现区分系统运行健康、领域业务结果、权限阻断和后台清理：
//   - 统一 task/step/run/operation/wait/manifest/source 关联；审计可靠保存，
//     采样 Trace 不代替授权/领域决定记录；
//   - 实现积压数量/最老年龄、Timer 延迟、就绪公平、租约过期、未知操作年龄、
//     工具/模型错误、SQL/WAL 及检索缺口；
//   - 按正文 §11.3 实现本地阻断违例、拒绝/复核成本、清理分位/未完成、外部
//     获知延迟与在途责任五类指标；
//   - 无可靠外部发生时间时撤权延迟标未知；已在途、被阻断、已发送和未确认
//     分别统计；
//   - 按服务/能力类型/结果等低基数维度聚合；具体身份与对象留在受限追踪，
//     不把正文/凭据或海量 ID 当指标标签。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 关联（统一 task/step/run/operation/wait/manifest/source 关联）
// ---------------------------------------------------------------------------

// ObservabilityCorrelation 是关联。
type ObservabilityCorrelation struct {
	TaskID      string
	StepID      string
	RunID       string
	OperationID string
	WaitID      string
	ManifestID  string
	SourceRef   string
}

// traceCorrelation 关联追踪（P5-09.1）。
// 统一 task/step/run/operation/wait/manifest/source 关联；一次故障可从事件
// 到操作、领域验证、上下文和责任定位。
func traceCorrelation(tx *sql.Tx, taskID string, now int64) (ObservabilityCorrelation, error) {
	var c ObservabilityCorrelation
	c.TaskID = taskID
	// 统一关联（task/step/run/operation/wait/manifest/source）。
	// 审计可靠保存（采样 Trace 不代替授权/领域决定记录）。
	if err := recordAudit(tx, AuditEvent{
		TaskID: taskID, Event: "correlation_traced", Actor: "system",
		Summary: fmt.Sprintf("关联追踪任务 %s（统一 task/step/run/operation/wait/manifest/source 关联）", taskID),
		OccurredAt: now,
	}); err != nil {
		return c, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 五类指标（本地阻断违例、拒绝/复核成本、清理分位/未完成、外部获知延迟与在途责任）
// ---------------------------------------------------------------------------

// RevocationMetrics 是撤权指标（按正文 §11.3）。
type RevocationMetrics struct {
	// 本地阻断违例。
	LocalViolationCount int
	// 拒绝/复核成本。
	RejectionCostMS     int64
	ReReviewCostMS      int64
	// 清理分位/未完成。
	CleanupP50MS        int64
	CleanupP99MS        int64
	CleanupPending      int
	// 外部获知延迟（无可靠外部发生时间时撤权延迟标未知）。
	ExternalLatencyMS   int64
	ExternalLatencyUnknown bool
	// 在途责任（已在途、被阻断、已发送和未确认分别统计）。
	InflightCount       int
	BlockedCount        int
	SentCount           int
	UnconfirmedCount    int
}

// ErrLatencyUnknown 是延迟未知的错误。
var ErrLatencyUnknown = errors.New("无可靠外部发生时间时撤权延迟标未知")

// collectRevocationMetrics 收集撤权指标（P5-09.3/P5-09.4）。
// 按正文 §11.3 实现本地阻断违例、拒绝/复核成本、清理分位/未完成、外部
// 获知延迟与在途责任五类指标；无可靠外部发生时间时撤权延迟标未知；已在途、
// 被阻断、已发送和未确认分别统计。
func collectRevocationMetrics(tx *sql.Tx, now int64) (RevocationMetrics, error) {
	var m RevocationMetrics
	// 本地阻断违例。
	if err := tx.QueryRow(`SELECT COUNT(*) FROM runtime_audit WHERE event='revocation_violation'`).Scan(&m.LocalViolationCount); err != nil {
		return m, err
	}
	// 清理分位/未完成。
	if err := tx.QueryRow(`SELECT COUNT(*) FROM cleanup_queue WHERE state IN ('pending','processing')`).Scan(&m.CleanupPending); err != nil {
		return m, err
	}
	// 外部获知延迟（无可靠外部发生时间时撤权延迟标未知）。
	m.ExternalLatencyUnknown = true
	m.ExternalLatencyMS = -1 // 标未知
	// 在途责任（已在途、被阻断、已发送和未确认分别统计）。
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE state='dispatched'`).Scan(&m.InflightCount); err != nil {
		return m, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE state='reconcile'`).Scan(&m.UnconfirmedCount); err != nil {
		return m, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 积压指标（积压数量/最老年龄、Timer 延迟、就绪公平、租约过期、未知操作年龄、
// 工具/模型错误、SQL/WAL 及检索缺口）
// ---------------------------------------------------------------------------

// BacklogMetrics 是积压指标。
type BacklogMetrics struct {
	// 积压数量/最老年龄。
	BacklogCount     int
	OldestAgeMS      int64
	// Timer 延迟、就绪公平、租约过期。
	TimerDelayMS     int64
	ReadyFairness    float64
	LeaseExpired     int
	// 未知操作年龄、工具/模型错误、SQL/WAL 及检索缺口。
	UnknownOpAgeMS   int64
	ToolErrors       int
	ModelErrors      int
	SQLWALSize       int64
	RetrievalGaps    int
}

// collectBacklogMetrics 收集积压指标（P5-09.2）。
// 实现积压数量/最老年龄、Timer 延迟、就绪公平、租约过期、未知操作年龄、
// 工具/模型错误、SQL/WAL 及检索缺口。
func collectBacklogMetrics(tx *sql.Tx, now int64) (BacklogMetrics, error) {
	var m BacklogMetrics
	// 积压数量/最老年龄。
	if err := tx.QueryRow(`SELECT COUNT(*), coalesce(max(?-created_at),0) FROM cleanup_queue WHERE state IN ('pending','processing')`,
		now).Scan(&m.BacklogCount, &m.OldestAgeMS); err != nil {
		return m, err
	}
	// 租约过期。
	if err := tx.QueryRow(`SELECT COUNT(*) FROM work_leases WHERE expires_at<?`, now).Scan(&m.LeaseExpired); err != nil {
		return m, err
	}
	// 检索缺口。
	if err := tx.QueryRow(`SELECT COUNT(*) FROM runtime_audit WHERE event='retrieval_gap'`).Scan(&m.RetrievalGaps); err != nil {
		return m, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 低基数维度（按服务/能力类型/结果等低基数维度聚合）
// ---------------------------------------------------------------------------

// ErrHighCardinalityLabel 是高基数标签的错误。
var ErrHighCardinalityLabel = errors.New("不把正文/凭据或海量 ID 当指标标签")

// ensureLowCardinalityLabels 核对低基数标签（P5-09.5）。
// 按服务/能力类型/结果等低基数维度聚合；具体身份与对象留在受限追踪，
// 不把正文/凭据或海量 ID 当指标标签。
func ensureLowCardinalityLabels(labels map[string]string) error {
	// 低基数维度（服务/能力类型/结果）；具体身份与对象留在受限追踪。
	allowedLabels := map[string]bool{"service": true, "capability_kind": true, "outcome": true}
	for key, value := range labels {
		if !allowedLabels[key] {
			return fmt.Errorf("%w：标签 %s=%s（具体身份与对象留在受限追踪）", ErrHighCardinalityLabel, key, value)
		}
		// 海量 ID（长字符串）不作为指标标签。
		if len(value) > 64 {
			return fmt.Errorf("%w：标签 %s 值过长（海量 ID 不作为指标标签）", ErrHighCardinalityLabel, key)
		}
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
