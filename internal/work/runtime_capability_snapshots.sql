-- P3-05：动态 Schema 发现与不可变执行快照（V3 §8.2，F10）。
--
-- 多字段、动态配置和测试参数由提供者描述，实例执行依据可恢复：
--   - 读取授权范围内目录、配置/测试/输入/输出 Schema、单位、依赖、默认值、
--     环境版本与有效期；
--   - 校验方言、大小、深度与参数；禁止自动访问任意远程 $ref；动态选项缺失
--     时返回缺口，不猜值；
--   - 选择实现后冻结具体参数、规范化摘要算法版本、Schema/连接/能力/凭据绑定
--     与实际目标清单；
--   - 领域阈值与验收标准单独引用已发布规范；提供者建议值不自动覆盖业务规则；
--   - 派发前重查当前权限/目标/连接版本；未派发不匹配即停，已派发按原快照核对。

CREATE TABLE IF NOT EXISTS capability_snapshots (
  snapshot_id TEXT PRIMARY KEY,
  -- 能力/连接/版本绑定。
  capability_key TEXT NOT NULL,
  capability_version INTEGER NOT NULL,
  connection_id TEXT NOT NULL,
  connection_version INTEGER NOT NULL,
  credential_ref TEXT NOT NULL DEFAULT '',
  -- 冻结的 Schema 与参数（不可变执行快照）。
  input_schema TEXT NOT NULL,
  output_schema TEXT NOT NULL,
  frozen_params TEXT NOT NULL,      -- JSON：冻结的具体参数
  params_digest TEXT NOT NULL,      -- 规范化摘要（同键不同参数摘要拒绝）
  digest_algorithm TEXT NOT NULL DEFAULT 'sha256',
  -- 目标清单（含各对象 revision/epoch）。
  target_refs TEXT NOT NULL DEFAULT '[]',
  -- 环境版本与有效期。
  environment_version TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 创建。
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS snapshots_by_capability ON capability_snapshots(capability_key, capability_version);
CREATE INDEX IF NOT EXISTS snapshots_by_params ON capability_snapshots(params_digest);

-- Schema 发现记录（读取授权范围内的 Schema，校验后保存）。
CREATE TABLE IF NOT EXISTS schema_discoveries (
  discovery_id TEXT PRIMARY KEY,
  capability_key TEXT NOT NULL,
  -- 发现的 Schema 与元数据。
  input_schema TEXT NOT NULL DEFAULT '{}',
  output_schema TEXT NOT NULL DEFAULT '{}',
  units TEXT NOT NULL DEFAULT '{}',
  defaults TEXT NOT NULL DEFAULT '{}',
  dependencies TEXT NOT NULL DEFAULT '[]',
  environment_version TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  -- 校验结果（方言/大小/深度/参数）。
  validation_outcome TEXT NOT NULL CHECK(validation_outcome IN ('valid','invalid','gap')),
  validation_detail TEXT NOT NULL DEFAULT '',
  -- 缺口（动态选项缺失返回缺口，不猜值）。
  gaps TEXT NOT NULL DEFAULT '[]',
  discovered_at INTEGER NOT NULL,
  discovered_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS discoveries_by_capability ON schema_discoveries(capability_key, discovered_at);
