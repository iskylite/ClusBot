-- 原子领取、续租与过期回收（§5.4—5.6、F04/F05）。
--
-- 同一执行单元同一时刻只有一个有效推进者，失联可恢复：
--   - Claim 在短事务核对 Task/Step、合同与控制修订、依赖、工作者范围和额度，
--     条件更新领取并增加 lease_generation；
--   - 签发绑定 unit_id、worker_id、版本和期限的不可伪造令牌；令牌不包含外部凭据；
--   - Renew 以控制面时间验证原租约、代际与身份，过期令牌不能续租；新领取
--     不复用旧令牌；
--   - 区分 Task 的 assignment_generation 和每个单元的 lease_generation；
--   - 失联回收先判断是否有已派发操作；未知外部效果进入核对，不能自动重新发出。

CREATE TABLE IF NOT EXISTS work_leases (
  -- 租约身份：unit_id + lease_generation 唯一（一个单元同一时刻至多一个有效领取）。
  lease_id TEXT PRIMARY KEY,
  unit_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  -- 租约代际（单元级，与任务分配代际不同）。
  lease_generation INTEGER NOT NULL,
  -- 持有者与令牌。
  worker_id TEXT NOT NULL,
  token_id TEXT NOT NULL,
  -- 期限（控制面时间裁决）。
  issued_at INTEGER NOT NULL,
  lease_until INTEGER NOT NULL,
  -- 状态：active/expired/returned/reclaimed。
  state TEXT NOT NULL CHECK(state IN ('active','expired','returned','reclaimed')) DEFAULT 'active',
  -- 回收核对：失联回收先判断是否有已派发操作。
  reclaim_reason TEXT NOT NULL DEFAULT '',
  reclaimed_at INTEGER NOT NULL DEFAULT 0,
  -- 已派发操作数（回收时快照，未知外部效果进入核对）。
  dispatched_ops INTEGER NOT NULL DEFAULT 0,
  UNIQUE(unit_id, lease_generation)
);

-- 有效租约查询（单元当前有效领取）。
CREATE INDEX IF NOT EXISTS leases_active ON work_leases(unit_id, state, lease_until) WHERE state='active';
CREATE INDEX IF NOT EXISTS leases_by_worker ON work_leases(worker_id, state);
CREATE INDEX IF NOT EXISTS leases_expired ON work_leases(lease_until, state) WHERE state='active';
