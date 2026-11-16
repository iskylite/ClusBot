-- P2-05：公平队列、背压与关键工作保留容量（V3 §5.6、F11/F12）。
--
-- 持续负载下独立任务有进展，回执、控制和核对不被新工作淹没：
--   - 按主体/来源与对象范围分稳定组，组间轮转、组内优先级与等待老化；
--     保存必要游标/计数以便恢复；
--   - 按 ready_at、组和状态建索引，分页扫描；不能每个 Worker 反复扫描所有
--     等待任务；
--   - 为回执、续租、取消和核对保留处理容量；新任务超限在持久受理确认前
--     返回可重试结果及请求键；
--   - 已确认受理的任务只能延期并暴露积压，不能因过载丢弃；重试退避有抖动
--     和上限；
--   - 按类别区分局部提供者失败与数据库全局不可写，避免无关 Task 永久被
--     一个异常结果阻塞。

CREATE TABLE IF NOT EXISTS scheduler_groups (
  -- 稳定组：按主体/来源与对象范围。
  group_key TEXT PRIMARY KEY,
  principal TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  scope_label TEXT NOT NULL DEFAULT '',
  -- 轮转游标与计数（恢复用）。
  round_robin_cursor INTEGER NOT NULL DEFAULT 0,
  total_claimed INTEGER NOT NULL DEFAULT 0,
  total_completed INTEGER NOT NULL DEFAULT 0,
  last_claimed_at INTEGER NOT NULL DEFAULT 0,
  -- 容量保留：为回执/续租/取消/核对保留的处理容量。
  reserved_capacity INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);

-- 就绪索引（按 ready_at、组和状态分页扫描）。
CREATE INDEX IF NOT EXISTS work_units_ready_group ON work_units(state, ready_at)
  WHERE state='ready';

-- 队列背压记录（受理确认前的可重试结果）。
CREATE TABLE IF NOT EXISTS scheduler_backpressure (
  request_key TEXT PRIMARY KEY,
  -- 可重试结果与请求键（新任务超限在持久受理确认前返回）。
  retry_after INTEGER NOT NULL,
  reason TEXT NOT NULL,
  rejected_at INTEGER NOT NULL,
  -- 类别：local_provider（局部提供者失败）/ global_db（数据库全局不可写）。
  category TEXT NOT NULL CHECK(category IN ('local_provider','global_db'))
);

-- 公平性观测（各组排队时间与持续进展）。
CREATE TABLE IF NOT EXISTS scheduler_fairness_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  group_key TEXT NOT NULL,
  unit_id TEXT NOT NULL,
  -- 入队到领取的等待时间（公平性证据）。
  queue_wait_ms INTEGER NOT NULL,
  claimed_at INTEGER NOT NULL,
  worker_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS fairness_by_group ON scheduler_fairness_log(group_key, claimed_at);
