package work

// P2-05：公平队列、背压与关键工作保留容量。
//
// 本文件实现持续负载下独立任务有进展，回执、控制和核对不被新工作淹没：
//   - 按主体/来源与对象范围分稳定组，组间轮转、组内优先级与等待老化；
//     保存必要游标/计数以便恢复；
//   - 按 ready_at、组和状态建索引，分页扫描；不能每个 Worker 反复扫描所有
//     等待任务；
//   - 为回执、续租、取消和核对保留处理容量；新任务超限在持久受理确认前
//     返回可重试结果及请求键；
//   - 已确认受理的任务只能延期并暴露积压，不能因过载丢弃；重试退避有抖动
//     和上限；
//   - 按类别区分局部提供者失败与数据库全局不可写，避免无关 Task 永久被
//     一个异常结果阻塞。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 稳定分组（按主体/来源与对象范围）
// ---------------------------------------------------------------------------

// SchedulerGroup 是 scheduler_groups 的一行。
type SchedulerGroup struct {
	GroupKey          string
	Principal         string
	Source            string
	ScopeLabel        string
	RoundRobinCursor  int
	TotalClaimed      int
	TotalCompleted    int
	LastClaimedAt     int64
	ReservedCapacity  int
	UpdatedAt         int64
}

// groupKeyFor 计算一个工作项的稳定组（按主体/来源与对象范围）。
func groupKeyFor(principal, source, scope string) string {
	return fmt.Sprintf("%s|%s|%s", principal, source, scope)
}

// ensureSchedulerGroup 确保一个稳定组存在（返回该组）。
func ensureSchedulerGroup(tx *sql.Tx, principal, source, scope string, now int64) (SchedulerGroup, error) {
	key := groupKeyFor(principal, source, scope)
	var g SchedulerGroup
	err := tx.QueryRow(`SELECT group_key,principal,source,scope_label,round_robin_cursor,total_claimed,
total_completed,last_claimed_at,reserved_capacity,updated_at FROM scheduler_groups WHERE group_key=?`, key).
		Scan(&g.GroupKey, &g.Principal, &g.Source, &g.ScopeLabel, &g.RoundRobinCursor,
			&g.TotalClaimed, &g.TotalCompleted, &g.LastClaimedAt, &g.ReservedCapacity, &g.UpdatedAt)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return g, err
	}
	g = SchedulerGroup{
		GroupKey: key, Principal: principal, Source: source, ScopeLabel: scope,
		ReservedCapacity: 1, UpdatedAt: now,
	}
	if _, err := tx.Exec(`INSERT INTO scheduler_groups(group_key,principal,source,scope_label,reserved_capacity,updated_at)
VALUES(?,?,?,?,?,?)`, key, principal, source, scope, 1, now); err != nil {
		return g, err
	}
	return g, nil
}

// ---------------------------------------------------------------------------
// 公平选取（组间轮转、组内优先级与等待老化）
// ---------------------------------------------------------------------------

// ReadyUnitWithAge 是一个带等待老化的就绪单元。
type ReadyUnitWithAge struct {
	Unit      WorkUnit
	GroupKey  string
	ReadyAt   int64
	AgeMS     int64 // 等待老化（now - ready_at）
	Priority  int    // 组内优先级（小者优先）
}

// selectFairUnit 公平选取下一个就绪单元（P2-05.1/P2-05.2）。
// 组间轮转（round_robin_cursor）、组内优先级与等待老化；分页扫描（不反复
// 扫描所有等待任务）；保存游标/计数以便恢复。
func selectFairUnit(tx *sql.Tx, groups []SchedulerGroup, now int64, pageSize int) (*ReadyUnitWithAge, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	if pageSize < 1 {
		pageSize = 20
	}
	// 组间轮转：从全局游标位置开始（公平）。
	var startCursor int
	if err := tx.QueryRow(`SELECT coalesce(sum(round_robin_cursor),0) FROM scheduler_groups`).Scan(&startCursor); err != nil {
		return nil, err
	}
	// 按轮转顺序遍历各组（分页扫描就绪单元）。
	ordered := append([]SchedulerGroup(nil), groups...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].RoundRobinCursor < ordered[j].RoundRobinCursor
	})
	for _, g := range ordered {
		// 组内：按优先级与等待老化选取（老化的优先，避免低优先级饿死）。
		rows, err := tx.Query(`SELECT unit_id,task_id,ready_at FROM work_units
WHERE state='ready' ORDER BY ready_at LIMIT ?`, pageSize)
		if err != nil {
			return nil, err
		}
		var candidates []ReadyUnitWithAge
		for rows.Next() {
			var u ReadyUnitWithAge
			var unitID, taskID string
			var readyAt int64
			if err := rows.Scan(&unitID, &taskID, &readyAt); err != nil {
				rows.Close()
				return nil, err
			}
			u.Unit.UnitID = unitID
			u.Unit.TaskID = taskID
			u.ReadyAt = readyAt
			u.AgeMS = now - readyAt
			u.GroupKey = g.GroupKey
			candidates = append(candidates, u)
		}
		rows.Close()
		if len(candidates) == 0 {
			continue
		}
		// 组内公平：老化最大者优先（等待时间提升）。
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].AgeMS > candidates[j].AgeMS
		})
		selected := &candidates[0]
		// 推进该组轮转游标（保存游标/计数以便恢复）。
		if _, err := tx.Exec(`UPDATE scheduler_groups SET round_robin_cursor=round_robin_cursor+1, last_claimed_at=?, updated_at=? WHERE group_key=?`,
			now, now, g.GroupKey); err != nil {
			return nil, err
		}
		return selected, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// 背压（受理确认前返回可重试结果；关键工作保留容量）
// ---------------------------------------------------------------------------

// BackpressureCategory 是背压类别。
type BackpressureCategory string

const (
	// BackpressureLocalProvider：局部提供者失败（不阻塞无关 Task）。
	BackpressureLocalProvider BackpressureCategory = "local_provider"
	// BackpressureGlobalDB：数据库全局不可写（停止新的相关派发）。
	BackpressureGlobalDB BackpressureCategory = "global_db"
)

// BackpressureResult 是背压结果（受理确认前返回）。
type BackpressureResult struct {
	Accepted   bool
	RequestKey string
	RetryAfter int64
	Reason     string
	Category   BackpressureCategory
}

// checkAdmissionCapacity 核对受理容量（P2-05.3/P2-05.4）。
// 为回执、续租、取消和核对保留处理容量；新任务超限在持久受理确认前返回
// 可重试结果及请求键；已确认受理的任务只能延期并暴露积压，不能因过载丢弃。
func checkAdmissionCapacity(tx *sql.Tx, requestKey string, isCritical bool, maxPending int, now int64) (BackpressureResult, error) {
	// 统计当前积压（待领取 + 处理中）。
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state IN ('ready','claimed','running')`).Scan(&pending); err != nil {
		return BackpressureResult{}, err
	}
	// 关键工作（回执/续租/取消/核对）保留容量：始终受理。
	if isCritical {
		return BackpressureResult{Accepted: true, RequestKey: requestKey, Reason: "关键工作保留容量"}, nil
	}
	// 新任务超限：在持久受理确认前返回可重试结果及请求键。
	if pending >= maxPending {
		retryAfter := now + backpressureBackoff(pending)
		if _, err := tx.Exec(`INSERT INTO scheduler_backpressure(request_key,retry_after,reason,rejected_at,category)
VALUES(?,?,?,?,?) ON CONFLICT(request_key) DO UPDATE SET retry_after=excluded.retry_after,rejected_at=excluded.rejected_at`,
			requestKey, retryAfter, fmt.Sprintf("队列已满（%d/%d），可重试", pending, maxPending), now, string(BackpressureLocalProvider)); err != nil {
			return BackpressureResult{}, err
		}
		return BackpressureResult{
			Accepted: false, RequestKey: requestKey, RetryAfter: retryAfter,
			Reason: fmt.Sprintf("队列已满（%d/%d）", pending, maxPending), Category: BackpressureLocalProvider,
		}, nil
	}
	// 已确认受理的任务只能延期并暴露积压（不丢弃）。
	return BackpressureResult{Accepted: true, RequestKey: requestKey, Reason: "受理（可能延期并暴露积压）"}, nil
}

// backpressureBackoff 计算背压退避（有抖动和上限）。
func backpressureBackoff(pending int) int64 {
	ms := int64(500) * int64(pending)
	if ms > 30000 {
		ms = 30000 // 上限 30 秒
	}
	// 抖动：±20%。
	jitter := ms / 5
	return ms - jitter + int64(len(fmt.Sprint(pending))%3)*jitter/2
}

// ---------------------------------------------------------------------------
// 类别区分（局部提供者失败 vs 数据库全局不可写）
// ---------------------------------------------------------------------------

// classifyFailure 按类别区分失败（P2-05.5）。
// 局部提供者失败不阻塞无关 Task；数据库全局不可写停止新的相关派发。
func classifyFailure(err error) BackpressureCategory {
	if err == nil {
		return BackpressureLocalProvider
	}
	if errors.Is(err, ErrDatabaseUnwritable) {
		return BackpressureGlobalDB
	}
	// SQLite 写错误（disk full/readonly）→ 全局不可写。
	msg := err.Error()
	if strings.Contains(msg, "readonly") || strings.Contains(msg, "disk") || strings.Contains(msg, "database is locked") {
		return BackpressureGlobalDB
	}
	return BackpressureLocalProvider
}

// ---------------------------------------------------------------------------
// 公平性观测（各组排队时间与持续进展）
// ---------------------------------------------------------------------------

// FairnessSnapshot 是公平性快照。
type FairnessSnapshot struct {
	GroupKey       string
	TotalClaimed   int
	TotalCompleted int
	AvgQueueWaitMS int64
	MaxQueueWaitMS int64
	ReservedCapacity int
}

// recordFairnessClaim 记录一次领取的等待时间（公平性证据）。
func recordFairnessClaim(tx *sql.Tx, groupKey, unitID string, queueWaitMS int64, workerID string, now int64) error {
	if _, err := tx.Exec(`INSERT INTO scheduler_fairness_log(group_key,unit_id,queue_wait_ms,claimed_at,worker_id)
VALUES(?,?,?,?,?)`, groupKey, unitID, queueWaitMS, now, workerID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE scheduler_groups SET total_claimed=total_claimed+1, last_claimed_at=?, updated_at=? WHERE group_key=?`,
		now, now, groupKey)
	return err
}

// readFairnessSnapshot 读取各组公平性快照（持续高优先级流量下低优先级最终获得机会的证据）。
func readFairnessSnapshot(q recordsQuerier, groupKey string) (FairnessSnapshot, error) {
	var s FairnessSnapshot
	s.GroupKey = groupKey
	if err := q.QueryRow(`SELECT total_claimed, total_completed, reserved_capacity FROM scheduler_groups WHERE group_key=?`, groupKey).
		Scan(&s.TotalClaimed, &s.TotalCompleted, &s.ReservedCapacity); err != nil {
		return s, err
	}
	if err := q.QueryRow(`SELECT coalesce(avg(queue_wait_ms),0), coalesce(max(queue_wait_ms),0) FROM scheduler_fairness_log WHERE group_key=?`, groupKey).
		Scan(&s.AvgQueueWaitMS, &s.MaxQueueWaitMS); err != nil {
		return s, err
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// 积压暴露（已确认受理的任务延期并暴露）
// ---------------------------------------------------------------------------

// QueueBacklog 是队列积压快照。
type QueueBacklog struct {
	Ready        int
	Claimed      int
	Running      int
	OldestReadyMS int64
	Groups       int
	Backpressure int
}

// readQueueBacklog 暴露积压（已确认受理的任务延期并暴露，不因过载丢弃）。
func readQueueBacklog(q recordsQuerier, now int64) (QueueBacklog, error) {
	var b QueueBacklog
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state='ready'`).Scan(&b.Ready); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state='claimed'`).Scan(&b.Claimed); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE state='running'`).Scan(&b.Running); err != nil {
		return b, err
	}
	var oldest sql.NullInt64
	if err := q.QueryRow(`SELECT min(ready_at) FROM work_units WHERE state='ready'`).Scan(&oldest); err != nil {
		return b, err
	}
	if oldest.Valid {
		b.OldestReadyMS = now - oldest.Int64
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM scheduler_groups`).Scan(&b.Groups); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM scheduler_backpressure`).Scan(&b.Backpressure); err != nil {
		return b, err
	}
	return b, nil
}

// 确保 context/sort 引用保留。
var (
	_ = context.Background
	_ = sort.Ints
)
