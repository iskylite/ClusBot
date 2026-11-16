-- P1-07：版本化 Wait 与事件唤醒（V3 §5.1—5.3、F09）。
--
-- Wait 把人工、外部结果、依赖和时间等待变成独立持久记录：
--   - wait_id + wait_version；同一开放版本只能关闭一次；重复或旧事件可以
--     留证，不重复推进；
--   - OpenWait 与 Task/Step 状态变更同事务，等待不保留模型调用或执行工作者；
--   - ResolveWait 以 wait_id + version + open 条件更新；关闭、重设和控制
--     变化产生新版本/记录；
--   - 旧 task_waits 人工等待转换为对应有限等待；无法恢复关联时保留缺口，
--     不制造已满足结果。

CREATE TABLE IF NOT EXISTS waits (
  wait_id TEXT PRIMARY KEY,
  -- 关联（Task/Step 至少其一）。
  task_id TEXT NOT NULL,
  step_id TEXT,
  -- 类型与匹配键（input/acceptance/publication/dependency/timer/external）。
  kind TEXT NOT NULL,
  match_key TEXT NOT NULL DEFAULT '',
  -- 版本：关闭/重设/控制变化产生新版本。
  wait_version INTEGER NOT NULL DEFAULT 1,
  -- 开放/截止与超时策略。
  opened_at INTEGER NOT NULL,
  deadline_at INTEGER NOT NULL DEFAULT 0,
  timeout_policy TEXT NOT NULL DEFAULT 'reconcile', -- reconcile/notify/fail
  -- 状态：开放或关闭（同一开放版本只能关闭一次）。
  state TEXT NOT NULL CHECK(state IN ('open','closed')) DEFAULT 'open',
  closed_at INTEGER NOT NULL DEFAULT 0,
  -- 结果与证据引用（关闭时写入）。
  resolution TEXT NOT NULL DEFAULT '',
  evidence_ref TEXT NOT NULL DEFAULT '',
  -- 关闭者（事件 ID / 决定者）。
  closed_by TEXT NOT NULL DEFAULT ''
);

-- 开放 Wait 的匹配与竞争关闭索引。
CREATE INDEX IF NOT EXISTS waits_open ON waits(task_id, kind, state) WHERE state='open';
CREATE INDEX IF NOT EXISTS waits_match ON waits(kind, match_key, state) WHERE state='open';
CREATE INDEX IF NOT EXISTS waits_deadline ON waits(deadline_at, state) WHERE state='open' AND deadline_at>0;

-- Wait 事件留证：重复或旧事件保留可查证据，不重复推进。
CREATE TABLE IF NOT EXISTS wait_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  wait_id TEXT NOT NULL,
  wait_version INTEGER NOT NULL,
  -- 事件来源（inbox source/event_id）。
  source TEXT NOT NULL,
  event_id TEXT NOT NULL,
  -- 事件是否关闭了 Wait（只有一个能关闭开放版本）。
  closed_wait INTEGER NOT NULL DEFAULT 0 CHECK(closed_wait IN (0,1)),
  -- 留证内容。
  summary TEXT NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL,
  UNIQUE(source, event_id, wait_id)
);
