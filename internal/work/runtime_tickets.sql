-- 硬件故障、工单和更换部件（§10.3、E03、F03/F08/F10）。
--
-- 人工维修跨日等待可持续跟踪，工单回执不替代设备验证：
--   - 经领域维护接管创建/关联外部工单，记录责任人、对象/部件、故障证据和
--     外部状态版本；
--   - 工单长等待使用 Wait/事件/查询，不占模型和工作者；重复回调不重复新建
--     工单；
--   - 部件更换结束旧安装关系并建立新部件/关系，触发相关验收依赖失效；
--   - 工单关闭后查询实际效果并测试，验证后回待复核；未恢复继续限制和后续
--     责任；
--   - 借出期间只有专项授权维修可调用相关动作，维修记录不把资产变维护中或
--     在役。

CREATE TABLE IF NOT EXISTS tickets (
  ticket_id TEXT PRIMARY KEY,
  -- 外部工单系统与工单 ID（幂等：重复回调不重复新建工单）。
  external_system TEXT NOT NULL,
  external_ticket_id TEXT NOT NULL,
  -- 关联 incident 与维护接管。
  incident_id TEXT,
  takeover_id TEXT,
  -- 责任人、对象/部件、故障证据和外部状态版本。
  assignee TEXT NOT NULL DEFAULT '',
  object_ref TEXT NOT NULL,
  component_refs TEXT NOT NULL DEFAULT '[]',
  fault_evidence TEXT NOT NULL DEFAULT '[]',
  external_state_version INTEGER NOT NULL DEFAULT 1,
  external_state TEXT NOT NULL DEFAULT 'open',
  -- 状态（open/waiting/closed/verified/failed）。
  state TEXT NOT NULL CHECK(state IN ('open','waiting','closed','verified','failed')) DEFAULT 'open',
  -- 维修复核（工单关闭后查询实际效果并测试，验证后回待复核）。
  verification_outcome TEXT NOT NULL DEFAULT '',
  residual_limitations TEXT NOT NULL DEFAULT '',
  follow_up_responsibility TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(external_system, external_ticket_id)
);

CREATE INDEX IF NOT EXISTS tickets_by_incident ON tickets(incident_id, state);
CREATE INDEX IF NOT EXISTS tickets_by_object ON tickets(object_ref, state);
