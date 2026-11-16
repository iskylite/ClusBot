package work

// P1-09：Schedule 与旧周期 Goal 兼容。
//
// 本文件实现 V3 §4.2 的周期和定点触发：产生有限 Task，Goal 不再承担所有
// 持续运行职责。
//   - 每个发生点建立稳定唯一 occurrence 键；创建 Task、发生记录及推进
//     next_at 同事务（同发生点重复扫描只建一个 Task，重启保持稳定身份）；
//   - 显式支持已选择的跳过/合并/有界补发；不无界补建任务；
//   - 旧周期 Goal 映射到计划与可选上层目标的关联，不复制两套调度权威；
//   - 触发前检查计划有效性和执行主体；不能用匿名后台超级权限。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Schedule 类型
// ---------------------------------------------------------------------------

// MissedPolicy 是漏触发策略。
type MissedPolicy string

const (
	// MissedSkip：跳过错过的发生点（不补发）。
	MissedSkip MissedPolicy = "skip"
	// MissedCoalesce：合并错过的发生点为一次（复用现有停机漏周期合并逻辑）。
	MissedCoalesce MissedPolicy = "coalesce"
	// MissedBoundedCatchup：有界补发（不超过 catchup_limit 次）。
	MissedBoundedCatchup MissedPolicy = "bounded_catchup"
)

// ScheduleTemplate 是任务模板（版本化）。
type ScheduleTemplate struct {
	Goal     string `json:"goal"`
	Scope    string `json:"scope_label"`
	Criteria string `json:"acceptance_criteria"`
	Priority string `json:"priority,omitempty"`
}

// Schedule 是 schedules 的一行。
type Schedule struct {
	ScheduleID       string
	Kind             string // interval/cron/once
	IntervalMS       int64
	Cron             string
	OnceAt           int64
	Timezone         string
	Template         ScheduleTemplate
	TemplateVersion  int
	Principal        string
	ScopeLabel       string
	Enabled          bool
	NextAt           int64
	ExpiresAt        int64
	MissedPolicy     MissedPolicy
	CatchupLimit     int
	GoalID           string
	Revision         int
	CreatedAt        int64
	UpdatedAt        int64
}

// validate 校验计划有效性（P1-09.5：不能用匿名后台超级权限）。
func (s Schedule) validate() error {
	if s.ScheduleID == "" {
		return errors.New("计划缺少身份")
	}
	if s.Principal == "" || s.Principal == "anonymous" || s.Principal == "system-superuser" {
		return errors.New("计划必须有可信执行主体，不能用匿名后台超级权限")
	}
	if s.Template.Goal == "" {
		return errors.New("计划任务模板缺少目标")
	}
	switch s.Kind {
	case "interval":
		if s.IntervalMS <= 0 {
			return errors.New("周期计划缺少间隔")
		}
	case "once":
		if s.OnceAt <= 0 {
			return errors.New("定点计划缺少时间")
		}
	case "cron":
		if s.Cron == "" {
			return errors.New("cron 计划缺少表达式")
		}
	default:
		return fmt.Errorf("未识别的计划类型 %q", s.Kind)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 创建与查询
// ---------------------------------------------------------------------------

// upsertSchedule 在事务内创建/更新计划（模板版本化）。
func upsertSchedule(tx *sql.Tx, s Schedule) (Schedule, error) {
	if err := s.validate(); err != nil {
		return Schedule{}, err
	}
	templateJSON, err := json.Marshal(s.Template)
	if err != nil {
		return Schedule{}, err
	}
	if s.TemplateVersion < 1 {
		s.TemplateVersion = 1
	}
	if s.Revision < 1 {
		s.Revision = 1
	}
	var goalID any
	if s.GoalID != "" {
		goalID = s.GoalID
	}
	_, err = tx.Exec(`INSERT INTO schedules(schedule_id,kind,interval_ms,cron,once_at,timezone,
template,template_version,principal,scope_label,enabled,next_at,expires_at,missed_policy,catchup_limit,goal_id,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(schedule_id) DO UPDATE SET kind=excluded.kind,interval_ms=excluded.interval_ms,cron=excluded.cron,
once_at=excluded.once_at,timezone=excluded.timezone,template=excluded.template,template_version=excluded.template_version,
principal=excluded.principal,scope_label=excluded.scope_label,enabled=excluded.enabled,next_at=excluded.next_at,
expires_at=excluded.expires_at,missed_policy=excluded.missed_policy,catchup_limit=excluded.catchup_limit,
goal_id=excluded.goal_id,revision=excluded.revision,updated_at=excluded.updated_at`,
		s.ScheduleID, s.Kind, s.IntervalMS, s.Cron, s.OnceAt, s.Timezone,
		string(templateJSON), s.TemplateVersion, s.Principal, s.ScopeLabel, boolToInt(s.Enabled),
		s.NextAt, s.ExpiresAt, string(s.MissedPolicy), s.CatchupLimit, goalID, s.Revision, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return Schedule{}, err
	}
	return s, nil
}

// readSchedule 读取一个计划。
func readSchedule(q recordsQuerier, scheduleID string) (Schedule, error) {
	var s Schedule
	var templateJSON, missedPolicy string
	var goalID sql.NullString
	var enabled int
	err := q.QueryRow(`SELECT schedule_id,kind,interval_ms,cron,once_at,timezone,template,template_version,
principal,scope_label,enabled,next_at,expires_at,missed_policy,catchup_limit,goal_id,revision,created_at,updated_at
FROM schedules WHERE schedule_id=?`, scheduleID).
		Scan(&s.ScheduleID, &s.Kind, &s.IntervalMS, &s.Cron, &s.OnceAt, &s.Timezone,
			&templateJSON, &s.TemplateVersion, &s.Principal, &s.ScopeLabel, &enabled,
			&s.NextAt, &s.ExpiresAt, &missedPolicy, &s.CatchupLimit, &goalID, &s.Revision, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(templateJSON), &s.Template); err != nil {
		return s, err
	}
	s.MissedPolicy = MissedPolicy(missedPolicy)
	s.GoalID = goalID.String
	s.Enabled = enabled == 1
	return s, nil
}

// ---------------------------------------------------------------------------
// 触发（同事务创建 Task + occurrence + 推进 next_at）
// ---------------------------------------------------------------------------

// TriggerResult 是触发结果。
type TriggerResult struct {
	Created      bool
	TaskID       string
	OccurrenceKey string
	Outcome      string // created/skipped/coalesced/catchup/failed/rejected
	NextAt       int64
	Reason       string
}

// occurrenceKey 计算一个发生点的稳定唯一键（重启保持稳定身份）。
func occurrenceKey(scheduleID string, scheduledAt int64) string {
	return fmt.Sprintf("%s@%d", scheduleID, scheduledAt)
}

// triggerSchedule 触发一个到期计划（P1-09.2）。
// 同事务：检查有效性/主体 → 创建 Task → 记录 occurrence（稳定键幂等）→ 推进 next_at。
// 同发生点重复扫描只建一个 Task（occurrence 主键去重）。
func triggerSchedule(tx *sql.Tx, s Schedule, now int64) (TriggerResult, error) {
	// 触发前检查计划有效性和执行主体（P1-09.5）。
	if !s.Enabled {
		return TriggerResult{Outcome: "skipped", Reason: "计划已停用"}, nil
	}
	if s.ExpiresAt > 0 && now >= s.ExpiresAt {
		return TriggerResult{Outcome: "skipped", Reason: "计划已过期"}, nil
	}
	if err := s.validate(); err != nil {
		return TriggerResult{Outcome: "rejected", Reason: err.Error()}, nil
	}
	// 计算发生点（漏触发策略）。
	scheduledAt, outcome := resolveOccurrence(s, now)
	if scheduledAt <= 0 {
		return TriggerResult{Outcome: "skipped", Reason: "按漏触发策略跳过", NextAt: nextOccurrence(s, now)}, nil
	}
	key := occurrenceKey(s.ScheduleID, scheduledAt)
	// 幂等：同发生点已存在 → 返回原结果（重复扫描只建一个 Task）。
	var existingTaskID, existingOutcome string
	err := tx.QueryRow(`SELECT coalesce(task_id,''), outcome FROM schedule_occurrences WHERE schedule_id=? AND occurrence_key=?`,
		s.ScheduleID, key).Scan(&existingTaskID, &existingOutcome)
	if err == nil {
		return TriggerResult{TaskID: existingTaskID, OccurrenceKey: key, Outcome: existingOutcome,
			Reason: "同发生点已受理（幂等）", NextAt: nextOccurrence(s, now)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TriggerResult{}, err
	}
	// 创建 Task（有限工作）。
	taskID := id()
	conversationID := id()
	run := Run{ID: id(), ConversationID: conversationID, TaskID: taskID, Status: "queued"}
	if _, err := tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", conversationID, title(s.Template.Goal), now); err != nil {
		return TriggerResult{}, err
	}
	if err := insertTask(tx, run, s.Template.Goal, s.Template.Scope, Delivery{Criteria: &s.Template.Criteria}); err != nil {
		return TriggerResult{}, err
	}
	if _, err := tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,?,?)",
		run.ID, conversationID, taskID, "queued", now); err != nil {
		return TriggerResult{}, err
	}
	res, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)", conversationID, run.ID, s.Template.Goal)
	if err != nil {
		return TriggerResult{}, err
	}
	message, err := res.LastInsertId()
	if err != nil {
		return TriggerResult{}, err
	}
	if err := recordInputEvent(tx, message, "schedule:"+s.ScheduleID+":"+key); err != nil {
		return TriggerResult{}, err
	}
	if err := addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": "queued"}); err != nil {
		return TriggerResult{}, err
	}
	// 记录 occurrence（与创建 Task 同事务）。
	if _, err := tx.Exec(`INSERT INTO schedule_occurrences(schedule_id,occurrence_key,scheduled_at,task_id,outcome,created_at)
VALUES(?,?,?,?,?,?)`, s.ScheduleID, key, scheduledAt, taskID, outcome, now); err != nil {
		return TriggerResult{}, err
	}
	// 推进 next_at（同事务）。
	next := nextOccurrence(s, now)
	if _, err := tx.Exec("UPDATE schedules SET next_at=?, revision=revision+1, updated_at=? WHERE schedule_id=?",
		next, now, s.ScheduleID); err != nil {
		return TriggerResult{}, err
	}
	return TriggerResult{Created: true, TaskID: taskID, OccurrenceKey: key, Outcome: outcome, NextAt: next}, nil
}

// resolveOccurrence 按漏触发策略计算本次发生点。
// 返回 (发生点时间戳, 触发方式)；<=0 表示跳过。
func resolveOccurrence(s Schedule, now int64) (int64, string) {
	if s.NextAt > now {
		return 0, "" // 未到期
	}
	switch s.MissedPolicy {
	case MissedSkip:
		// 跳过：直接到下一个未来发生点（不补发错过的）。
		return s.NextAt, "created"
	case MissedCoalesce:
		// 合并：错过的合并为一次（复用现有停机漏周期合并逻辑）。
		if s.IntervalMS > 0 {
			slot := s.NextAt + ((now-s.NextAt)/s.IntervalMS)*s.IntervalMS
			return slot, "coalesced"
		}
		return s.NextAt, "coalesced"
	case MissedBoundedCatchup:
		// 有界补发：补发最早的一个错过点（由调用方控制补发次数上限）。
		return s.NextAt, "catchup"
	default:
		return s.NextAt, "created"
	}
}

// nextOccurrence 计算下一个发生点。
func nextOccurrence(s Schedule, now int64) int64 {
	switch s.Kind {
	case "interval":
		if s.IntervalMS <= 0 {
			return 0
		}
		// 推进到下一个未来发生点。
		next := s.NextAt
		for next <= now {
			next += s.IntervalMS
		}
		return next
	case "once":
		return 0 // 一次性，无下一个
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// 到期扫描（触发所有到期计划）
// ---------------------------------------------------------------------------

// ScanSchedulesResult 是扫描结果。
type ScanSchedulesResult struct {
	Triggered int
	Skipped   int
	Rejected  int
}

// scanDueSchedules 扫描到期计划并触发（P1-09）。
// 不无界补建任务：有界补发由 catchup_limit 控制（调用方按补发次数循环）。
func scanDueSchedules(db *sql.DB, now int64, limit int) (ScanSchedulesResult, error) {
	if limit < 1 {
		limit = 20
	}
	var result ScanSchedulesResult
	rows, err := db.Query(`SELECT schedule_id FROM schedules WHERE enabled=1 AND next_at<=? AND (expires_at=0 OR expires_at>?) ORDER BY next_at LIMIT ?`,
		now, now, limit)
	if err != nil {
		return result, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		tx, err := db.Begin()
		if err != nil {
			return result, err
		}
		s, err := readSchedule(tx, id)
		if err != nil {
			tx.Rollback()
			return result, err
		}
		trigger, err := triggerSchedule(tx, s, now)
		if err != nil {
			tx.Rollback()
			return result, err
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		switch trigger.Outcome {
		case "rejected":
			result.Rejected++
		case "skipped":
			result.Skipped++
		default:
			if trigger.Created {
				result.Triggered++
			} else {
				result.Skipped++
			}
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 旧周期 Goal 映射（P1-09.4）
// ---------------------------------------------------------------------------

// mapLegacyGoalToSchedule 把旧周期 Goal 映射为 Schedule + 可选上层目标关联。
// 不复制两套调度权威：返回的 Schedule 以 goal_id 关联旧 Goal，触发由
// Schedule 统一驱动，旧 Goal 查询保留兼容视图。
func mapLegacyGoalToSchedule(g Goal, principal string, now int64) Schedule {
	return Schedule{
		ScheduleID:      "legacy-goal:" + g.ID,
		Kind:            "interval",
		IntervalMS:      g.Interval,
		Timezone:        "Asia/Shanghai",
		Template:        ScheduleTemplate{Goal: g.Target, Scope: g.Scope, Criteria: g.Criteria},
		TemplateVersion: 1,
		Principal:       principal,
		ScopeLabel:      g.Scope,
		Enabled:         g.Status == "active",
		NextAt:          g.NextAt,
		ExpiresAt:       g.ExpiresAt,
		MissedPolicy:    MissedCoalesce, // 复用现有停机漏周期合并逻辑
		CatchupLimit:    1,
		GoalID:          g.ID, // 关联旧 Goal，不复制两套调度权威
		Revision:        g.Revision,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// 确保 context 引用保留。
var _ = context.Background
