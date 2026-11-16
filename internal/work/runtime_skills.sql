-- P5-07：版本化 Skill 与受控改进（V3 §7.4/7.8、§9、E06、F10）。
--
-- 经验可提出方法改进，但不能自动改变生产规则、权限和执行能力：
--   - Skill 明确目标、前提、步骤、验证、限制和适用环境，引用已发布能力/
--     领域规范版本；
--   - 从案例产生改进候选，在受控案例和反例上回放，记录前后差异及失败；
--   - 发布/启用按既有授权策略处理，不能让模型自行审批自己的提案；运行中
--     任务不静默切换版本；
--   - 恢复和执行前重新核验 Skill/来源/能力版本；失效后停止后续新使用并
--     保留历史解释；
--   - 优化目标不包含放宽权限、跳过证据或隐藏未知；必要时回退到旧方法或
--     人工处理。

CREATE TABLE IF NOT EXISTS v3_skills (
  skill_id TEXT PRIMARY KEY,
  -- 目标、前提、步骤、验证、限制和适用环境。
  name TEXT NOT NULL,
  goal TEXT NOT NULL,
  preconditions TEXT NOT NULL DEFAULT '[]',
  steps TEXT NOT NULL,
  verification TEXT NOT NULL DEFAULT '',
  limitations TEXT NOT NULL DEFAULT '[]',
  environment TEXT NOT NULL DEFAULT '{}',
  -- 引用已发布能力/领域规范版本。
  capability_refs TEXT NOT NULL DEFAULT '[]',
  domain_spec_refs TEXT NOT NULL DEFAULT '[]',
  -- 版本（版本化 Skill）。
  version INTEGER NOT NULL DEFAULT 1,
  -- 状态（draft/candidate/published/suspended/rejected）。
  state TEXT NOT NULL CHECK(state IN ('draft','candidate','published','suspended','rejected')) DEFAULT 'draft',
  -- 来源案例（从案例产生改进候选）。
  source_case_ref TEXT,
  -- 发布者与时间（发布/启用按既有授权策略处理）。
  published_by TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS skills_by_name ON v3_skills(name, state, version);

-- Skill 回放（从案例产生改进候选，在受控案例和反例上回放）。
CREATE TABLE IF NOT EXISTS skill_replays (
  replay_id TEXT PRIMARY KEY,
  -- Skill 与回放案例/反例。
  skill_id TEXT NOT NULL REFERENCES v3_skills(skill_id) ON DELETE CASCADE,
  case_ref TEXT NOT NULL,
  is_counterexample INTEGER NOT NULL DEFAULT 0 CHECK(is_counterexample IN (0,1)),
  -- 前后差异及失败。
  before_outcome TEXT NOT NULL DEFAULT '',
  after_outcome TEXT NOT NULL DEFAULT '',
  diff TEXT NOT NULL DEFAULT '',
  failure_reason TEXT NOT NULL DEFAULT '',
  -- 结果（improved/degraded/unchanged/failed）。
  outcome TEXT NOT NULL CHECK(outcome IN ('improved','degraded','unchanged','failed')) DEFAULT 'unchanged',
  -- 时间。
  replayed_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS replays_by_skill ON skill_replays(skill_id, outcome);
