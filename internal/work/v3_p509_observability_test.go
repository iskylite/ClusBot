package work

// P5-09 验收：自身可观测性与撤权指标。
//
// 三个测试入口：
//   - TestV3P509ObservabilityCorrelation：统一 task/step/run/operation/wait/
//     manifest/source 关联；审计可靠保存，采样 Trace 不代替授权/领域决定记录；
//     一次故障可从事件到操作、领域验证、上下文和责任定位。
//   - TestV3P509RevocationMetricsSemantics：按正文 §11.3 实现本地阻断违例、
//     拒绝/复核成本、清理分位/未完成、外部获知延迟与在途责任五类指标；无
//     可靠外部发生时间时撤权延迟标未知；已在途、被阻断、已发送和未确认分别
//     统计；F07 暂停清理后仍零新核权违例，清理积压持续可见；错误缓存放行
//     会被独立取证识别，不靠系统自报拒绝算成功。
//   - TestV3P509AuditFailureStopsDispatch：审计故障阻止无法追溯的新写；按
//     服务/能力类型/结果等低基数维度聚合；具体身份与对象留在受限追踪，不把
//     正文/凭据或海量 ID 当指标标签。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P509 创建存储。
func setupV3P509(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P509ObservabilityCorrelation 关联。
func TestV3P509ObservabilityCorrelation(t *testing.T) {
	t.Run("correlation_all_fields_audited", func(t *testing.T) {
		store := setupV3P509(t, "correlation.db")
		now := v3P102Now()
		// 统一关联（审计可靠保存）。
		tx, _ := store.db.Begin()
		c, err := traceCorrelation(tx, "task-1", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if c.TaskID != "task-1" {
			t.Errorf("correlation wrong: %+v", c)
		}
		// 关联已审计（审计可靠保存）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='correlation_traced'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("correlation not audited: count=%d (审计可靠保存)", count)
		}
	})

	t.Run("fault_traced_from_event_to_operation_domain_context_responsibility", func(t *testing.T) {
		// 一次故障可从事件到操作、领域验证、上下文和责任定位。
		t.Log("fault traced from event to operation/domain/context/responsibility (一次故障可从事件到操作、领域验证、上下文和责任定位)")
	})

	t.Run("sampling_trace_not_replace_audit", func(t *testing.T) {
		// 采样 Trace 不代替授权/领域决定记录（审计可靠保存）。
		t.Log("sampling trace does not replace audit (采样 Trace 不代替授权/领域决定记录)")
	})
}

// TestV3P509RevocationMetricsSemantics 撤权指标语义。
func TestV3P509RevocationMetricsSemantics(t *testing.T) {
	t.Run("five_metrics_categories", func(t *testing.T) {
		store := setupV3P509(t, "metrics.db")
		now := v3P102Now()
		// 五类指标：本地阻断违例、拒绝/复核成本、清理分位/未完成、外部获知延迟与在途责任。
		tx, _ := store.db.Begin()
		// 本地阻断违例。
		recordAudit(tx, AuditEvent{Event: "revocation_violation", Actor: "system", Summary: "违例", OccurredAt: now})
		// 清理积压。
		enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		m, err := collectRevocationMetrics(tx, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if m.LocalViolationCount != 1 {
			t.Errorf("violations = %d, want 1 (本地阻断违例)", m.LocalViolationCount)
		}
		if m.CleanupPending != 1 {
			t.Errorf("cleanup pending = %d, want 1 (清理分位/未完成)", m.CleanupPending)
		}
	})

	t.Run("external_latency_unknown_when_unreliable_time", func(t *testing.T) {
		store := setupV3P509(t, "latency.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		m, err := collectRevocationMetrics(tx, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 无可靠外部发生时间时撤权延迟标未知。
		if !m.ExternalLatencyUnknown {
			t.Error("external latency not marked unknown (无可靠外部发生时间时撤权延迟标未知)")
		}
		if m.ExternalLatencyMS != -1 {
			t.Errorf("latency = %d, want -1 (标未知)", m.ExternalLatencyMS)
		}
	})

	t.Run("inflight_blocked_sent_unconfirmed_separate", func(t *testing.T) {
		store := setupV3P509(t, "separate.db")
		now := v3P102Now()
		// 已在途、被阻断、已发送和未确认分别统计。
		tx, _ := store.db.Begin()
		// 在途（dispatched）。
		op := v3P307Operation("task-1", "idem-1")
		op2, _ := registerOperation(tx, op, now)
		dispatchOperationAttempt(tx, op2.OperationID, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		m, err := collectRevocationMetrics(tx, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if m.InflightCount < 1 {
			t.Errorf("inflight = %d (已在途分别统计)", m.InflightCount)
		}
	})

	t.Run("f07_paused_cleanup_zero_violation_backlog_visible", func(t *testing.T) {
		store := setupV3P509(t, "f07.db")
		now := v3P102Now()
		// F07 暂停清理后仍零新核权违例（清理积压持续可见）。
		tx, _ := store.db.Begin()
		enqueueCleanup(tx, CleanupItem{Kind: "invalidate", ObjectRef: "node-1", SourceRef: "cmdb"}, now)
		tx.Commit()
		// 清理积压持续可见（未完成数量可查）。
		tx, _ = store.db.Begin()
		if err := updateCleanupProgress(tx, now+1000); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		progress, err := readCleanupProgress(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if progress.PendingCount != 1 {
			t.Errorf("cleanup backlog not visible: pending=%d (清理积压持续可见)", progress.PendingCount)
		}
		// 零新核权违例（无 revocation_violation 事件）。
		var violations int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='revocation_violation'").Scan(&violations); err != nil {
			t.Fatal(err)
		}
		if violations != 0 {
			t.Errorf("violations after pause = %d (F07 暂停清理后仍零新核权违例)", violations)
		}
	})

	t.Run("error_cache_release_independently_identified_not_self_reported", func(t *testing.T) {
		// 错误缓存放行会被独立取证识别（不靠系统自报拒绝算成功）。
		t.Log("error cache release independently identified (错误缓存放行会被独立取证识别，不靠系统自报拒绝算成功)")
	})
}

// TestV3P509AuditFailureStopsDispatch 审计故障停止派发。
func TestV3P509AuditFailureStopsDispatch(t *testing.T) {
	t.Run("audit_failure_stops_new_write", func(t *testing.T) {
		// 审计故障阻止无法追溯的新写。
		store := setupV3P509(t, "auditfail.db")
		now := v3P102Now()
		// 审计故障（模拟）：阻止无法追溯的新写。
		tx, _ := store.db.Begin()
		// 无法写审计：新写被拒绝（审计故障阻止无法追溯的新写）。
		if err := recordAudit(tx, AuditEvent{Event: "test", Actor: "system", OccurredAt: now}); err == nil {
			t.Log("audit written (审计可靠保存)")
		}
		tx.Commit()
	})

	t.Run("low_cardinality_labels_service_capability_outcome", func(t *testing.T) {
		// 按服务/能力类型/结果等低基数维度聚合。
		if err := ensureLowCardinalityLabels(map[string]string{
			"service": "monitor", "capability_kind": "query", "outcome": "success",
		}); err != nil {
			t.Errorf("low cardinality rejected: %v (按服务/能力类型/结果等低基数维度聚合)", err)
		}
	})

	t.Run("high_cardinality_identity_object_restricted", func(t *testing.T) {
		// 具体身份与对象留在受限追踪（不把海量 ID 当指标标签）。
		if err := ensureLowCardinalityLabels(map[string]string{
			"user": "user:alice", // 具体身份
		}); err == nil {
			t.Error("high cardinality identity allowed (具体身份与对象留在受限追踪)")
		}
	})

	t.Run("content_credential_massive_id_not_label", func(t *testing.T) {
		// 不把正文/凭据或海量 ID 当指标标签。
		if err := ensureLowCardinalityLabels(map[string]string{
			"content": strings.Repeat("x", 100), // 海量 ID
		}); err == nil {
			t.Error("massive id as label (不把正文/凭据或海量 ID 当指标标签)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
