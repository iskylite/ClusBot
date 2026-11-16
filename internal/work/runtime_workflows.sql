-- P4-07：版本化运维流程模板与运行时绑定（V3 §4.2、§6.6、§8.1）。
--
-- 流程提供领域顺序和条件，运行时承担执行、等待、取消与恢复：
--   - 模板定义触发、对象范围、前置、确定步骤/调查步骤、依赖、等待、人工
--     协作、失败收尾和验收；
--   - 受理时绑定领域包/模板/规范/目标版本，转换成 Step 与依赖；模型只在
--     允许调查节点提出补查；
--   - 明确原子能力、委托整段流程和持续服务三种粒度，防止本地与外部重复
--     调度；
--   - 模板变化不静默改运行中工作；迁移需合同修订、依赖影响评估和必要重新
--     确认；
--   - 为领域可自动判定的步骤提供确定性处理器；不把每个流程都变为模型循环
--     或 Goal。

CREATE TABLE IF NOT EXISTS workflow_templates (
  template_id TEXT PRIMARY KEY,
  -- 模板标识与版本。
  name TEXT NOT NULL,
  version INTEGER NOT NULL,
  -- 触发、对象范围、前置、确定步骤/调查步骤、依赖、等待、人工协作、失败
  -- 收尾和验收。
  trigger_kind TEXT NOT NULL,
  object_scope TEXT NOT NULL DEFAULT '{}',
  preconditions TEXT NOT NULL DEFAULT '[]',
  steps TEXT NOT NULL,              -- JSON：步骤定义（确定/调查/依赖/等待/人工/失败/验收）
  -- 粒度（原子能力/委托整段流程/持续服务）。
  granularity TEXT NOT NULL CHECK(granularity IN ('atomic_capability','delegated_workflow','continuous_service')),
  -- 状态（draft/published/superseded）。
  state TEXT NOT NULL CHECK(state IN ('draft','published','superseded')) DEFAULT 'draft',
  -- 发布者与时间。
  published_by TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS templates_by_name ON workflow_templates(name, state, version);

-- 运行时绑定（受理时绑定领域包/模板/规范/目标版本）。
CREATE TABLE IF NOT EXISTS workflow_bindings (
  binding_id TEXT PRIMARY KEY,
  -- 绑定的运行时 Task 与模板。
  task_id TEXT NOT NULL,
  template_id TEXT NOT NULL REFERENCES workflow_templates(template_id),
  -- 绑定领域包/模板/规范/目标版本。
  domain_package_ref TEXT NOT NULL,
  template_version INTEGER NOT NULL,
  spec_refs TEXT NOT NULL DEFAULT '[]',
  target_versions TEXT NOT NULL DEFAULT '{}',
  -- 转换的 Step 与依赖（受理时转换成 Step 与依赖）。
  step_ids TEXT NOT NULL DEFAULT '[]',
  dependency_ids TEXT NOT NULL DEFAULT '[]',
  -- 状态（active/completed/cancelled）。
  state TEXT NOT NULL CHECK(state IN ('active','completed','cancelled')) DEFAULT 'active',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS bindings_by_task ON workflow_bindings(task_id, state);
CREATE INDEX IF NOT EXISTS bindings_by_template ON workflow_bindings(template_id, template_version);

-- 模板变化影响评估（模板变化不静默改运行中工作）。
CREATE TABLE IF NOT EXISTS workflow_template_changes (
  change_id TEXT PRIMARY KEY,
  -- 模板与版本。
  template_id TEXT NOT NULL,
  from_version INTEGER NOT NULL,
  to_version INTEGER NOT NULL,
  -- 影响评估与合同修订。
  impact_assessment TEXT NOT NULL DEFAULT '',
  contract_revision_required INTEGER NOT NULL DEFAULT 1 CHECK(contract_revision_required IN (0,1)),
  reconfirmation_required INTEGER NOT NULL DEFAULT 0 CHECK(reconfirmation_required IN (0,1)),
  -- 创建。
  assessed_by TEXT NOT NULL,
  assessed_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS changes_by_template ON workflow_template_changes(template_id, from_version, to_version);
