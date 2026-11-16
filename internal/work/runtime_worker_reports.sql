-- P2-03：工作者回报、流式片段与迟到证据（V3 §5.4、F03/F04）。
--
-- 网络重复和旧执行者回报不会重复推进、计费或覆盖成果：
--   - 回报含稳定 report_id、单元/Run、合同、任务分配及租约代际、worker_id、
--     摘要和证据；服务端从令牌核对关联；
--   - 回报先入 Inbox；应用时再次比较当前状态与版本，结果与用量只应用一次；
--   - 流式片段有序号/摘要和大小限制；重复片段不重复拼接，同序号异内容隔离；
--     未持久片段不当作已交付成果；
--   - 旧代际可保存可信回执为核对线索，但不能完成当前 Step 或制造新派发；
--   - 工作者无法提交时保留有界重报机制；宿主不可写时不得回复已可靠接收。

CREATE TABLE IF NOT EXISTS worker_reports (
  -- 稳定回报身份（重复回报只应用一次）。
  report_id TEXT PRIMARY KEY,
  -- 关联：单元/Run/任务与代际（服务端从令牌核对）。
  unit_id TEXT NOT NULL,
  run_id TEXT,
  task_id TEXT NOT NULL,
  -- 合同、任务分配及租约代际。
  contract_revision INTEGER NOT NULL DEFAULT 0,
  assignment_generation INTEGER NOT NULL DEFAULT 0,
  lease_generation INTEGER NOT NULL,
  worker_id TEXT NOT NULL,
  -- 摘要、用量和证据引用。
  summary TEXT NOT NULL DEFAULT '',
  model_requests INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER,
  output_tokens INTEGER,
  evidence_ref TEXT NOT NULL DEFAULT '',
  -- 结果内容与摘要（供幂等比对）。
  result_digest TEXT NOT NULL,
  -- 接收与处理状态。
  received_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('received','applied','stale','quarantined')) DEFAULT 'received',
  applied_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS reports_by_unit ON worker_reports(unit_id, state);
CREATE INDEX IF NOT EXISTS reports_by_task ON worker_reports(task_id, state);

-- 流式片段：有序号/摘要和大小限制；重复片段不重复拼接，同序号异内容隔离。
CREATE TABLE IF NOT EXISTS report_fragments (
  report_id TEXT NOT NULL REFERENCES worker_reports(report_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  -- 片段摘要与内容（大小受限；未持久片段不当作已交付成果）。
  fragment_digest TEXT NOT NULL,
  content TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (report_id, seq)
);

-- 同序号异内容的隔离记录（不拼接错误正文）。
CREATE TABLE IF NOT EXISTS report_fragment_conflicts (
  report_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  existing_digest TEXT NOT NULL,
  incoming_digest TEXT NOT NULL,
  incoming_content TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (report_id, seq, incoming_digest)
);
