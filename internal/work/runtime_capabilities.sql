-- P3-04：能力、提供者与连接分离（V3 §8.1/8.5）。
--
-- 统一能力语义而保持内部函数、CLI、HTTP、MCP 与已有平台可替换：
--   - 能力有稳定 ID/版本、Query/Action 类别、输入输出 Schema、目标类型和
--     执行模式；提供者与连接另有身份及版本；
--   - 对 Get/Cancel/Events/Materials/幂等/fencing 分别记录 supported/
--     unsupported/unknown 及实际验证范围；
--   - 工具名称与连接 ID 保持来源区分；现有 MCP 别名可复用，不能把同名不同
--     连接合并；
--   - 能力和连接描述只声明机制，不拥有 Task/资产权威，也不能自报业务验收
--     通过；
--   - 新写能力默认不向模型暴露，需相应契约验证和当前主体准入通过。

CREATE TABLE IF NOT EXISTS capabilities (
  capability_id TEXT PRIMARY KEY,
  -- 稳定 ID/版本与类别。
  capability_key TEXT NOT NULL,   -- 能力标识（如 cluster.node.read）
  version INTEGER NOT NULL DEFAULT 1,
  -- Query/Action 类别。
  kind TEXT NOT NULL CHECK(kind IN ('query','action')),
  -- 输入输出 Schema（JSON）。
  input_schema TEXT NOT NULL DEFAULT '{}',
  output_schema TEXT NOT NULL DEFAULT '{}',
  -- 目标类型与执行模式。
  target_kind TEXT NOT NULL DEFAULT '',
  execution_mode TEXT NOT NULL DEFAULT 'sync', -- sync/async/deterministic
  -- 能力特性：Get/Cancel/Events/Materials/幂等/fencing 分别记录。
  supports_get TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_get IN ('supported','unsupported','unknown')),
  supports_cancel TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_cancel IN ('supported','unsupported','unknown')),
  supports_events TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_events IN ('supported','unsupported','unknown')),
  supports_materials TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_materials IN ('supported','unsupported','unknown')),
  idempotent TEXT NOT NULL DEFAULT 'unknown' CHECK(idempotent IN ('supported','unsupported','unknown')),
  fencing TEXT NOT NULL DEFAULT 'unknown' CHECK(fencing IN ('supported','unsupported','unknown')),
  -- 实际验证范围（声明只声明机制，不拥有权威）。
  verified_scope TEXT NOT NULL DEFAULT '',
  -- 新写能力默认不向模型暴露（需契约验证和主体准入）。
  exposed_to_model INTEGER NOT NULL DEFAULT 0 CHECK(exposed_to_model IN (0,1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(capability_key, version)
);

-- 提供者（内部函数/CLI/HTTP/MCP/平台可替换）。
CREATE TABLE IF NOT EXISTS capability_providers (
  provider_id TEXT PRIMARY KEY,
  -- 提供者身份与版本（与连接分离）。
  provider_key TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1,
  -- 类型与配置引用（不含明文秘密）。
  kind TEXT NOT NULL CHECK(kind IN ('internal','cli','http','mcp','platform')),
  config_ref TEXT NOT NULL DEFAULT '',
  -- 支持的能力（capability_key:version 列表，JSON）。
  capabilities TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL
);

-- 连接（具体实例，与提供者分离）。
CREATE TABLE IF NOT EXISTS capability_connections (
  connection_id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL REFERENCES capability_providers(provider_id),
  -- 连接身份与版本（同名不同连接不合并）。
  connection_name TEXT NOT NULL,
  connection_version INTEGER NOT NULL DEFAULT 1,
  -- 凭据引用（不含明文秘密）。
  credential_ref TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS connections_by_provider ON capability_connections(provider_id, enabled);
CREATE INDEX IF NOT EXISTS capabilities_by_key ON capabilities(capability_key, version);
