-- 观测、事实和关系的双时间历史（§6.3—6.4、F03/F10）。
--
-- 区分现实生效时间与系统获知时间，冲突和迟到不破坏历史：
--   - 原始 Observation 保存来源/采集时间/覆盖；规范声明保留值、单位、来源、
--     有效时间和记录时间；
--   - 按字段与用途选择权威来源，冲突保留双方及判据；不能按最后到达或相似度
--     胜出；
--   - 更正新增修订并关闭原记录的适用区间，不覆盖历史；重复事件依据来源
--     身份/版本去重；
--   - 物理组成无环、单一父关系的有效区间不重叠；网络拓扑允许环，端口连线
--     规范端点并支持双向查；
--   - 当前事实投影可重建并带水位，过期或缺失不被伪装为新鲜；权限/状态准入
--     回权威记录核验。

CREATE TABLE IF NOT EXISTS observations (
  observation_id TEXT PRIMARY KEY,
  -- 来源与采集时间。
  source_ref TEXT NOT NULL,
  source_identity TEXT NOT NULL,
  collected_at INTEGER NOT NULL,
  -- 覆盖。
  coverage TEXT NOT NULL DEFAULT '',
  -- 对象与字段。
  object_ref TEXT NOT NULL,
  field_key TEXT NOT NULL,
  -- 值与单位。
  value TEXT NOT NULL,
  unit TEXT NOT NULL DEFAULT '',
  -- 记录时间（系统获知时间）。
  recorded_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS observations_by_object ON observations(object_ref, field_key, collected_at);
CREATE INDEX IF NOT EXISTS observations_by_source ON observations(source_ref, collected_at);

-- 规范声明（保留值、单位、来源、有效时间和记录时间）。
CREATE TABLE IF NOT EXISTS facts (
  fact_id TEXT PRIMARY KEY,
  -- 对象与字段。
  object_ref TEXT NOT NULL,
  field_key TEXT NOT NULL,
  -- 值与单位。
  value TEXT NOT NULL,
  unit TEXT NOT NULL DEFAULT '',
  -- 来源与权威（按字段与用途选择权威来源）。
  source_ref TEXT NOT NULL,
  authority TEXT NOT NULL DEFAULT 'unknown' CHECK(authority IN ('authoritative','candidate','conflicting','unknown')),
  -- 双时间：有效时间（现实生效）与记录时间（系统获知）。
  valid_from INTEGER NOT NULL,
  valid_to INTEGER NOT NULL DEFAULT 0,  -- 0 表示仍有效
  recorded_at INTEGER NOT NULL,
  -- 修订（更正新增修订并关闭原记录的适用区间）。
  revision INTEGER NOT NULL DEFAULT 1,
  supersedes TEXT,                  -- 被本修订关闭的原记录
  -- 冲突保留（双方及判据）。
  conflict_with TEXT,
  conflict_basis TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS facts_by_object ON facts(object_ref, field_key, valid_from, valid_to);
CREATE INDEX IF NOT EXISTS facts_by_authority ON facts(object_ref, field_key, authority);

-- 关系（物理组成无环、单一父关系；网络拓扑允许环）。
CREATE TABLE IF NOT EXISTS relations (
  relation_id TEXT PRIMARY KEY,
  -- 关系类型与端点。
  relation_kind TEXT NOT NULL CHECK(relation_kind IN ('physical_parent','physical_component','network_link')),
  source_ref TEXT NOT NULL,
  target_ref TEXT NOT NULL,
  -- 有效区间（单一父关系的有效区间不重叠）。
  valid_from INTEGER NOT NULL,
  valid_to INTEGER NOT NULL DEFAULT 0,
  -- 创建。
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS relations_by_source ON relations(source_ref, relation_kind, valid_from, valid_to);
CREATE INDEX IF NOT EXISTS relations_by_target ON relations(target_ref, relation_kind, valid_from, valid_to);

-- 当前事实投影（可重建并带水位）。
CREATE TABLE IF NOT EXISTS fact_projections (
  projection_id INTEGER PRIMARY KEY CHECK(projection_id=1),
  -- 水位（重建到的时间点）。
  watermark INTEGER NOT NULL DEFAULT 0,
  -- 重建时间。
  rebuilt_at INTEGER NOT NULL DEFAULT 0
);
