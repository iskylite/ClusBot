-- P4-02：资产发现身份、部件和外部映射（V3 §6.3、E01/E03）。
--
-- 准确关联节点、可更换部件与外部系统身份，不靠 IP 合并历史：
--   - 使用稳定内部 ID，外部映射含来源系统实例、外部类型/ID、设备代际及
--     有效区间；
--   - 发现先保存 identity_claim；可信无矛盾规则可归一，名称/IP/相似度仅作候选；
--   - IP/名称变化保持资产身份；更换网卡等部件创建新部件并结束旧安装关系，
--     节点身份连续性不明时待核对；
--   - 支持误合并/拆分的带证据修订，保留旧引用并触发相关验证依赖复查；
--   - 身份冲突只阻止相关对象写动作，不阻断其他已确认资产的工作。

CREATE TABLE IF NOT EXISTS asset_external_mappings (
  mapping_id TEXT PRIMARY KEY,
  -- 稳定内部 ID（不靠 IP 合并历史）。
  object_ref TEXT NOT NULL,
  -- 外部映射：来源系统实例、外部类型/ID、设备代际及有效区间。
  source_system TEXT NOT NULL,
  source_instance TEXT NOT NULL,
  external_type TEXT NOT NULL,
  external_id TEXT NOT NULL,
  device_generation INTEGER NOT NULL DEFAULT 1,
  valid_from INTEGER NOT NULL,
  valid_to INTEGER NOT NULL DEFAULT 0,  -- 0 表示仍有效
  -- 创建。
  created_at INTEGER NOT NULL,
  UNIQUE(source_system, source_instance, external_type, external_id, device_generation)
);

CREATE INDEX IF NOT EXISTS mappings_by_object ON asset_external_mappings(object_ref, valid_from);
CREATE INDEX IF NOT EXISTS mappings_by_external ON asset_external_mappings(source_system, external_id, valid_to);

-- 身份声明（发现先保存 identity_claim；可信无矛盾规则可归一）。
CREATE TABLE IF NOT EXISTS identity_claims (
  claim_id TEXT PRIMARY KEY,
  -- 声明的身份候选（名称/IP/相似度仅作候选）。
  candidate_object_ref TEXT NOT NULL,
  candidate_identity TEXT NOT NULL,  -- IP/名称/序列号等
  identity_kind TEXT NOT NULL,       -- ip/name/serial/similarity
  -- 声明来源与可信度。
  claimed_by TEXT NOT NULL,
  confidence TEXT NOT NULL CHECK(confidence IN ('trusted','candidate','untrusted')) DEFAULT 'candidate',
  -- 归一（可信无矛盾规则可归一）。
  unified_to TEXT,
  -- 创建。
  claimed_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS claims_by_candidate ON identity_claims(candidate_object_ref, confidence);
CREATE INDEX IF NOT EXISTS claims_by_identity ON identity_claims(candidate_identity, identity_kind);

-- 部件安装关系（更换部件创建新部件并结束旧安装关系）。
CREATE TABLE IF NOT EXISTS component_installations (
  installation_id TEXT PRIMARY KEY,
  -- 节点与部件。
  node_ref TEXT NOT NULL,
  component_ref TEXT NOT NULL,
  component_kind TEXT NOT NULL,      -- nic/disk/...
  -- 安装时间边界（结束旧安装关系）。
  installed_at INTEGER NOT NULL,
  removed_at INTEGER NOT NULL DEFAULT 0,
  -- 节点身份连续性（不明时待核对）。
  node_identity_continuous TEXT NOT NULL DEFAULT 'unknown' CHECK(node_identity_continuous IN ('confirmed','unknown','broken')),
  -- 创建。
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS installations_by_node ON component_installations(node_ref, removed_at);
CREATE INDEX IF NOT EXISTS installations_by_component ON component_installations(component_ref, removed_at);

-- 身份修订（误合并/拆分的带证据修订）。
CREATE TABLE IF NOT EXISTS identity_corrections (
  correction_id TEXT PRIMARY KEY,
  -- 修订类型（merge_split/identity_change）。
  kind TEXT NOT NULL CHECK(kind IN ('merge_split','identity_change')),
  -- 旧引用与新引用（保留旧引用）。
  old_object_ref TEXT NOT NULL,
  new_object_ref TEXT NOT NULL,
  -- 证据与复查依赖。
  evidence_ref TEXT NOT NULL,
  verification_dependencies TEXT NOT NULL DEFAULT '[]',
  -- 创建者与时间。
  corrected_by TEXT NOT NULL,
  corrected_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS corrections_by_object ON identity_corrections(old_object_ref, new_object_ref);
