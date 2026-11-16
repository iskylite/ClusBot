-- P3-02：最小资产身份、版本与执行准入（V3 §5.5、§6.4/6.7，F05/F06）。
--
-- 任何集群写操作都能回答目标是谁、当前谁管理、是否允许执行：
--   - 资产保存稳定 ObjectRef、唯一当前末级状态、revision、ownership_epoch、
--     冻结和必要占用；未知身份不能写；
--   - 授权与领域准入分离的接口，返回允许/拒绝/待核对及来源版本；
--     不能只用字符串 scope_label；
--   - 最终派发和冻结共用同一资产权威记录与写事务顺序；批量对象逐一核对
--     版本/代际，不能用单个版本代表全部；
--   - 冻结先提交阻止新冲突写；已派发保留在途；此阶段只做协议级冻结测试；
--   - 无外部 fencing 时保留迟到外部效果可能发生的边界，SQL 版本不能冒充
--     设备侧撤销。

CREATE TABLE IF NOT EXISTS ops_assets (
  -- 稳定 ObjectRef（唯一当前末级身份）。
  object_ref TEXT PRIMARY KEY,
  -- 资产种类与属性（最小：kind/identity）。
  kind TEXT NOT NULL,
  identity TEXT NOT NULL,             -- 唯一身份（IP/序列号/名称等）
  -- 唯一当前末级状态。
  state TEXT NOT NULL,
  -- 版本与所有权代际。
  revision INTEGER NOT NULL DEFAULT 1,
  ownership_epoch INTEGER NOT NULL DEFAULT 1,
  -- 冻结（阻止新冲突写）。
  frozen_at INTEGER NOT NULL DEFAULT 0,
  frozen_by TEXT NOT NULL DEFAULT '',
  freeze_reason TEXT NOT NULL DEFAULT '',
  -- 必要占用（当前管理任务/操作）。
  current_owner TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS assets_state ON ops_assets(state);
CREATE INDEX IF NOT EXISTS assets_identity ON ops_assets(identity);

-- 资产状态历史（状态迁移留证）。
CREATE TABLE IF NOT EXISTS asset_state_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  object_ref TEXT NOT NULL,
  state TEXT NOT NULL,
  revision INTEGER NOT NULL,
  changed_by TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS asset_history ON asset_state_history(object_ref, occurred_at);

-- 领域准入决策（授权与领域准入分离）。
CREATE TABLE IF NOT EXISTS asset_admission_decisions (
  decision_id TEXT PRIMARY KEY,
  object_ref TEXT NOT NULL,
  action TEXT NOT NULL,
  -- 授权结果（谁可以做）。
  auth_outcome TEXT NOT NULL,
  -- 领域准入结果（当前资产状态和证据下能不能做）。
  admission_outcome TEXT NOT NULL CHECK(admission_outcome IN ('allowed','denied','reconcile')),
  admission_reason TEXT NOT NULL DEFAULT '',
  -- 来源版本（资产 revision/ownership_epoch）。
  asset_revision INTEGER NOT NULL,
  ownership_epoch INTEGER NOT NULL,
  decided_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS admission_by_asset ON asset_admission_decisions(object_ref, decided_at);
