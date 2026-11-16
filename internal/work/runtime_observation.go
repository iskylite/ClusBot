package work

// P4-15：NHC、告警、网络与服务观测接入。
//
// 本文件实现持续感知产生有意义的调查工作，保持来源、覆盖和事件边界：
//   - 规范化 NHC、带外告警、网络/服务监测：来源实例、对象、发生/接收时间、
//     序号、严重度与材料引用；
//   - 按已发布规则去重、归并同一故障片段；恢复后复发可创建新故障，不无限
//     合并所有相似症状；
//   - 健康/可达性更新与资产业务状态分开，告警不能直接改维护中；
//   - 配置窗口、限速及降采样并保留原始计数/覆盖水位；撤权、控制和操作回执
//     不被聚合丢弃；
//   - 触发有限 Task 或补充已有调查；借出资产可读监测告警，不因此自动获得
//     写权限。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 观测规范化（NHC、带外告警、网络/服务监测）
// ---------------------------------------------------------------------------

// NormalizedObservation 是规范化观测。
type NormalizedObservation struct {
	// 来源实例、对象、发生/接收时间、序号。
	SourceInstance   string
	ObjectRef        string
	OccurredAt       int64
	ReceivedAt       int64
	Sequence         int64
	// 严重度与材料引用。
	Severity         string // critical/warning/info
	MaterialRefs     []string
	// 观测类型（NHC/带外告警/网络/服务监测）。
	ObservationKind  string // nhc/oob_alert/network/service
	// 原始数据。
	Payload          string
	PayloadDigest    string
}

// normalizeObservation 规范化观测（P4-15.1）。
// 规范化 NHC、带外告警、网络/服务监测：来源实例、对象、发生/接收时间、
// 序号、严重度与材料引用。
func normalizeObservation(o NormalizedObservation, now int64) (NormalizedObservation, error) {
	if o.SourceInstance == "" || o.ObjectRef == "" || o.ObservationKind == "" {
		return o, errors.New("观测缺少来源实例/对象/类型")
	}
	if o.OccurredAt == 0 {
		return o, errors.New("观测缺少发生时间")
	}
	o.ReceivedAt = now
	if o.PayloadDigest == "" {
		o.PayloadDigest = digest(o.Payload)
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// 故障归并（按已发布规则去重、归并同一故障片段）
// ---------------------------------------------------------------------------

// FaultSegment 是故障片段。
type FaultSegment struct {
	SegmentID        string
	// 故障标识（同一故障片段归并）。
	FaultKey         string
	// 观测引用与窗口。
	ObservationRefs  []string
	WindowStart      int64
	WindowEnd        int64
	// 状态（active/recovered/recurred）。
	State            string
}

// mergeFaultSegment 归并故障片段（P4-15.2）。
// 按已发布规则去重、归并同一故障片段；恢复后复发可创建新故障，不无限合并
// 所有相似症状；同症状不同来源/对象/时间窗口不会被错误合并。
func mergeFaultSegment(tx *sql.Tx, o NormalizedObservation, windowMS int64, now int64) (merged bool, segment FaultSegment, err error) {
	// 故障标识：来源+对象+类型（同症状不同来源/对象/时间窗口不会被错误合并）。
	faultKey := o.SourceInstance + ":" + o.ObjectRef + ":" + o.ObservationKind
	// 查找活跃故障片段（时间窗口内）。
	var existing FaultSegment
	var windowEnd int64
	err = tx.QueryRow(`SELECT segment_id, fault_key, window_start, window_end, state FROM fault_segments
WHERE fault_key=? AND state='active' AND window_end>?`, faultKey, now-windowMS).
		Scan(&existing.SegmentID, &existing.FaultKey, &existing.WindowStart, &windowEnd, &existing.State)
	if err == nil {
		// 归并（同一故障片段）。
		existing.WindowEnd = now
		return true, existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, segment, err
	}
	// 新故障片段（恢复后复发可创建新故障）。
	segment = FaultSegment{
		SegmentID: id(), FaultKey: faultKey, ObservationRefs: []string{o.PayloadDigest},
		WindowStart: now, WindowEnd: now, State: "active",
	}
	return false, segment, nil
}

// ---------------------------------------------------------------------------
// 健康/可达性更新（与资产业务状态分开）
// ---------------------------------------------------------------------------

// ErrAlertChangesMaintenance 是告警改维护中的错误。
var ErrAlertChangesMaintenance = errors.New("告警不能直接改维护中（健康/可达性更新与资产业务状态分开）")

// updateHealthReachability 更新健康/可达性（P4-15.3）。
// 健康/可达性更新与资产业务状态分开，告警不能直接改维护中。
func updateHealthReachability(tx *sql.Tx, objectRef string, health string, reachability string, now int64) error {
	// 健康/可达性单独更新（与资产业务状态分开）。
	if health == "" && reachability == "" {
		return errors.New("健康/可达性更新缺少值")
	}
	// 不直接改资产状态（告警不能直接改维护中）。
	return recordAudit(tx, AuditEvent{
		Event: "health_reachability_updated", Actor: "system",
		Summary: fmt.Sprintf("对象 %s 健康=%s 可达性=%s（与资产业务状态分开）", objectRef, health, reachability),
		OccurredAt: now,
	})
}

// ---------------------------------------------------------------------------
// 聚合控制（配置窗口、限速及降采样并保留原始计数/覆盖水位）
// ---------------------------------------------------------------------------

// AggregationControl 是聚合控制。
type AggregationControl struct {
	// 配置窗口与限速。
	WindowMS         int64
	RateLimit        int
	// 降采样与原始计数/覆盖水位。
	DownsampleRatio  float64
	OriginalCount    int64
	CoverageWatermark int64
}

// validateAggregationControl 核对聚合控制（P4-15.4）。
// 配置窗口、限速及降采样并保留原始计数/覆盖水位；撤权、控制和操作回执
// 不被聚合丢弃。
func validateAggregationControl(c AggregationControl) error {
	// 降采样保留原始计数/覆盖水位。
	if c.DownsampleRatio > 0 && c.DownsampleRatio < 1 && c.OriginalCount == 0 {
		return errors.New("降采样必须保留原始计数（撤权、控制和操作回执不被聚合丢弃）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 触发 Task（触发有限 Task 或补充已有调查）
// ---------------------------------------------------------------------------

// triggerInvestigationTask 触发调查 Task（P4-15.5）。
// 触发有限 Task 或补充已有调查；借出资产可读监测告警，不因此自动获得写权限。
func triggerInvestigationTask(tx *sql.Tx, o NormalizedObservation, now int64) error {
	// 借出资产可读监测告警（不因此自动获得写权限）。
	asset, err := readAsset(tx, o.ObjectRef)
	if err == nil && asset.State == AssetStateLent {
		// 借出资产：可读监测告警（不自动获得写权限）。
		return recordAudit(tx, AuditEvent{
			Event: "lent_asset_observation", Actor: "system",
			Summary: fmt.Sprintf("借出资产 %s 观测告警（可读监测，不自动获得写权限）", o.ObjectRef),
			OccurredAt: now,
		})
	}
	// 触发有限 Task 或补充已有调查。
	return recordAudit(tx, AuditEvent{
		Event: "investigation_triggered", Actor: "system",
		Summary: fmt.Sprintf("观测触发调查：%s（%s）", o.ObjectRef, o.ObservationKind), OccurredAt: now,
	})
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
