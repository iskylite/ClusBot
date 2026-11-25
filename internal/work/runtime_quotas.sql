-- 持久预算、并发额度与占用（§3.3、§5.6、F04/F05/F12）。
--
-- 多工作者共享预算和配额，重启、重领或新 Run 都不能重置已消费成本：
--   - 区分 Task 总预算、单 Run 上限、模型/提供者并发、工作组速率和目标占用；
--     配置项含明确单位与作用域；
--   - 领取/派发所需的预留与状态同事务；回报按稳定身份结算，重复结算无影响；
--   - 模型槽按实际模型请求占用；确定步骤和持久等待不占模型槽；
--   - 费用未知时保留已预留上限/未知状态，不退款成零；租约可回收的执行额度
--     与资产危险操作占用分开；
--   - 外部动作效果未核实前不能仅按 TTL 释放排他占用；运行时间和 token 使用
--     累计到正确任务/步骤。

CREATE TABLE IF NOT EXISTS quota_reservations (
  reservation_id TEXT PRIMARY KEY,
  -- 作用域：task_budget（任务总预算）/run_limit（单 Run 上限）/model_slot
  --（模型并发）/provider_rate（提供者速率）/target_hold（目标占用）。
  scope TEXT NOT NULL CHECK(scope IN ('task_budget','run_limit','model_slot','provider_rate','target_hold')),
  -- 关联（按作用域填充）。
  task_id TEXT,
  run_id TEXT,
  step_id TEXT,
  worker_id TEXT,
  target_ref TEXT NOT NULL DEFAULT '',
  -- 预留量（明确单位与作用域）。
  amount INTEGER NOT NULL,
  unit TEXT NOT NULL, -- tokens/ms/slots/requests/holds
  -- 状态：reserved/settled/released/expired（重复结算无影响）。
  state TEXT NOT NULL CHECK(state IN ('reserved','settled','released','expired')) DEFAULT 'reserved',
  -- 稳定身份（回报按稳定身份结算，重复结算无影响）。
  settlement_key TEXT UNIQUE,
  settled_at INTEGER NOT NULL DEFAULT 0,
  -- 费用未知时保留已预留上限/未知状态，不退款成零。
  cost_known INTEGER NOT NULL DEFAULT 0 CHECK(cost_known IN (0,1)),
  -- 排他占用（资产危险操作；效果未核实前不能仅按 TTL 释放）。
  exclusive INTEGER NOT NULL DEFAULT 0 CHECK(exclusive IN (0,1)),
  effect_verified INTEGER NOT NULL DEFAULT 0 CHECK(effect_verified IN (0,1)),
  -- 时间。
  reserved_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0
);

-- 作用域占用统计（预留与状态同事务）。
CREATE INDEX IF NOT EXISTS reservations_by_scope ON quota_reservations(scope, task_id, state);
CREATE INDEX IF NOT EXISTS reservations_settlement ON quota_reservations(settlement_key) WHERE settlement_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS reservations_holds ON quota_reservations(target_ref, exclusive, state) WHERE exclusive=1;

-- 配额配置（区分 Task 总预算、单 Run 上限、并发、速率；含明确单位与作用域）。
CREATE TABLE IF NOT EXISTS quota_configs (
  config_key TEXT PRIMARY KEY,  -- task_budget:{task_id} / model_slot:{provider} / ...
  scope TEXT NOT NULL,
  -- 上限与单位。
  limit_amount INTEGER NOT NULL,
  unit TEXT NOT NULL,
  -- 作用域说明。
  subject TEXT NOT NULL DEFAULT '',
  -- 版本（乐观并发）。
  revision INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);
