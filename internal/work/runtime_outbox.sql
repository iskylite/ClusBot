-- P1-06：Outbox 与受控命令接收端（V3 §5.3）。
--
-- Outbox 保证状态变化后的工作请求不会因崩溃丢失，同时不把命令重投变成
-- 副作用重做：
--   - 命令记录 command_id、目标/版本、payload 摘要、后端、状态、尝试数和
--     下次投递时间；业务事务同步创建；
--   - 提交后投递；接收端以 command_id/内容去重，返回原受理结果；确认丢失
--     按同键有界退避重投；
--   - 去重记录覆盖重投期和操作生命周期；未知最大重投期保留摘要/墓碑，
--     不能随清理删除保证。

CREATE TABLE IF NOT EXISTS outbox_commands (
  -- 唯一命令身份。
  command_id TEXT PRIMARY KEY,
  -- 类型与目标（含版本）。
  kind TEXT NOT NULL,               -- dispatch/notify/schedule/...
  target_kind TEXT NOT NULL DEFAULT '', -- work_unit/operation/wait/...
  target_id TEXT NOT NULL DEFAULT '',
  target_version INTEGER NOT NULL DEFAULT 0,
  -- 关联（便于核对与查询）。
  task_id TEXT,
  unit_id TEXT,
  operation_id TEXT,
  -- 载荷摘要/引用（大内容不内嵌）。
  payload_digest TEXT NOT NULL DEFAULT '',
  payload_ref TEXT NOT NULL DEFAULT '',
  -- 调度后端与投递状态。
  backend TEXT NOT NULL DEFAULT '',     -- worker/adapter/...
  state TEXT NOT NULL CHECK(state IN ('pending','dispatched','acknowledged','failed','dead')) DEFAULT 'pending',
  -- 尝试与下次投递。
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  -- 时间。
  created_at INTEGER NOT NULL,
  dispatched_at INTEGER NOT NULL DEFAULT 0,
  acknowledged_at INTEGER NOT NULL DEFAULT 0,
  -- 失败原因（不可恢复冲突单独标记为 dead）。
  failure TEXT NOT NULL DEFAULT ''
);

-- 投递扫描索引（到期 + 状态）。
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox_commands(state, next_attempt_at)
  WHERE state IN ('pending','failed');
CREATE INDEX IF NOT EXISTS outbox_by_task ON outbox_commands(task_id, created_at);

-- 接收端去重记录：覆盖重投期和操作生命周期；未知最大重投期保留摘要/墓碑。
CREATE TABLE IF NOT EXISTS outbox_receipts (
  command_id TEXT NOT NULL,
  -- 接收端身份（同一接收端内去重）。
  receiver TEXT NOT NULL,
  payload_digest TEXT NOT NULL,
  -- 受理结果（重复投递返回原受理结果）。
  receipt TEXT NOT NULL DEFAULT '',
  received_at INTEGER NOT NULL,
  -- 墓碑：保留期未知时长期保留，不随清理删除保证。
  tombstone INTEGER NOT NULL DEFAULT 0 CHECK(tombstone IN (0,1)),
  PRIMARY KEY (command_id, receiver)
);
