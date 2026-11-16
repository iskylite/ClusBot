-- P5-10：长期运行、归档与后台预算（V3 §5.6/5.8、§7.3、§11，F09/F12）。
--
-- 长期等待、历史增长和后台整理不会吞掉交互与核对能力：
--   - 为模型整理、索引、过期扫描、归档和清理设置独立可控预算，优先保障回执、
--     核对、取消和续租；
--   - 归档 Task/Run/材料前检查未决责任、证据依赖、法定/业务留存和授权；活跃
--     引用不被清除；
--   - 去重摘要/墓碑至少覆盖重投期和操作生命周期；未知保留期不擅自按短 TTL
--     清除；
--   - 后台工作支持重启、幂等、进度水位及有界失败重试，避免全表长事务；
--   - 持续运行期间按策略失效、过期、源不可用和磁盘压力展示状态，不静默停摆。

CREATE TABLE IF NOT EXISTS background_budgets (
  budget_key TEXT PRIMARY KEY,
  -- 独立可控预算（模型整理、索引、过期扫描、归档和清理）。
  kind TEXT NOT NULL CHECK(kind IN ('model_compaction','indexing','expiry_scan','archive','cleanup')),
  -- 预算（独立可控，优先保障回执、核对、取消和续租）。
  max_units INTEGER NOT NULL DEFAULT 1,
  max_bytes INTEGER NOT NULL DEFAULT 0,
  max_time_ms INTEGER NOT NULL DEFAULT 0,
  -- 优先级（优先保障回执、核对、取消和续租）。
  priority TEXT NOT NULL CHECK(priority IN ('critical','normal','background')) DEFAULT 'background',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS retention_policies (
  policy_key TEXT PRIMARY KEY,
  -- 对象类型与保留期。
  object_kind TEXT NOT NULL,
  retention_ms INTEGER NOT NULL DEFAULT 0,
  -- 法定/业务留存（归档前检查未决责任、证据依赖、法定/业务留存和授权）。
  legal_hold INTEGER NOT NULL DEFAULT 0 CHECK(legal_hold IN (0,1)),
  business_hold INTEGER NOT NULL DEFAULT 0 CHECK(business_hold IN (0,1)),
  -- 去重摘要/墓碑至少覆盖重投期和操作生命周期。
  tombstone_ms INTEGER NOT NULL DEFAULT 0,
  -- 未知保留期不擅自按短 TTL 清除（0 表示未知，不擅自清除）。
  unknown_retention_ms INTEGER NOT NULL DEFAULT 0,
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 归档记录（归档 Task/Run/材料前检查未决责任、证据依赖、法定/业务留存和授权）。
CREATE TABLE IF NOT EXISTS archive_records (
  archive_id TEXT PRIMARY KEY,
  -- 对象与归档。
  object_ref TEXT NOT NULL,
  object_kind TEXT NOT NULL,
  -- 归档前检查（未决责任、证据依赖、法定/业务留存和授权）。
  pending_responsibilities TEXT NOT NULL DEFAULT '[]',
  evidence_dependencies TEXT NOT NULL DEFAULT '[]',
  legal_hold INTEGER NOT NULL DEFAULT 0 CHECK(legal_hold IN (0,1)),
  business_hold INTEGER NOT NULL DEFAULT 0 CHECK(business_hold IN (0,1)),
  authorized INTEGER NOT NULL DEFAULT 0 CHECK(authorized IN (0,1)),
  -- 状态（archived/pending/blocked）。
  state TEXT NOT NULL CHECK(state IN ('archived','pending','blocked')) DEFAULT 'pending',
  -- 归档者与时间。
  archived_by TEXT NOT NULL DEFAULT '',
  archived_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS archives_by_object ON archive_records(object_ref, state);
