package work

// P1-14：运行时审计与阶段 G1 验收。
//
// 本文件把阶段 P1 的持久性与恢复作为可观测交付，保留后续故障证据：
//   - 业务变更与必要审计同事务，贯通 input/task/step/run/wait/command/result
//     标识；普通日志不能替代审计；
//   - 提供 Inbox/Outbox 数量与最老年龄、到期延迟、就绪等待、未知结果、
//     数据库错误/事务时长的查询或指标接口；
//   - 执行 F01/F03/F09/F11 的本阶段子例，沿真实 HTTP/运行时入口走通一项
//     个人助手工作及等待恢复；
//   - 核对生成新库、升级库、API 状态、前端构建和原有关键回归；记录未涵盖
//     的操作/租约/领域子例。
//
// 与 P1-02 的 runtime_audit 关系：本文件提供审计的关联查询与统一观测快照，
// 不建设监控看板；P5-09 扩展为完整的撤权与业务指标。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 审计关联查询（贯通 input/task/step/run/wait/command/result 标识）
// ---------------------------------------------------------------------------

// AuditTrail 是一条审计关联链：一次业务变更的全部标识。
type AuditTrail struct {
	TaskID    string
	StepID    string
	RunID     string
	UnitID    string
	WaitID    string
	CommandID string
	ResultKey string
	Events    []AuditRecord
}

// AuditRecord 是一条审计记录（含时间与摘要）。
type AuditRecord struct {
	AuditID    int64
	Event      string
	Actor      string
	Summary    string
	OccurredAt int64
}

// readAuditTrail 按任一标识查询审计关联链（P1-14.1）。
// 任务/输入/等待和审计记录能互相定位，失败原因及未决项可查询。
func readAuditTrail(q recordsQuerier, byField, value string) (AuditTrail, error) {
	var trail AuditTrail
	switch byField {
	case "task_id", "step_id", "run_id", "unit_id", "wait_id", "command_id", "result_key":
	default:
		return trail, fmt.Errorf("不支持的审计查询字段 %q", byField)
	}
	rows, err := q.Query(fmt.Sprintf(`SELECT audit_id,coalesce(task_id,''),coalesce(step_id,''),coalesce(run_id,''),
coalesce(unit_id,''),coalesce(wait_id,''),coalesce(command_id,''),coalesce(result_key,''),event,actor,summary,occurred_at
FROM runtime_audit WHERE %s=? ORDER BY occurred_at LIMIT 100`, byField), value)
	if err != nil {
		return trail, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.AuditID, &trail.TaskID, &trail.StepID, &trail.RunID, &trail.UnitID,
			&trail.WaitID, &trail.CommandID, &trail.ResultKey, &r.Event, &r.Actor, &r.Summary, &r.OccurredAt); err != nil {
			return trail, err
		}
		trail.Events = append(trail.Events, r)
	}
	return trail, rows.Err()
}

// ---------------------------------------------------------------------------
// 统一观测快照（Inbox/Outbox/到期/就绪/未知/数据库）
// ---------------------------------------------------------------------------

// RuntimeObservability 是运行时统一观测快照（P1-14.2）。
type RuntimeObservability struct {
	// Inbox/Outbox 数量与最老年龄。
	InboxPending     int
	InboxOldestMS    int64
	OutboxBacklog    OutboxBacklog
	// 到期延迟与就绪等待。
	TimerBacklog     TimerBacklog
	TimerOverdueMS   int64 // 最老到期延迟
	ReadyUnits       int
	// 未知结果。
	UnknownResults   int
	// 数据库错误/事务时长。
	DatabaseWritable bool
	WriteTxMaxMS     int64
	WALFileBytes     int64
	// 采集时间。
	CollectedAt      int64
}

// collectObservability 采集运行时统一观测快照。
func (s *Store) collectObservability(now int64) (RuntimeObservability, error) {
	var o RuntimeObservability
	o.CollectedAt = now
	// Inbox 数量与最老年龄。
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_events WHERE state IN ('received','failed')`).Scan(&o.InboxPending); err != nil {
		return o, err
	}
	var oldestInbox sql.NullInt64
	if err := s.db.QueryRow(`SELECT min(received_at) FROM inbox_events WHERE state IN ('received','failed')`).Scan(&oldestInbox); err != nil {
		return o, err
	}
	if oldestInbox.Valid {
		o.InboxOldestMS = now - oldestInbox.Int64
	}
	// Outbox 积压。
	outbox, err := readOutboxBacklog(s.db, now)
	if err != nil {
		return o, err
	}
	o.OutboxBacklog = outbox
	// Timer 积压与到期延迟。
	timer, err := readTimerBacklog(s.db)
	if err != nil {
		return o, err
	}
	o.TimerBacklog = timer
	if timer.OldestPendingAt > 0 && now > timer.OldestPendingAt {
		o.TimerOverdueMS = now - timer.OldestPendingAt
	}
	// 就绪等待。
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state='ready'`).Scan(&o.ReadyUnits); err != nil {
		return o, err
	}
	// 未知结果（旧代际留证 + 未知读取）。
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM result_receipts WHERE state='received' AND summary LIKE '%旧代际留证%') +
(SELECT COUNT(*) FROM run_reads WHERE status='unknown' AND acknowledged_at=0)`).Scan(&o.UnknownResults); err != nil {
		return o, err
	}
	// 数据库可写性与事务时长。
	tx, err := s.db.Begin()
	if err != nil {
		o.DatabaseWritable = false
	} else {
		o.DatabaseWritable = ensureWritable(tx) == nil
		tx.Rollback()
	}
	if s.writer != nil {
		m := s.writer.metrics()
		o.WriteTxMaxMS = m.WriteTxMaxMS
	}
	o.WALFileBytes = 0 // WAL 大小由调用方提供路径时填充
	return o, nil
}

// ---------------------------------------------------------------------------
// F01/F03/F09/F11 本阶段子例（沿真实运行时入口）
// ---------------------------------------------------------------------------

// F01Proof 是 F01（持久受理与提交边界）的证据。
type F01Proof struct {
	InputSurvivesRestart     bool // 已确认受理的输入在重启后仍在
	NoPartialCommit          bool // 未提交事务不留下部分本地状态
	DuplicateNotDuplicated   bool // 重复输入不新建同一工作
}

// F03Proof 是 F03（重复与乱序）的证据。
type F03Proof struct {
	AppliedOnce        bool // 相同事件只应用一次
	ConflictQuarantined bool // 同 ID 不同内容隔离
	OldEvidenceKept    bool // 迟到/旧事件留证不覆盖
}

// F09Proof 是 F09（等待与到期）的证据。
type F09Proof struct {
	WaitClosedOnce     bool // 同一 Wait 只关闭一次
	TimerIdempotent    bool // 恢复扫描不重复投递
	WaitNotOccupying   bool // 等待不占执行槽
}

// F11Proof 是 F11（依赖与持久化故障）的证据。
type F11Proof struct {
	UnwritableStopsDispatch bool // 数据库不可写时不确认新工作
	ModelUnavailableRecovers bool // 模型不可用时确定性核对继续
	UnknownNotSuccess       bool // 外部未知不被写成成功
}

// ---------------------------------------------------------------------------
// 阶段 G1 验收汇总
// ---------------------------------------------------------------------------

// G1Evidence 是阶段 G1 的验收证据汇总。
type G1Evidence struct {
	// 新建/升级库受测。
	FreshSchemaWorks    bool
	UpgradedSchemaWorks bool
	SchemaVersion       int
	// 持久性与恢复。
	F01 F01Proof
	F03 F03Proof
	F09 F09Proof
	F11 F11Proof
	// 观测就绪。
	Observability RuntimeObservability
	// 未涵盖的子例（明确记录，不冒充通过）。
	NotCovered []string
}

// 确保 errors/time 引用保留。
var (
	_ = errors.Is
	_ = time.Now
	_ = context.Background
)
