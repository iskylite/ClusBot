-- A2+A3：只读领域查询任务（资产验收状态查询）。
--
-- 记录每个只读领域查询任务的请求键（幂等）与关联的 Task。
-- 任务本体（task_states/steps/work_units）与结果（result_receipts）复用 结构。

CREATE TABLE IF NOT EXISTS domain_query_tasks (
  -- 请求键（幂等：同键返回同一任务）。
  request_key TEXT PRIMARY KEY,
  -- 关联的 Task。
  task_id TEXT NOT NULL,
  -- 查询目标（资产 ObjectRef）。
  object_ref TEXT NOT NULL,
  -- 可信主体。
  principal TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS domain_query_by_task ON domain_query_tasks(task_id);

-- B 步：维护接管任务（工单场景，工单挂到真实 Task 下）。

CREATE TABLE IF NOT EXISTS maintenance_tasks (
  -- 请求键（幂等：同键复用同一接管任务）。
  request_key TEXT PRIMARY KEY,
  -- 关联的 Task。
  task_id TEXT NOT NULL,
  -- 接管对象。
  object_ref TEXT NOT NULL,
  -- 可信主体。
  subject TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS maintenance_by_object ON maintenance_tasks(object_ref);

CREATE TABLE IF NOT EXISTS ticket_dispatch_inputs (
  operation_id TEXT PRIMARY KEY REFERENCES operations(operation_id) ON DELETE CASCADE,
  evidence TEXT NOT NULL,
  asset_revision INTEGER NOT NULL,
  ownership_epoch INTEGER NOT NULL,
  provider_id TEXT NOT NULL,
  next_check_at INTEGER NOT NULL DEFAULT 0
);
