-- P3-09：异步 Job、回执、查询与事件关联（V3 §8.3—8.5，F02/F03/F04）。
--
-- 外部长作业跨进程与会话持续跟踪，回执有可信来源与关联：
--   - Start 同步结果/异步句柄分别规范化；保存 provider_job_id、查询键、
--     状态保留契约和下一查询时间；
--   - 回调认证后先入 Inbox，绑定 operation_id、attempt、目标、外部版本与
--     代际；未知关联隔离；
--   - 推送与轮询进入同一去重/核对路径；无可靠顺序不按到达时间覆盖，必要时
--     Get 当前状态；
--   - 轮询用 Wait/Timer 和有界退避，不占 Worker；重启按持久 Job ID 继续；
--   - 纯外部 MCP Task 只作为远端句柄，不替代本地 Task；无推送/查询时明确
--     保留未知。

CREATE TABLE IF NOT EXISTS operation_jobs (
  job_id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE CASCADE,
  -- provider_job_id、查询键、状态保留契约和下一查询时间。
  provider_job_id TEXT NOT NULL,
  query_key TEXT NOT NULL,
  retention_contract TEXT NOT NULL DEFAULT '',
  next_query_at INTEGER NOT NULL DEFAULT 0,
  -- 状态（running/succeeded/failed/expired）。
  state TEXT NOT NULL CHECK(state IN ('running','succeeded','failed','expired')) DEFAULT 'running',
  -- 查询代际（无可靠顺序不按到达时间覆盖）。
  query_generation INTEGER NOT NULL DEFAULT 1,
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(operation_id, provider_job_id)
);

CREATE INDEX IF NOT EXISTS jobs_by_provider ON operation_jobs(provider_job_id, state);
CREATE INDEX IF NOT EXISTS jobs_due ON operation_jobs(next_query_at, state) WHERE state='running';

-- 回执关联（回调认证后绑定 operation/attempt/目标/版本/代际）。
CREATE TABLE IF NOT EXISTS operation_receipts (
  receipt_id TEXT PRIMARY KEY,
  -- 绑定：operation/attempt/目标/外部版本/代际。
  operation_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL DEFAULT '',
  target_ref TEXT NOT NULL DEFAULT '',
  external_version INTEGER NOT NULL DEFAULT 0,
  query_generation INTEGER NOT NULL DEFAULT 0,
  -- 回执内容与摘要（同键不同内容隔离）。
  payload_digest TEXT NOT NULL,
  receipt TEXT NOT NULL DEFAULT '',
  -- 接收来源（回调/轮询）。
  source TEXT NOT NULL CHECK(source IN ('callback','poll')),
  received_at INTEGER NOT NULL,
  -- 处理状态（received/applied/stale/quarantined）。
  state TEXT NOT NULL CHECK(state IN ('received','applied','stale','quarantined')) DEFAULT 'received',
  UNIQUE(operation_id, external_version, source)
);

CREATE INDEX IF NOT EXISTS receipts_by_operation ON operation_receipts(operation_id, state);
