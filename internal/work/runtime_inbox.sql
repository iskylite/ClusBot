-- 通用 Inbox 与幂等应用（§5.3）。
--
-- Inbox 先可靠受理事件，再以单事务应用到当前权威状态：
--   - (source, event_id) 唯一；同 ID 同摘要返回原受理结果，同 ID 不同摘要
--     隔离并审计，不覆盖原事件；
--   - 应用事件时核对主体/关联/版本，在一个事务更新目标、审计、后续
--     Outbox 并标记已处理；
--   - 未知关联、不能判定顺序和处理失败保存明确原因与重试安排。

CREATE TABLE IF NOT EXISTS inbox_events (
  -- 受理身份：来源 + 事件 ID 唯一。
  source TEXT NOT NULL,
  event_id TEXT NOT NULL,
  -- 来源序号/时间（无全局可信顺序时按来源版本核对，不以最后到达覆盖）。
  source_seq INTEGER NOT NULL DEFAULT 0,
  source_version TEXT NOT NULL DEFAULT '',
  -- 可信主体（授权引用不能由请求方自证）。
  principal TEXT NOT NULL DEFAULT '',
  -- 关联（Task/Step/Run/Operation 等）；未知关联隔离待查，不丢弃。
  task_id TEXT,
  step_id TEXT,
  run_id TEXT,
  operation_id TEXT,
  wait_id TEXT,
  -- 内容摘要或材料引用（大内容不内嵌）。
  kind TEXT NOT NULL,           -- 事件类型（chat_input/report/receipt/timer/...）
  summary TEXT NOT NULL DEFAULT '',
  payload_digest TEXT NOT NULL DEFAULT '',
  payload_ref TEXT NOT NULL DEFAULT '',
  -- 接收与处理状态。
  received_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('received','applying','applied','quarantined','failed')) DEFAULT 'received',
  -- 处理结果与重试安排。
  applied_at INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  failure TEXT NOT NULL DEFAULT '',
  -- 同 ID 不同摘要的隔离原因。
  quarantine_reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (source, event_id)
);

-- 处理顺序与重试扫描索引。
CREATE INDEX IF NOT EXISTS inbox_pending ON inbox_events(state, next_retry_at)
  WHERE state IN ('received','failed');
CREATE INDEX IF NOT EXISTS inbox_by_task ON inbox_events(task_id, received_at);
