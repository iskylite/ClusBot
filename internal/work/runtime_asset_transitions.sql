-- P4-05：资产九态与领域迁移服务（V3 §6.7—6.8、F05/F06/F08）。
--
-- 状态唯一、语义互斥，每次迁移由领域证据与规则确认：
--   - 实现 V3 §6.7 全部合法迁移、触发与前置条件；Task/Operation 仅引用，
--     不直接写资产状态；
--   - 迁移事务核对当前状态、预期版本、规则、证据、占用、责任和授权，写
--     迁移历史与 Outbox；
--   - 健康、可达性、业务占用单独表达；告警不自动进入维护中，Task 取消不
--     自动回在役；
--   - 维护接管可由有效策略自动完成，也可等待有权人员；均必须有影响与责任
--     记录；
--   - 验证失效、调试中断、首次纳管、归还和再启用按各自条件进入待复核；
--     保留实际限制。

CREATE TABLE IF NOT EXISTS asset_transitions (
  transition_id TEXT PRIMARY KEY,
  -- 对象与迁移。
  object_ref TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  -- 触发与前置条件（证据与规则确认）。
  trigger_kind TEXT NOT NULL,
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  rule_refs TEXT NOT NULL DEFAULT '[]',
  -- 版本与占用（核对当前状态、预期版本、占用）。
  expected_revision INTEGER NOT NULL,
  expected_ownership_epoch INTEGER NOT NULL,
  -- 责任与授权。
  actor TEXT NOT NULL,
  responsibility TEXT NOT NULL DEFAULT '',
  -- 健康、可达性、业务占用单独表达。
  health TEXT NOT NULL DEFAULT 'unknown' CHECK(health IN ('healthy','degraded','unhealthy','unknown')),
  reachability TEXT NOT NULL DEFAULT 'unknown' CHECK(reachability IN ('reachable','unreachable','unknown')),
  business_occupancy TEXT NOT NULL DEFAULT '' CHECK(business_occupancy IN ('','occupied','free')),
  -- 时间。
  occurred_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS transitions_by_object ON asset_transitions(object_ref, occurred_at);
CREATE INDEX IF NOT EXISTS transitions_by_state ON asset_transitions(object_ref, to_state, occurred_at);

-- 维护接管（影响与责任记录）。
CREATE TABLE IF NOT EXISTS maintenance_takeovers (
  takeover_id TEXT PRIMARY KEY,
  -- 对象与接管方式（自动/人工）。
  object_ref TEXT NOT NULL,
  takeover_kind TEXT NOT NULL CHECK(takeover_kind IN ('automatic','manual')),
  -- 影响与责任记录。
  impact_assessment TEXT NOT NULL,
  responsibility TEXT NOT NULL,
  authorized_by TEXT NOT NULL DEFAULT '',
  -- 时间。
  taken_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS maintenance_by_object ON maintenance_takeovers(object_ref, taken_at);
