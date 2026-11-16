package work

// P1-07：版本化 Wait 与事件唤醒。
//
// 本文件把人工、外部结果、依赖和时间等待变成独立持久记录：
//   - wait_id + wait_version；同一开放版本只能关闭一次；重复或旧事件可以
//     留证，不重复推进；
//   - OpenWait 与 Task/Step 状态变更同事务，等待不保留模型调用或执行工作者；
//   - ResolveWait 以 wait_id + version + open 条件更新；关闭、重设和控制
//     变化产生新版本/记录；
//   - 唤醒只生成需重新核对的就绪工作，不跳过当前合同、权限及未知操作检查；
//   - 旧 task_waits 人工等待转换为对应有限等待；无法恢复关联时保留缺口，
//     不制造已满足结果。

import (
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Wait 类型
// ---------------------------------------------------------------------------

// WaitKind 是等待类型。
type WaitKind string

const (
	WaitKindInput       WaitKind = "input"       // 人工输入
	WaitKindAcceptance  WaitKind = "acceptance"  // 人工验收
	WaitKindPublication WaitKind = "publication" // 文件发布
	WaitKindDependency  WaitKind = "dependency"  // 依赖
	WaitKindTimer       WaitKind = "timer"       // 时间
	WaitKindExternal    WaitKind = "external"    // 外部结果
)

// Wait 是 waits 的一行。
type Wait struct {
	WaitID        string
	TaskID        string
	StepID        string
	Kind          WaitKind
	MatchKey      string
	WaitVersion   int
	OpenedAt      int64
	DeadlineAt    int64
	TimeoutPolicy string // reconcile/notify/fail
	State         string // open/closed
	ClosedAt      int64
	Resolution    string
	EvidenceRef   string
	ClosedBy      string
}

// ---------------------------------------------------------------------------
// OpenWait（与 Task/Step 状态变更同事务）
// ---------------------------------------------------------------------------

// openWait 在事务内开启一个版本化 Wait（P1-07.2）。
// 等待不保留模型调用或执行工作者：它是独立的持久记录，唤醒由后续事件驱动。
func openWait(tx *sql.Tx, w Wait) (Wait, error) {
	if w.WaitID == "" {
		return Wait{}, errors.New("wait 缺少 wait_id")
	}
	if w.TaskID == "" {
		return Wait{}, errors.New("wait 缺少关联任务")
	}
	if w.Kind == "" {
		return Wait{}, errors.New("wait 缺少类型")
	}
	if w.WaitVersion < 1 {
		w.WaitVersion = 1
	}
	w.State = "open"
	var stepID any
	if w.StepID != "" {
		stepID = w.StepID
	}
	_, err := tx.Exec(`INSERT INTO waits(wait_id,task_id,step_id,kind,match_key,wait_version,
opened_at,deadline_at,timeout_policy,state)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		w.WaitID, w.TaskID, stepID, string(w.Kind), w.MatchKey, w.WaitVersion,
		w.OpenedAt, w.DeadlineAt, w.TimeoutPolicy, w.State)
	if err != nil {
		return Wait{}, err
	}
	return w, nil
}

// ---------------------------------------------------------------------------
// ResolveWait（wait_id + version + open 条件更新，只关闭一次）
// ---------------------------------------------------------------------------

// ResolveResult 是关闭结果。
type ResolveResult struct {
	Closed     bool   // 本次事件是否关闭了开放版本
	Wait       Wait   // 当前 Wait 状态
	Duplicate  bool   // 重复/旧事件（已留证但未关闭）
	Reason     string
}

// resolveWait 以 wait_id + version + open 条件更新关闭 Wait（P1-07.3）。
// 两个并发事件最多一个关闭开放版本，另一个仍可存证；旧版本事件、错误目标
// 或未授权决定不能关闭新等待。
func resolveWait(tx *sql.Tx, waitID string, version int, source, eventID, resolution, evidenceRef, closedBy string, now int64) (ResolveResult, error) {
	// 留证：重复或旧事件保留可查证据（不重复推进）。
	leaveEvidence := func(closedWait bool) error {
		_, err := tx.Exec(`INSERT INTO wait_events(wait_id,wait_version,source,event_id,closed_wait,summary,occurred_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(source,event_id,wait_id) DO NOTHING`,
			waitID, version, source, eventID, boolToInt(closedWait), resolution, now)
		return err
	}
	// 读取当前 Wait（同事务重新读取）。
	var w Wait
	var stepID sql.NullString
	err := tx.QueryRow(`SELECT wait_id,task_id,step_id,kind,match_key,wait_version,opened_at,deadline_at,
timeout_policy,state,closed_at,resolution,evidence_ref,closed_by FROM waits WHERE wait_id=?`, waitID).
		Scan(&w.WaitID, &w.TaskID, &stepID, &w.Kind, &w.MatchKey, &w.WaitVersion, &w.OpenedAt, &w.DeadlineAt,
			&w.TimeoutPolicy, &w.State, &w.ClosedAt, &w.Resolution, &w.EvidenceRef, &w.ClosedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ResolveResult{}, fmt.Errorf("wait %s 不存在", waitID)
	}
	if err != nil {
		return ResolveResult{}, err
	}
	w.StepID = stepID.String
	// 版本不匹配：旧版本事件不能关闭新等待（P1-07 反例）。
	if version != w.WaitVersion {
		if err := leaveEvidence(false); err != nil {
			return ResolveResult{}, err
		}
		return ResolveResult{Wait: w, Duplicate: true, Reason: fmt.Sprintf("版本不匹配：事件 %d，当前 %d", version, w.WaitVersion)}, nil
	}
	// 条件更新关闭开放版本（同一开放版本只能关闭一次）。
	res, err := tx.Exec(`UPDATE waits SET state='closed', closed_at=?, resolution=?, evidence_ref=?, closed_by=?
WHERE wait_id=? AND wait_version=? AND state='open'`,
		now, resolution, evidenceRef, closedBy, waitID, version)
	if err != nil {
		return ResolveResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ResolveResult{}, err
	}
	if n != 1 {
		// 已被其他事件关闭：留证但不重复推进。
		if err := leaveEvidence(false); err != nil {
			return ResolveResult{}, err
		}
		return ResolveResult{Wait: w, Duplicate: true, Reason: "开放版本已被其他事件关闭"}, nil
	}
	// 本次事件关闭了开放版本。
	if err := leaveEvidence(true); err != nil {
		return ResolveResult{}, err
	}
	w.State = "closed"
	w.ClosedAt = now
	w.Resolution = resolution
	return ResolveResult{Closed: true, Wait: w, Reason: "关闭开放版本"}, nil
}

// ---------------------------------------------------------------------------
// 重设（关闭旧版本 + 开启新版本）
// ---------------------------------------------------------------------------

// resetWait 关闭当前开放版本并以新版本重开（控制变化产生新版本/记录）。
// 旧版本事件在新版本上无效（不能再唤醒新等待）。
func resetWait(tx *sql.Tx, w Wait, now int64) (Wait, error) {
	// 关闭旧开放版本（若存在）。
	if _, err := tx.Exec(`UPDATE waits SET state='closed', closed_at=?, resolution='重设', closed_by='reset'
WHERE wait_id=? AND state='open'`, now, w.WaitID); err != nil {
		return Wait{}, err
	}
	// 新版本重开。
	newWait := w
	newWait.WaitVersion = w.WaitVersion + 1
	newWait.State = "open"
	newWait.OpenedAt = now
	newWait.ClosedAt = 0
	newWait.Resolution = ""
	// 用同一 wait_id 但新版本：需要先删除旧记录再插入（或更新版本号）。
	// 设计：wait_id 稳定，版本号区分——更新同一行而非新插入。
	if _, err := tx.Exec(`UPDATE waits SET wait_version=?, state='open', opened_at=?, closed_at=0,
resolution='', evidence_ref='', closed_by='', deadline_at=?, timeout_policy=? WHERE wait_id=?`,
		newWait.WaitVersion, now, newWait.DeadlineAt, newWait.TimeoutPolicy, w.WaitID); err != nil {
		return Wait{}, err
	}
	newWait.WaitID = w.WaitID
	return newWait, nil
}

// ---------------------------------------------------------------------------
// 唤醒（只生成需重新核对的就绪工作）
// ---------------------------------------------------------------------------

// wakeWait 唤醒一个已关闭的 Wait：只生成需重新核对的就绪工作，不跳过当前
// 合同、权限及未知操作检查（P1-07.4）。
// 返回需要重新核对的 Task ID 与后续命令；实际资格重评由 P1-03 的状态机执行。
func wakeWait(tx *sql.Tx, waitID string, now int64) (taskID string, followUp []string, err error) {
	var w Wait
	var stepID sql.NullString
	err = tx.QueryRow(`SELECT wait_id,task_id,step_id,kind,state FROM waits WHERE wait_id=?`, waitID).
		Scan(&w.WaitID, &w.TaskID, &stepID, &w.Kind, &w.State)
	if err != nil {
		return "", nil, err
	}
	w.StepID = stepID.String
	if w.State != "closed" {
		return "", nil, fmt.Errorf("wait %s 未关闭（%s），不能唤醒", waitID, w.State)
	}
	// 唤醒只产生"重新核对"的就绪工作：资格重评（合同/权限/未知操作检查）
	// 由调用方经状态机的 wait_resolved 事件执行。
	return w.TaskID, []string{"recheck_eligibility"}, nil
}

// ---------------------------------------------------------------------------
// 旧 task_waits 迁移适配（P1-07.5）
// ---------------------------------------------------------------------------

// migrateLegacyWait 把旧 task_waits 人工等待转换为对应有限等待。
// 无法恢复关联时保留缺口（返回 wait=nil, gap=原因），不制造已满足结果。
func migrateLegacyWait(tx *sql.Tx, runID string, kind string, openedAt int64, now int64) (w *Wait, gap string, err error) {
	// 恢复关联：旧 task_waits 以 run_id 关联，须找到对应 task。
	var taskID string
	err = tx.QueryRow("SELECT task_id FROM runs WHERE id=?", runID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Sprintf("旧等待的 run %s 不存在，无法恢复关联", runID), nil
	}
	if err != nil {
		return nil, "", err
	}
	if taskID == "" {
		return nil, fmt.Sprintf("旧等待的 run %s 无关联任务，保留缺口", runID), nil
	}
	// 转换为有限等待（wait_id 稳定：legacy:{run_id}:{kind}）。
	newWait := Wait{
		WaitID:        fmt.Sprintf("legacy:%s:%s", runID, kind),
		TaskID:        taskID,
		Kind:          WaitKind(kind),
		WaitVersion:   1,
		OpenedAt:      openedAt,
		TimeoutPolicy: "reconcile",
	}
	created, err := openWait(tx, newWait)
	if err != nil {
		return nil, "", err
	}
	return &created, "", nil
}

// readOpenWaits 读取一个 Task 的开放 Wait（查询模型）。
func readOpenWaits(q recordsQuerier, taskID string) ([]Wait, error) {
	rows, err := q.Query(`SELECT wait_id,task_id,step_id,kind,match_key,wait_version,opened_at,deadline_at,
timeout_policy,state,closed_at,resolution,evidence_ref,closed_by FROM waits WHERE task_id=? AND state='open'`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Wait
	for rows.Next() {
		var w Wait
		var stepID sql.NullString
		if err := rows.Scan(&w.WaitID, &w.TaskID, &stepID, &w.Kind, &w.MatchKey, &w.WaitVersion,
			&w.OpenedAt, &w.DeadlineAt, &w.TimeoutPolicy, &w.State, &w.ClosedAt,
			&w.Resolution, &w.EvidenceRef, &w.ClosedBy); err != nil {
			return nil, err
		}
		w.StepID = stepID.String
		out = append(out, w)
	}
	return out, rows.Err()
}
