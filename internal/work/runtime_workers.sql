-- P2-01：工作者实例与受限身份（V3 §3.3、§5.4、§9.1）。
--
-- 区分实际执行实例和角色，为统一 API 建立可信调用主体：
--   - 登记 worker_id、角色、支持单元类型、实例版本、最近联系、启用/撤销
--     及有效期；不能所有实例都叫 personal-worker；
--   - 建立受限 Worker 身份及凭据摘要，绑定允许领取的类型/范围；秘密不进入
--     日志、Manifest 或普通回执；
--   - 本地嵌入执行器也通过同一服务协议校验，不直接访问 SQLite；
--   - 通用主体记录在 P3 扩展，不再创建一套不受统一撤权管理的 Worker 权限系统。

CREATE TABLE IF NOT EXISTS worker_instances (
  -- 实例身份：worker_id 是实例，不是 personal-worker 角色。
  worker_id TEXT PRIMARY KEY,
  -- 角色与支持的单元类型（model/deterministic/action 的组合，JSON 数组）。
  role TEXT NOT NULL,
  supported_kinds TEXT NOT NULL DEFAULT '[]',
  -- 实例版本与最近联系。
  instance_version TEXT NOT NULL DEFAULT '',
  last_contact_at INTEGER NOT NULL DEFAULT 0,
  -- 启用/撤销与有效期。
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  revoked_at INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 受限身份：凭据摘要（不存明文秘密）与允许领取的范围。
  credential_digest TEXT NOT NULL,
  credential_version INTEGER NOT NULL DEFAULT 1,
  allowed_scope TEXT NOT NULL DEFAULT '',  -- 允许领取的对象范围（scope_label）
  -- 审计。
  registered_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS workers_enabled ON worker_instances(enabled, expires_at);

-- Worker 领取/续租/回报的认证令牌（每次领取签发，绑定单元与代际）。
CREATE TABLE IF NOT EXISTS worker_tokens (
  token_id TEXT PRIMARY KEY,
  worker_id TEXT NOT NULL REFERENCES worker_instances(worker_id) ON DELETE CASCADE,
  -- 绑定的单元与代际（令牌不包含外部凭据）。
  unit_id TEXT NOT NULL,
  lease_generation INTEGER NOT NULL,
  -- 令牌摘要（不存明文）与有效期。
  token_digest TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  -- 使用状态。
  state TEXT NOT NULL CHECK(state IN ('active','renewed','expired','revoked')) DEFAULT 'active',
  issued_at INTEGER NOT NULL,
  renewed_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS tokens_by_worker ON worker_tokens(worker_id, state);
CREATE INDEX IF NOT EXISTS tokens_by_unit ON worker_tokens(unit_id, lease_generation);
