-- 持久 Timer、逾期扫描与时间边界（§5.3、F09/F12）。
--
-- Timer 绑定 wait_id 与版本，控制面停机后仍能识别所有应处理的等待，
-- 避免旧计时器唤醒新工作：
--   - 在同一等待创建/重设事务记录；建立到期和状态索引；
--   - 扫描到期项时用条件更新标记已投递并创建唯一 Inbox 到期事件；处理事件
--     再竞争关闭 Wait；
--   - 控制面时间统一裁决；不用远端 Worker 墙钟直接判租约/等待；
--   - 恢复扫描分批处理逾期项，限制每批/每轮耗时，保留水位和最老年龄；
--   - Timer 已投递不等于 Task 已唤醒；旧 Timer 在 Wait 重设后无效。

CREATE TABLE IF NOT EXISTS timers (
  timer_id TEXT PRIMARY KEY,
  -- 绑定的 Wait 与版本（wait_id + wait_version 唯一）。
  wait_id TEXT NOT NULL,
  wait_version INTEGER NOT NULL,
  -- 到期时间与状态。
  due_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('pending','delivered','cancelled')) DEFAULT 'pending',
  -- 已投递不等于已唤醒：投递时创建唯一 Inbox 到期事件。
  delivered_at INTEGER NOT NULL DEFAULT 0,
  inbox_event_id TEXT NOT NULL DEFAULT '',
  -- 创建时间。
  created_at INTEGER NOT NULL,
  UNIQUE(wait_id, wait_version)
);

-- 到期扫描索引（分批逾期处理）。
CREATE INDEX IF NOT EXISTS timers_due ON timers(due_at, state) WHERE state='pending';

-- 扫描水位：恢复扫描分批处理，保留水位与最老年龄（不一次全塞内存）。
CREATE TABLE IF NOT EXISTS timer_scan_state (
  id INTEGER PRIMARY KEY CHECK(id=1),
  -- 上次扫描到的到期时间水位。
  watermark_due_at INTEGER NOT NULL DEFAULT 0,
  -- 上次扫描时间。
  last_scan_at INTEGER NOT NULL DEFAULT 0,
  -- 最老未处理年龄（观测）。
  oldest_pending_at INTEGER NOT NULL DEFAULT 0
);
