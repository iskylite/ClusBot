-- P3-03：绑定具体动作的确认与人工接管（V3 §5.2/5.5、§9.2、F07/F08）。
--
-- 人工决定既具体可追溯，也不会变成绕过授权的通行证：
--   - 确认绑定主体、动作、实际对象、参数摘要、合同/策略/资产版本、决定者
--     和期限；
--   - 通过已有 Chat/责任入口返回待决定事项；满足有效自动策略的动作不逐次
--     要求人工；
--   - 内容/权限/版本变化后旧确认无效，需重判并按策略决定是否重新请求确认；
--   - 人工接管未知效果须记录明确授权、接收人、未决操作、隔离措施、残余
--     风险和后续责任；
--   - 确认通过与实际派发分开；请求已收到或用户点过按钮不能直接修改
--     Operation 为成功。

CREATE TABLE IF NOT EXISTS approvals (
  approval_id TEXT PRIMARY KEY,
  -- 绑定：主体、动作、实际对象、参数摘要。
  subject TEXT NOT NULL,
  action TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  params_digest TEXT NOT NULL,
  -- 合同/策略/资产版本（内容/权限/版本变化后旧确认无效）。
  contract_revision INTEGER NOT NULL DEFAULT 0,
  policy_version INTEGER NOT NULL DEFAULT 0,
  asset_revision INTEGER NOT NULL DEFAULT 0,
  -- 决定者与期限。
  decided_by TEXT NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 决定结果与状态。
  decision TEXT NOT NULL CHECK(decision IN ('pending','approved','rejected','expired')) DEFAULT 'pending',
  decided_at INTEGER NOT NULL DEFAULT 0,
  -- 请求键（重复点击只产生一次有效决定）。
  request_key TEXT UNIQUE,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS approvals_by_object ON approvals(object_ref, action, decision);
CREATE INDEX IF NOT EXISTS approvals_pending ON approvals(decision, expires_at) WHERE decision='pending';

-- 人工接管记录（未知效果的明确授权与后续责任）。
CREATE TABLE IF NOT EXISTS manual_takeovers (
  takeover_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  -- 明确授权与接收人。
  authorized_by TEXT NOT NULL,
  taken_over_by TEXT NOT NULL,
  -- 未决操作、隔离措施、残余风险和后续责任。
  pending_operations TEXT NOT NULL DEFAULT '',
  isolation_measures TEXT NOT NULL DEFAULT '',
  residual_risks TEXT NOT NULL DEFAULT '',
  follow_up_responsibility TEXT NOT NULL DEFAULT '',
  -- 接管时间。
  taken_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS takeovers_by_task ON manual_takeovers(task_id);
