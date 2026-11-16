-- P5-04：来源依赖闭包与撤销版本闸门（V3 §7.3、§11.3，F07/F10）。
--
-- 后台传播未完成时也能阻断已知失权来源，读取不递归扫全图：
--   - 派生修订保存按源修订标识的有界依赖闭包和反向依赖；有环、超限、未完成
--     的派生记录保持不可用；
--   - 本地撤销/删除与策略分区代际递增、可靠传播事件同事务；主体授权变化也
--     必须进入有效判据；
--   - 读取缓存键包含主体、记录修订、闭包摘要、相关策略版本/分区代际及最早
--     到期时间；
--   - 新最终核验以当前权威快照比较代际；不一致有界重查，做不完先返回缺口
--     并重建，不能沿用旧缓存；
--   - 外部可靠通知/实时授权与仅凭有效期两种保证分开；无撤权机制不能宣称
--     即时获知，已在途内容不能撤回。

CREATE TABLE IF NOT EXISTS provenance_closures (
  closure_id TEXT PRIMARY KEY,
  -- 派生记录与源修订标识。
  derived_ref TEXT NOT NULL,
  derived_revision INTEGER NOT NULL DEFAULT 1,
  source_ref TEXT NOT NULL,
  source_revision INTEGER NOT NULL DEFAULT 1,
  -- 有界依赖闭包和反向依赖。
  forward_closure TEXT NOT NULL DEFAULT '[]',   -- JSON：正向依赖（源→派生）
  reverse_closure TEXT NOT NULL DEFAULT '[]',   -- JSON：反向依赖（派生→源）
  -- 闭包状态（complete/incomplete/cycle/overflow）。
  state TEXT NOT NULL CHECK(state IN ('complete','incomplete','cycle','overflow')) DEFAULT 'complete',
  -- 有界（超限的派生记录保持不可用）。
  bounded INTEGER NOT NULL DEFAULT 1 CHECK(bounded IN (0,1)),
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(derived_ref, derived_revision, source_ref, source_revision)
);

CREATE INDEX IF NOT EXISTS closures_by_derived ON provenance_closures(derived_ref, state);
CREATE INDEX IF NOT EXISTS closures_by_source ON provenance_closures(source_ref, source_revision);

-- 撤销版本闸门（本地撤销/删除与策略分区代际递增）。
CREATE TABLE IF NOT EXISTS revocation_gates (
  gate_id TEXT PRIMARY KEY,
  -- 对象/来源与代际。
  object_ref TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  -- 策略分区代际（代际递增阻断已知失权来源）。
  policy_partition TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 1,
  -- 状态（active/revoked/deleted）。
  state TEXT NOT NULL CHECK(state IN ('active','revoked','deleted')) DEFAULT 'active',
  -- 撤销者与时间。
  revoked_by TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(object_ref, source_ref, policy_partition)
);

CREATE INDEX IF NOT EXISTS gates_by_object ON revocation_gates(object_ref, state, generation);
CREATE INDEX IF NOT EXISTS gates_by_partition ON revocation_gates(policy_partition, generation);

-- 读取缓存键（主体、记录修订、闭包摘要、相关策略版本/分区代际及最早到期时间）。
CREATE TABLE IF NOT EXISTS read_cache_keys (
  cache_key TEXT PRIMARY KEY,
  -- 主体、记录修订、闭包摘要。
  subject TEXT NOT NULL,
  record_revision INTEGER NOT NULL DEFAULT 1,
  closure_digest TEXT NOT NULL,
  -- 相关策略版本/分区代际及最早到期时间。
  policy_version INTEGER NOT NULL DEFAULT 1,
  partition_generation INTEGER NOT NULL DEFAULT 1,
  earliest_expiry INTEGER NOT NULL DEFAULT 0,
  -- 创建。
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS cache_by_subject ON read_cache_keys(subject, policy_version, partition_generation);
