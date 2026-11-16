-- P4-01：版本化领域包和类型约束（V3 §6.2/6.6、F10）。
--
-- 领域定义可以发布、引用和回退，并解释历史工作：
--   - 定义对象/关系类型、字段类型与单位、基数、身份规则、数据绑定、配置/
--     测试规范及流程引用；
--   - 草稿校验引用闭合、关系端点、单位、参数范围、互斥目标和样例；发布为
--     不可变版本；
--   - Task/Step 固定使用版本；规则发布不静默迁移运行中任务，紧急限制和
--     当前授权仍即时核验；
--   - 更改定义需记录影响、兼容性及回退依据；旧记录保留原版本；
--   - 提供声明式规则/规范加载和查询入口，禁止让模型生成规则后直接激活。

CREATE TABLE IF NOT EXISTS domain_packages (
  package_id TEXT PRIMARY KEY,
  -- 包标识与版本。
  name TEXT NOT NULL,
  version INTEGER NOT NULL,
  -- 状态（draft/published/superseded；发布为不可变版本）。
  state TEXT NOT NULL CHECK(state IN ('draft','published','superseded')) DEFAULT 'draft',
  -- 定义内容（对象/关系类型、字段类型与单位、基数、身份规则、数据绑定、
  -- 配置/测试规范及流程引用）。
  definitions TEXT NOT NULL,        -- JSON：完整领域定义
  definitions_digest TEXT NOT NULL,
  -- 影响、兼容性及回退依据。
  compatibility TEXT NOT NULL DEFAULT '',
  impact_summary TEXT NOT NULL DEFAULT '',
  rollback_basis TEXT NOT NULL DEFAULT '',
  -- 发布者与时间。
  published_by TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS packages_by_name ON domain_packages(name, state, version);

-- 对象类型定义（对象/关系类型、字段类型与单位、基数、身份规则）。
CREATE TABLE IF NOT EXISTS domain_object_types (
  package_id TEXT NOT NULL REFERENCES domain_packages(package_id) ON DELETE CASCADE,
  type_key TEXT NOT NULL,
  -- 字段类型与单位。
  fields TEXT NOT NULL DEFAULT '[]',  -- JSON：字段定义（名称/类型/单位/必需）
  -- 基数与身份规则。
  cardinality TEXT NOT NULL DEFAULT '{}',
  identity_rule TEXT NOT NULL DEFAULT '',
  -- 数据绑定与配置/测试规范。
  data_binding TEXT NOT NULL DEFAULT '{}',
  config_spec TEXT NOT NULL DEFAULT '{}',
  test_spec TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (package_id, type_key)
);

-- 关系类型定义（关系端点、基数、互斥目标）。
CREATE TABLE IF NOT EXISTS domain_relation_types (
  package_id TEXT NOT NULL REFERENCES domain_packages(package_id) ON DELETE CASCADE,
  relation_key TEXT NOT NULL,
  -- 关系端点（源/目标类型）。
  source_type TEXT NOT NULL,
  target_type TEXT NOT NULL,
  -- 基数与互斥目标。
  cardinality TEXT NOT NULL DEFAULT '{}',
  mutually_exclusive_with TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (package_id, relation_key)
);

-- 流程引用（领域流程引用）。
CREATE TABLE IF NOT EXISTS domain_process_refs (
  package_id TEXT NOT NULL REFERENCES domain_packages(package_id) ON DELETE CASCADE,
  process_key TEXT NOT NULL,
  -- 流程引用与步骤。
  ref TEXT NOT NULL,
  steps TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (package_id, process_key)
);
