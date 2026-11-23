-- P4-16：故障调查、假设和验证证据（V3 §10.1/10.4、E02/E06）。
--
-- 调查能够比较假设和反证，定位与恢复结论分开：
--   - 建立 incident、症状/影响范围、时间线、假设、支持/反证、未知项和下一
--     查询；模型猜测不能写当前事实；
--   - 按对象图查共同依赖和时间相关观测，按预算读取日志片段；优先做有区
--     分度的补查；
--   - 规则流程确定检查继续执行，需要判断时委派 Worker；不固定所有问题都
--     套一种 RCA 步骤；
--   - 形成处置提案时携带具体对象、能力/参数、证据和风险边界，经运行时与
--     领域准入后才可动作；
--   - 恢复验证与根因证明分别记录；恢复成功但根因未知仍保持未知，候选经验
--     不得改称已证实根因。

CREATE TABLE IF NOT EXISTS incidents (
  incident_id TEXT PRIMARY KEY,
  -- 症状/影响范围与时间线。
  symptoms TEXT NOT NULL DEFAULT '[]',
  impact_scope TEXT NOT NULL DEFAULT '{}',
  timeline TEXT NOT NULL DEFAULT '[]',
  -- 状态（open/investigating/mitigated/resolved/closed）。
  state TEXT NOT NULL CHECK(state IN ('open','investigating','mitigated','resolved','closed')) DEFAULT 'open',
  -- 恢复验证与根因证明分别记录。
  recovery_verified INTEGER NOT NULL DEFAULT 0 CHECK(recovery_verified IN (0,1)),
  recovery_evidence TEXT NOT NULL DEFAULT '',
  root_cause_proven INTEGER NOT NULL DEFAULT 0 CHECK(root_cause_proven IN (0,1)),
  root_cause TEXT NOT NULL DEFAULT '',
  root_cause_evidence TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS incidents_by_state ON incidents(state, created_at);

-- 假设（假设、支持/反证、未知项和下一查询）。
CREATE TABLE IF NOT EXISTS incident_hypotheses (
  hypothesis_id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES incidents(incident_id) ON DELETE CASCADE,
  -- 假设内容（模型猜测不能写当前事实）。
  statement TEXT NOT NULL,
  proposed_by TEXT NOT NULL,        -- model/worker/human
  -- 支持/反证。
  supporting_evidence TEXT NOT NULL DEFAULT '[]',
  counter_evidence TEXT NOT NULL DEFAULT '[]',
  -- 状态（candidate/supported/refuted/unknown）。
  state TEXT NOT NULL CHECK(state IN ('candidate','supported','refuted','unknown')) DEFAULT 'candidate',
  -- 未知项与下一查询。
  unknowns TEXT NOT NULL DEFAULT '[]',
  next_queries TEXT NOT NULL DEFAULT '[]',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS hypotheses_by_incident ON incident_hypotheses(incident_id, state);

-- 处置提案（携带具体对象、能力/参数、证据和风险边界）。
CREATE TABLE IF NOT EXISTS incident_proposals (
  proposal_id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES incidents(incident_id) ON DELETE CASCADE,
  -- 具体对象、能力/参数。
  object_refs TEXT NOT NULL,
  capability_key TEXT NOT NULL,
  params TEXT NOT NULL DEFAULT '{}',
  -- 证据与风险边界。
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  risk_boundary TEXT NOT NULL DEFAULT '',
  -- 状态（proposed/admitted/rejected/executed）。
  state TEXT NOT NULL CHECK(state IN ('proposed','admitted','rejected','executed')) DEFAULT 'proposed',
  -- 创建与时间。
  proposed_by TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS proposals_by_incident ON incident_proposals(incident_id, state);

-- A confirmation is bound to a principal, incident, target and observed
-- revisions. It is also the stable request key for ticket submission.
CREATE TABLE IF NOT EXISTS incident_action_previews (
  preview_id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES incidents(incident_id),
  subject TEXT NOT NULL,
  action TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  description TEXT NOT NULL,
  incident_updated_at INTEGER NOT NULL,
  asset_identity TEXT NOT NULL,
  asset_revision INTEGER NOT NULL,
  ownership_epoch INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS incident_ticket_operations (
  operation_id TEXT PRIMARY KEY REFERENCES operations(operation_id),
  incident_id TEXT NOT NULL REFERENCES incidents(incident_id),
  object_ref TEXT NOT NULL,
  subject TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS incident_ticket_operations_by_incident ON incident_ticket_operations(incident_id, object_ref);

-- The independent checker supplies this evidence. A ticket-system receipt or
-- a human-entered description cannot create a passing verification receipt.
CREATE TABLE IF NOT EXISTS ticket_health_verifications (
  ticket_id TEXT PRIMARY KEY REFERENCES tickets(ticket_id),
  object_ref TEXT NOT NULL,
  asset_identity TEXT NOT NULL,
  checked_asset_revision INTEGER NOT NULL,
  asset_revision INTEGER NOT NULL,
  ownership_epoch INTEGER NOT NULL,
  external_state_version INTEGER NOT NULL,
  evidence TEXT NOT NULL,
  verified_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS incident_ticket_rechecks (
  preview_id TEXT PRIMARY KEY REFERENCES incident_action_previews(preview_id),
  incident_id TEXT NOT NULL REFERENCES incidents(incident_id),
  ticket_id TEXT NOT NULL REFERENCES tickets(ticket_id),
  subject TEXT NOT NULL,
  evidence_digest TEXT NOT NULL,
  confirmed_at INTEGER NOT NULL
);
