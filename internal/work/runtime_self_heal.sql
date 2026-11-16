-- P4-17：有界自动自愈策略与 E02（V3 §10.1.1、E02、F02/F06/F07/F08）。
--
-- 授权范围内自动推进，超出条件准确停止和交接：
--   - 策略固定 ID/版本、主体、资产范围、有效期、前置、动作与验证标准；
--   - 调查归并、维护接管、Operation、验证和待复核闭环；借出/冻结/身份冲突/
--     共享故障不能走普通自动写；
--   - 示例滚动 30 分钟每节点最多一次写派发、同策略集群并发最多五个；作为
--     可配置待验证参数，按稳定策略 ID 持久计数，重启/新任务/修订不清零；
--   - 成功回执后独立检查服务与新心跳/覆盖；验证失败不追加第二次重启，未知
--     效果先核对；
--   - 观察模式只报告拟判定，不派发写或改资产生命周期；停用/撤销阻止新准入，
--     未决责任仍处理。

CREATE TABLE IF NOT EXISTS self_heal_policies (
  policy_id TEXT PRIMARY KEY,
  -- 策略固定 ID/版本、主体、资产范围、有效期。
  policy_key TEXT NOT NULL,
  version INTEGER NOT NULL,
  subject TEXT NOT NULL,
  asset_scope TEXT NOT NULL DEFAULT '{}',
  valid_from INTEGER NOT NULL,
  valid_until INTEGER NOT NULL DEFAULT 0,
  -- 前置、动作与验证标准。
  preconditions TEXT NOT NULL DEFAULT '[]',
  action_capability TEXT NOT NULL,
  verification_standard TEXT NOT NULL DEFAULT '',
  -- 限流（可配置待验证参数）。
  per_node_window_ms INTEGER NOT NULL DEFAULT 1800000,  -- 30 分钟
  per_node_max_writes INTEGER NOT NULL DEFAULT 1,       -- 每节点最多一次
  cluster_max_concurrent INTEGER NOT NULL DEFAULT 5,    -- 集群并发最多五个
  -- 状态（draft/published/active/suspended/revoked）。
  state TEXT NOT NULL CHECK(state IN ('draft','published','active','suspended','revoked')) DEFAULT 'draft',
  -- 观察模式（只报告拟判定，不派发写）。
  observe_only INTEGER NOT NULL DEFAULT 1 CHECK(observe_only IN (0,1)),
  -- 发布者与时间。
  published_by TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(policy_key, version)
);

-- 策略执行计数（按稳定策略 ID 持久计数，重启/新任务/修订不清零）。
CREATE TABLE IF NOT EXISTS self_heal_counters (
  policy_id TEXT NOT NULL,
  counter_key TEXT NOT NULL,       -- per_node:<object_ref> 或 cluster_concurrent
  window_start INTEGER NOT NULL,
  count INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (policy_id, counter_key, window_start)
);

-- 策略执行记录（动作与效果有独立账本）。
CREATE TABLE IF NOT EXISTS self_heal_executions (
  execution_id TEXT PRIMARY KEY,
  policy_id TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  -- Operation 关联。
  operation_id TEXT,
  -- 状态（proposed/admitted/dispatched/verified/failed/blocked）。
  state TEXT NOT NULL CHECK(state IN ('proposed','admitted','dispatched','verified','failed','blocked')) DEFAULT 'proposed',
  -- 阻断原因。
  block_reason TEXT NOT NULL DEFAULT '',
  -- 验证结果（成功回执后独立检查服务与新心跳/覆盖）。
  verification_outcome TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS executions_by_policy ON self_heal_executions(policy_id, state, created_at);
CREATE INDEX IF NOT EXISTS executions_by_object ON self_heal_executions(object_ref, created_at);
