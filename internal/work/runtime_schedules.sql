-- P1-09：Schedule 与旧周期 Goal 兼容（V3 §4.2、§5.3、F09）。
--
-- 周期和定点触发产生有限 Task，Goal 不再承担所有持续运行职责：
--   - Schedule 的时区/时间、任务模板版本、主体、范围、有效期、停用和漏触发策略；
--   - 每个发生点建立稳定唯一 occurrence 键；创建 Task、发生记录及推进
--     next_at 同事务；
--   - 复用当前停机漏周期合并逻辑，显式支持已选择的跳过/合并/有界补发；
--     不无界补建任务；
--   - 旧周期 Goal 映射到计划与可选上层目标的关联，不复制两套调度权威；
--   - 触发前检查计划有效性和执行主体；不能用匿名后台超级权限。

CREATE TABLE IF NOT EXISTS schedules (
  schedule_id TEXT PRIMARY KEY,
  -- 触发：周期或具体时间 + 时区。
  kind TEXT NOT NULL CHECK(kind IN ('interval','cron','once')),
  interval_ms INTEGER NOT NULL DEFAULT 0,
  cron TEXT NOT NULL DEFAULT '',
  once_at INTEGER NOT NULL DEFAULT 0,
  timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  -- 任务模板（含版本）。
  template TEXT NOT NULL,             -- JSON：目标/范围/验收/优先级等
  template_version INTEGER NOT NULL DEFAULT 1,
  -- 执行主体与范围（不能用匿名后台超级权限）。
  principal TEXT NOT NULL,
  scope_label TEXT NOT NULL DEFAULT '',
  -- 有效期与停用。
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  next_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 漏触发策略：skip（跳过）/coalesce（合并为一次）/bounded_catchup（有界补发）。
  missed_policy TEXT NOT NULL DEFAULT 'coalesce' CHECK(missed_policy IN ('skip','coalesce','bounded_catchup')),
  catchup_limit INTEGER NOT NULL DEFAULT 1,  -- 有界补发上限
  -- 可选上层目标关联（旧 Goal 映射，不复制两套调度权威）。
  goal_id TEXT,
  -- 版本（乐观并发）。
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 到期扫描索引。
CREATE INDEX IF NOT EXISTS schedules_due ON schedules(enabled, next_at) WHERE enabled=1;

-- 发生记录：每个发生点稳定唯一键，创建 Task 的同事务写入（幂等）。
CREATE TABLE IF NOT EXISTS schedule_occurrences (
  schedule_id TEXT NOT NULL REFERENCES schedules(schedule_id) ON DELETE CASCADE,
  occurrence_key TEXT NOT NULL,       -- 稳定唯一键（计划发生点）
  -- 发生时间与创建的 Task。
  scheduled_at INTEGER NOT NULL,
  task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
  -- 触发时核对结果（成功/跳过/补发/失败）。
  outcome TEXT NOT NULL CHECK(outcome IN ('created','skipped','coalesced','catchup','failed','rejected')) DEFAULT 'created',
  detail TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (schedule_id, occurrence_key)
);

CREATE INDEX IF NOT EXISTS occurrences_by_task ON schedule_occurrences(task_id);
