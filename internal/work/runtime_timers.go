package work

// P1-08：持久 Timer、逾期扫描与时间边界。
//
// 本文件实现 V3 §5.3 的持久计时器：控制面停机后仍能识别所有应处理的等待，
// 避免旧计时器唤醒新工作。
//   - Timer 绑定 wait_id 与版本，在同一等待创建/重设事务记录；
//   - 扫描到期项时用条件更新标记已投递并创建唯一 Inbox 到期事件；处理事件
//     再竞争关闭 Wait；
//   - 控制面时间统一裁决；不用远端 Worker 墙钟直接判租约/等待；
//   - 恢复扫描分批处理逾期项，限制每批/每轮耗时，保留水位和最老年龄；
//   - Timeout 的后续行为由等待策略决定：继续核对、通知或失败收尾，不能一律
//     视为外部动作失败。
//
// 关键约束：Timer 必须写 SQLite，不用 goroutine sleep 保存唯一唤醒责任；
// Timer 已投递不等于 Task 已唤醒；Wait 重设后旧 Timer 无效。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Timer 类型
// ---------------------------------------------------------------------------

// Timer 是 timers 的一行。
type Timer struct {
	TimerID      string
	WaitID       string
	WaitVersion  int
	DueAt        int64
	State        string // pending/delivered/cancelled
	DeliveredAt  int64
	InboxEventID string
	CreatedAt    int64
}

// ---------------------------------------------------------------------------
// 创建（与等待创建/重设同事务）
// ---------------------------------------------------------------------------

// createTimer 在同一等待创建/重设事务内登记 Timer（P1-08.1）。
// wait_id + wait_version 唯一：重设 Wait 后旧 Timer 因版本不匹配自然无效。
func createTimer(tx *sql.Tx, t Timer) (Timer, error) {
	if t.TimerID == "" || t.WaitID == "" {
		return Timer{}, errors.New("timer 缺少身份或绑定")
	}
	if t.WaitVersion < 1 {
		return Timer{}, errors.New("timer 缺少 wait_version")
	}
	if t.DueAt <= 0 {
		return Timer{}, errors.New("timer 缺少到期时间")
	}
	t.State = "pending"
	_, err := tx.Exec(`INSERT INTO timers(timer_id,wait_id,wait_version,due_at,state,created_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(wait_id,wait_version) DO UPDATE SET due_at=excluded.due_at`,
		t.TimerID, t.WaitID, t.WaitVersion, t.DueAt, t.State, t.CreatedAt)
	if err != nil {
		return Timer{}, err
	}
	return t, nil
}

// cancelTimer 取消绑定的 Timer（Wait 关闭/重设时）。
// 旧 Timer 在新 Wait 版本上无效（不能再唤醒新等待）。
func cancelTimer(tx *sql.Tx, waitID string, version int) error {
	_, err := tx.Exec(`UPDATE timers SET state='cancelled' WHERE wait_id=? AND wait_version<>? AND state='pending'`,
		waitID, version)
	return err
}

// ---------------------------------------------------------------------------
// 逾期扫描（分批 + 条件更新标记已投递 + 唯一 Inbox 事件）
// ---------------------------------------------------------------------------

// ScanResult 是扫描结果。
type TimerScanResult struct {
	Delivered       int   // 本轮投递数
	Skipped         int   // 并发竞争/已处理跳过数
	OldestPendingAt int64 // 最老未处理年龄（观测水位）
	ProcessedUpTo   int64 // 本轮处理到的到期时间水位
}

// scanDueTimers 分批扫描到期 Timer，用条件更新标记已投递并创建唯一 Inbox
// 到期事件（P1-08.2）。处理事件再竞争关闭 Wait（由 Inbox 应用路径完成）。
// 控制面时间统一裁决（now 由控制面时钟提供，不用远端 Worker 墙钟）。
func scanDueTimers(db *sql.DB, now int64, batchSize int, maxDuration time.Duration) (TimerScanResult, error) {
	return scanTimers(db, now, batchSize, maxDuration, false)
}

func scanTimers(db *sql.DB, now int64, batchSize int, maxDuration time.Duration, repairLegacy bool) (TimerScanResult, error) {
	if batchSize < 1 {
		batchSize = 50
	}
	deadline := time.Now().Add(maxDuration)
	var result TimerScanResult
	filter := "state='pending'"
	if repairLegacy {
		// Only during startup: keep the periodic scan on the pending index.
		filter = "(state='pending' OR (state='delivered' AND inbox_event_id=''))"
	}
	// 已交付行从查询中移除。不要把到期时间当分页游标：一批以上
	// 的 Timer 可以在同一毫秒到期。
	for time.Now().Before(deadline) {
		rows, err := db.Query(`SELECT timer_id,wait_id,wait_version,due_at FROM timers
WHERE due_at<=? AND `+filter+`
ORDER BY due_at,timer_id LIMIT ?`, now, batchSize)
		if err != nil {
			return result, err
		}
		var batch []Timer
		for rows.Next() {
			var t Timer
			if err := rows.Scan(&t.TimerID, &t.WaitID, &t.WaitVersion, &t.DueAt); err != nil {
				rows.Close()
				return result, err
			}
			batch = append(batch, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			break
		}
		for _, t := range batch {
			if !time.Now().Before(deadline) {
				break
			}
			delivered, err := deliverTimer(db, t, now)
			if err != nil {
				return result, fmt.Errorf("投递 Timer %s: %w", t.TimerID, err)
			}
			if !delivered {
				result.Skipped++
				continue
			}
			result.ProcessedUpTo = max(result.ProcessedUpTo, t.DueAt)
			result.Delivered++
		}
	}
	var oldest sql.NullInt64
	if err := db.QueryRow(`SELECT min(due_at) FROM timers WHERE ` + filter).Scan(&oldest); err != nil {
		return result, err
	}
	if oldest.Valid {
		result.OldestPendingAt = oldest.Int64
	}
	_, err := db.Exec(`INSERT INTO timer_scan_state(id,watermark_due_at,last_scan_at,oldest_pending_at) VALUES(1,?,?,?)
ON CONFLICT(id) DO UPDATE SET watermark_due_at=max(timer_scan_state.watermark_due_at,excluded.watermark_due_at),
last_scan_at=excluded.last_scan_at,oldest_pending_at=excluded.oldest_pending_at`,
		result.ProcessedUpTo, now, result.OldestPendingAt)
	return result, err
}

// deliverTimer 将标记与到期事件放入一个写入事务。之前版本可能在两步
// 之间崩溃，留下 delivered 但无 inbox_event_id；用同一稳定键补齐它。
func deliverTimer(db *sql.DB, t Timer, now int64) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	eventID := "timer:" + t.TimerID
	res, err := tx.Exec(`UPDATE timers SET state='delivered',delivered_at=?,inbox_event_id=?
WHERE timer_id=? AND wait_id=? AND wait_version=? AND due_at=? AND due_at<=?
AND (state='pending' OR (state='delivered' AND inbox_event_id=''))`,
		now, eventID, t.TimerID, t.WaitID, t.WaitVersion, t.DueAt, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	received, err := receiveInbox(tx, InboxEvent{
		Source: "timer", EventID: eventID, Principal: "system", Kind: "timer_due",
		WaitID: t.WaitID, Summary: fmt.Sprintf("Timer %s 到期（wait %s v%d）", t.TimerID, t.WaitID, t.WaitVersion),
		// 保留 V3 摘要口径，以便可重放既有事件。
		PayloadDigest: digest(t.TimerID + t.WaitID), ReceivedAt: now,
	})
	if err != nil {
		return false, err
	}
	if received.Quarantined || received.Event.Kind != "timer_due" || received.Event.Principal != "system" ||
		received.Event.WaitID != t.WaitID || received.Event.State == "quarantined" {
		return false, errors.New("到期事件身份冲突；未标记已投递")
	}
	return true, tx.Commit()
}

// ---------------------------------------------------------------------------
// 恢复扫描（停机跨多个到期点，不丢事件、不重复建后续动作）
// ---------------------------------------------------------------------------

// recoverDueTimers 恢复扫描：停机跨多个到期点后，逐批处理逾期项。
// 幂等：已投递的 Timer 不重复投递（state 条件），重复事件由 Inbox 去重。
func recoverDueTimers(db *sql.DB, now int64, batchSize int) (TimerScanResult, error) {
	// 恢复扫描不限单轮耗时（逐步处理全部逾期），但保留分批与水位。
	result, err := scanTimers(db, now, batchSize, 30*time.Second, true)
	if err == nil {
		var remaining bool
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM timers WHERE state='delivered' AND inbox_event_id='' AND due_at<=?)`, now).Scan(&remaining)
		if err == nil && remaining {
			err = errors.New("旧版 Timer 仍有未补齐的到期事件；重启可继续恢复")
		}
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Timeout 策略（P1-08.5）
// ---------------------------------------------------------------------------

// TimeoutAction 是超时的后续行为。
type TimeoutAction string

const (
	// TimeoutReconcile：继续核对（不一律视为外部动作失败）。
	TimeoutReconcile TimeoutAction = "reconcile"
	// TimeoutNotify：通知（人工决定）。
	TimeoutNotify TimeoutAction = "notify"
	// TimeoutFail：失败收尾（由等待策略显式决定）。
	TimeoutFail TimeoutAction = "fail"
)

// timeoutAction 返回一个到期 Wait 的后续行为（由等待策略决定）。
// 不能一律视为外部动作失败：默认继续核对。
func timeoutAction(wait Wait) TimeoutAction {
	switch wait.TimeoutPolicy {
	case "notify":
		return TimeoutNotify
	case "fail":
		return TimeoutFail
	default:
		return TimeoutReconcile
	}
}

// ---------------------------------------------------------------------------
// 观测（P1-08.4）
// ---------------------------------------------------------------------------

// TimerBacklog 是 Timer 的运行时查询快照。
type TimerBacklog struct {
	Pending         int   // 待处理数
	Delivered       int   // 已投递未唤醒数
	OldestPendingAt int64 // 最老待处理到期时间
	WatermarkDueAt  int64 // 扫描水位
}

// readTimerBacklog 读取 Timer 积压与水位（恢复扫描不丢事件的可观测证据）。
func readTimerBacklog(q recordsQuerier) (TimerBacklog, error) {
	var b TimerBacklog
	if err := q.QueryRow(`SELECT COUNT(*) FROM timers WHERE state='pending' OR (state='delivered' AND inbox_event_id='')`).Scan(&b.Pending); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM timers t WHERE state='delivered' AND inbox_event_id<>'' AND EXISTS(SELECT 1 FROM inbox_events e WHERE e.source='timer' AND e.event_id=t.inbox_event_id AND e.state IN ('received','applying','failed'))`).Scan(&b.Delivered); err != nil {
		return b, err
	}
	var oldest sql.NullInt64
	if err := q.QueryRow(`SELECT min(due_at) FROM timers WHERE state='pending' OR (state='delivered' AND inbox_event_id='')`).Scan(&oldest); err != nil {
		return b, err
	}
	if oldest.Valid {
		b.OldestPendingAt = oldest.Int64
	}
	var watermark sql.NullInt64
	if err := q.QueryRow(`SELECT watermark_due_at FROM timer_scan_state WHERE id=1`).Scan(&watermark); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	if watermark.Valid {
		b.WatermarkDueAt = watermark.Int64
	}
	return b, nil
}

// 确保 context/time 引用保留。
var (
	_ = context.Background
	_ = time.Now
)
