-- 统一主体、范围授权与直接撤权（§9、§11.3，F07）。
--
-- 用户、外部服务、Worker 和策略执行主体使用同一授权语义：
--   - 授权包含主体、动作、对象/范围、参数/时间限制、来源/策略版本、有效期、
--     撤销和责任人；旧工具 allowed 不自动变成资产写权限；
--   - 读取、材料、模型外发、任务控制与动作派发分别判定；调用者不能用伪造
--     auth_ref 自证获准；
--   - 撤销与授权版本/相关代际及可靠通知同事务；新的最终核验读取当前权威，
--     不使用滞后索引或旧缓存；
--   - 区分已撤销后的新核验和撤销前在途请求；保留在途责任，不宣称已发送内容
--     可撤回；
--   - API 不返回未授权对象的标题/数量/敏感正文；策略主体只有明确范围，不
--     继承用户所有权限。

CREATE TABLE IF NOT EXISTS authorizations (
  auth_id TEXT PRIMARY KEY,
  -- 主体与动作。
  subject TEXT NOT NULL,          -- user:<id>/service:<id>/worker:<id>/policy:<id>
  action TEXT NOT NULL,           -- read/write/control/dispatch/export_model/...
  -- 对象/范围与参数限制。
  object_ref TEXT NOT NULL DEFAULT '',  -- 具体对象或范围
  scope_label TEXT NOT NULL DEFAULT '',
  param_constraints TEXT NOT NULL DEFAULT '{}', -- JSON：参数/时间限制
  -- 来源与策略版本。
  source TEXT NOT NULL DEFAULT '',       -- 授权来源（用户授权/策略）
  policy_version INTEGER NOT NULL DEFAULT 1,
  -- 有效期与撤销。
  valid_from INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0,
  revoked_by TEXT NOT NULL DEFAULT '',
  -- 责任人与版本。
  owner TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS authz_subject ON authorizations(subject, action, revoked_at);
CREATE INDEX IF NOT EXISTS authz_object ON authorizations(object_ref, action, revoked_at);

-- 授权决策记录（每次判定，含拒绝）。
CREATE TABLE IF NOT EXISTS authorization_decisions (
  decision_id TEXT PRIMARY KEY,
  subject TEXT NOT NULL,
  action TEXT NOT NULL,
  object_ref TEXT NOT NULL DEFAULT '',
  -- 决策结果与依据。
  outcome TEXT NOT NULL CHECK(outcome IN ('allowed','denied','requires_confirmation')),
  reason TEXT NOT NULL DEFAULT '',
  -- 决策版本与政策。
  policy_version INTEGER NOT NULL DEFAULT 1,
  decided_at INTEGER NOT NULL,
  -- 在途标记（区分已撤销后的新核验和撤销前在途请求）。
  in_flight INTEGER NOT NULL DEFAULT 0 CHECK(in_flight IN (0,1))
);

CREATE INDEX IF NOT EXISTS authz_decisions_subject ON authorization_decisions(subject, decided_at);

-- 主体身份（用户/服务/Worker/策略执行主体统一注册，不能互相冒用）。
CREATE TABLE IF NOT EXISTS principals (
  principal_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK(kind IN ('user','service','worker','policy')),
  -- 身份摘要与有效期。
  identity_digest TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  expires_at INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
