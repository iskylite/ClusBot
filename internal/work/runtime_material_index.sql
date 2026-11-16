-- P5-01：大日志材料索引与有界片段读取（V3 §6.5、§7.2/7.5、E06、F07/F10/F12）。
--
-- 大文本不拖垮权威事务或模型输入，仍能回查完整证据范围：
--   - 按来源/对象/时间/部件/错误类别建立索引和片段定位，保留原文摘要、编码、
--     行/字节/时间区间及覆盖；
--   - 原文独立存储或保留授权外部句柄；索引、摘要和去重结果引用材料版本，
--     不复制整篇到图属性；
--   - 解析/索引由有预算的后台单元完成，提交前检查源版本和租约；新材料权威
--     可读，索引滞后明确显示；
--   - 旧数据库材料按兼容方式读取或迁移，未核实来源权限不能自动扩大跨任务
--     可见性；
--   - 限制候选字节、单次片段、总读取量和时间；相同原文/摘要不作为多份独立
--     证据。

CREATE TABLE IF NOT EXISTS material_indexes (
  index_id TEXT PRIMARY KEY,
  -- 材料引用与版本（索引、摘要和去重结果引用材料版本）。
  material_id TEXT NOT NULL,
  material_version INTEGER NOT NULL DEFAULT 1,
  -- 按来源/对象/时间/部件/错误类别建立索引和片段定位。
  source_ref TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  time_range_start INTEGER NOT NULL DEFAULT 0,
  time_range_end INTEGER NOT NULL DEFAULT 0,
  component_ref TEXT NOT NULL DEFAULT '',
  error_kind TEXT NOT NULL DEFAULT '',
  -- 片段定位（保留原文摘要、编码、行/字节/时间区间及覆盖）。
  excerpt_locator TEXT NOT NULL DEFAULT '',  -- JSON：行/字节/时间区间
  excerpt_digest TEXT NOT NULL,
  excerpt_bytes INTEGER NOT NULL DEFAULT 0,
  coverage TEXT NOT NULL DEFAULT '',
  -- 索引状态（building/ready/stale）。
  state TEXT NOT NULL CHECK(state IN ('building','ready','stale')) DEFAULT 'building',
  -- 滞后显示（新材料权威可读，索引滞后明确显示）。
  lag_ms INTEGER NOT NULL DEFAULT 0,
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(material_id, material_version, error_kind)
);

CREATE INDEX IF NOT EXISTS indexes_by_object ON material_indexes(object_ref, state);
CREATE INDEX IF NOT EXISTS indexes_by_source ON material_indexes(source_ref, material_version);
CREATE INDEX IF NOT EXISTS indexes_by_error ON material_indexes(error_kind, state);

-- 材料版本（原文变化、删除或撤权后旧索引不能放行失权内容）。
CREATE TABLE IF NOT EXISTS material_versions (
  material_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  -- 摘要与撤权。
  content_digest TEXT NOT NULL,
  revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
  -- 创建。
  created_at INTEGER NOT NULL,
  PRIMARY KEY (material_id, version)
);

CREATE INDEX IF NOT EXISTS versions_by_material ON material_versions(material_id, revoked);
