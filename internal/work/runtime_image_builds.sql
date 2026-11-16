-- P4-09：母盘构建与版本核验（V3 §10.2、E01、F02/F10）。
--
-- 部署使用明确、可追溯且适用于目标的母盘版本：
--   - 表达母盘 ID/版本、摘要、适用架构/硬件、依赖、构建参数、来源和验证
--     证据；
--   - 构建作为受控 Operation；使用已有母盘则核验存在性、完整性、来源权限
--     与适用规范，不伪造构建步骤；
--   - 异步构建用 Job/Wait，结果材料独立存储；参数或目标规范变化产生新的
--     版本/操作；
--   - 保留失败/部分构建和未决清理责任，构建回执不自动成为部署验收；
--   - 流程中明确"构建"或"引用并核验"的选择及不适用依据。

CREATE TABLE IF NOT EXISTS golden_images (
  image_id TEXT PRIMARY KEY,
  -- 母盘 ID/版本、摘要、适用架构/硬件。
  image_key TEXT NOT NULL,
  version INTEGER NOT NULL,
  content_digest TEXT NOT NULL,
  architecture TEXT NOT NULL,
  hardware_requirements TEXT NOT NULL DEFAULT '{}',
  -- 依赖、构建参数、来源和验证证据。
  dependencies TEXT NOT NULL DEFAULT '[]',
  build_params TEXT NOT NULL DEFAULT '{}',
  source_ref TEXT NOT NULL,
  validation_evidence TEXT NOT NULL DEFAULT '[]',
  -- 状态（building/ready/failed/partial）。
  state TEXT NOT NULL CHECK(state IN ('building','ready','failed','partial')) DEFAULT 'building',
  -- 构建 Job 与材料（异步构建用 Job/Wait，结果材料独立存储）。
  build_job_ref TEXT,
  build_materials TEXT NOT NULL DEFAULT '[]',
  -- 未决清理责任（失败/部分构建保留）。
  cleanup_responsibility TEXT NOT NULL DEFAULT '',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(image_key, version)
);

CREATE INDEX IF NOT EXISTS images_by_key ON golden_images(image_key, state, version);
CREATE INDEX IF NOT EXISTS images_by_architecture ON golden_images(architecture, state);

-- 母盘核验（使用已有母盘则核验存在性、完整性、来源权限与适用规范）。
CREATE TABLE IF NOT EXISTS image_verifications (
  verification_id TEXT PRIMARY KEY,
  -- 母盘与核验项。
  image_id TEXT NOT NULL REFERENCES golden_images(image_id) ON DELETE CASCADE,
  -- 存在性、完整性、来源权限与适用规范（不伪造构建步骤）。
  existence_verified INTEGER NOT NULL DEFAULT 0 CHECK(existence_verified IN (0,1)),
  integrity_verified INTEGER NOT NULL DEFAULT 0 CHECK(integrity_verified IN (0,1)),
  source_authorized INTEGER NOT NULL DEFAULT 0 CHECK(source_authorized IN (0,1)),
  applicable_spec_met INTEGER NOT NULL DEFAULT 0 CHECK(applicable_spec_met IN (0,1)),
  -- 核验结果（verified/failed/unknown）。
  outcome TEXT NOT NULL CHECK(outcome IN ('verified','failed','unknown')) DEFAULT 'unknown',
  -- 核验者与时间。
  verified_by TEXT NOT NULL,
  verified_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS verifications_by_image ON image_verifications(image_id, outcome);
