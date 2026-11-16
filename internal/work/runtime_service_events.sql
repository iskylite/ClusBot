-- P3-17：持续服务实例与规范化事件（V3 §4.2、§8.1/8.3，F03/F09/F11）。
--
-- 监控/告警作为持续来源接入，事件产生有限工作：
--   - 登记服务实例、来源身份、订阅范围、配置版本、启停/变更能力和有效期；
--     服务不是永远运行的 Task；
--   - 规定推送/轮询的事件 ID、来源序号、对象、时间、关联及覆盖；同 ID 异
--     内容隔离；
--   - 明确周期检查由哪方触发；外部已调度的检查不再由本地 Schedule 重复执行；
--   - 失联后按可支持重放/查询补缺，记录水位与遗漏；无保证则返回覆盖未知；
--   - 观测允许规则化聚合，动作回执、撤权和控制事件不能当普通噪声丢弃。

CREATE TABLE IF NOT EXISTS service_instances (
  service_id TEXT PRIMARY KEY,
  -- 来源身份与订阅范围。
  source_identity TEXT NOT NULL,
  subscription_scope TEXT NOT NULL DEFAULT '',
  -- 配置版本与有效期。
  config_version INTEGER NOT NULL DEFAULT 1,
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 启停/变更能力。
  supports_pause TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_pause IN ('supported','unsupported','unknown')),
  supports_resume TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_resume IN ('supported','unsupported','unknown')),
  supports_update TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_update IN ('supported','unsupported','unknown')),
  -- 状态（active/paused/stopped；服务不是永远运行的 Task）。
  state TEXT NOT NULL CHECK(state IN ('active','paused','stopped')) DEFAULT 'active',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS services_by_source ON service_instances(source_identity, state);

-- 服务事件（推送/轮询的事件 ID、来源序号、对象、时间、关联及覆盖）。
CREATE TABLE IF NOT EXISTS service_events (
  -- 事件 ID 与来源序号（推送与轮询重复事件只应用一次）。
  event_id TEXT PRIMARY KEY,
  service_id TEXT NOT NULL REFERENCES service_instances(service_id) ON DELETE CASCADE,
  source_sequence INTEGER NOT NULL DEFAULT 0,
  -- 对象、时间、关联及覆盖。
  object_ref TEXT NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL,
  correlation_id TEXT NOT NULL DEFAULT '',
  coverage TEXT NOT NULL DEFAULT '',
  -- 事件类型与内容摘要。
  kind TEXT NOT NULL,
  payload_digest TEXT NOT NULL,
  -- 状态（received/applied/quarantined；同 ID 异内容隔离）。
  state TEXT NOT NULL CHECK(state IN ('received','applied','quarantined')) DEFAULT 'received',
  received_at INTEGER NOT NULL,
  UNIQUE(service_id, source_sequence)
);

CREATE INDEX IF NOT EXISTS events_by_service ON service_events(service_id, state);
CREATE INDEX IF NOT EXISTS events_by_object ON service_events(object_ref, occurred_at);

-- 服务水位（失联后按可支持重放/查询补缺，记录水位与遗漏）。
CREATE TABLE IF NOT EXISTS service_watermarks (
  service_id TEXT PRIMARY KEY REFERENCES service_instances(service_id) ON DELETE CASCADE,
  -- 最后应用的事件序号（水位）。
  last_sequence INTEGER NOT NULL DEFAULT 0,
  -- 遗漏（无保证则返回覆盖未知）。
  gaps TEXT NOT NULL DEFAULT '[]',
  coverage_unknown INTEGER NOT NULL DEFAULT 0 CHECK(coverage_unknown IN (0,1)),
  -- 更新时间。
  updated_at INTEGER NOT NULL
);

-- 周期检查归属（外部已调度的检查不再由本地 Schedule 重复执行）。
CREATE TABLE IF NOT EXISTS service_check_ownership (
  check_key TEXT PRIMARY KEY,  -- service_id:check_kind
  service_id TEXT NOT NULL,
  check_kind TEXT NOT NULL,
  -- 归属（external/local；外部已调度的检查不再由本地 Schedule 重复执行）。
  owner TEXT NOT NULL CHECK(owner IN ('external','local')),
  -- 外部调度信息（外部已调度时记录）。
  external_schedule_ref TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
