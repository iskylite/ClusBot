-- P5-05：异步清理、失效、删除与恢复保护（V3 §7.3/7.6、§11.3，F07/F10/F11）。
--
-- 派生内容可收敛清理，删除请求不会因旧快照恢复而重新出现：
--   - 区分事实失效、召回降权和用户删除；删除先设受控不可读标记并使撤销闸门
--     生效；
--   - 后台按反向依赖标记、清理/重建索引、摘要、候选和缓存；固定源/策略版本，
--     提交前再核对；
--   - 失败有限退避并留待处理记录；记录未完成数量、最老年龄和完成时间，发送
--     事件不算清理完成；
--   - 审计仅保留允许的必要元数据；外部副本和备份按各自范围处理，不声称已
--     物理删除所有副本；
--   - 恢复备份/检查点时应用当前删除/撤销约束并先核查；无法取得当前权威时
--     拒绝敏感读取，不恢复旧许可。

CREATE TABLE IF NOT EXISTS cleanup_queue (
  cleanup_id TEXT PRIMARY KEY,
  -- 清理类型（事实失效、召回降权和用户删除）。
  kind TEXT NOT NULL CHECK(kind IN ('invalidate','demote','delete')),
  -- 对象与来源。
  object_ref TEXT NOT NULL,
  source_ref TEXT NOT NULL DEFAULT '',
  -- 受控不可读标记（删除先设受控不可读标记并使撤销闸门生效）。
  unreadable INTEGER NOT NULL DEFAULT 0 CHECK(unreadable IN (0,1)),
  -- 固定源/策略版本（提交前再核对）。
  source_version INTEGER NOT NULL DEFAULT 1,
  policy_version INTEGER NOT NULL DEFAULT 1,
  -- 状态（pending/processing/completed/failed）。
  state TEXT NOT NULL CHECK(state IN ('pending','processing','completed','failed')) DEFAULT 'pending',
  -- 失败有限退避并留待处理记录。
  retry_count INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  failure_reason TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  completed_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS cleanup_by_state ON cleanup_queue(state, next_retry_at);
CREATE INDEX IF NOT EXISTS cleanup_by_object ON cleanup_queue(object_ref, kind, state);

-- 清理进度（记录未完成数量、最老年龄和完成时间）。
CREATE TABLE IF NOT EXISTS cleanup_progress (
  progress_id INTEGER PRIMARY KEY CHECK(progress_id=1),
  -- 未完成数量、最老年龄和完成时间。
  pending_count INTEGER NOT NULL DEFAULT 0,
  oldest_age_ms INTEGER NOT NULL DEFAULT 0,
  last_completed_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
