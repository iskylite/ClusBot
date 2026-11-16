-- P5-06：经历、案例候选与经验修订（V3 §7.4/7.7、§10.4、E06）。
--
-- 把运维经历提炼为有条件、有反例的候选，不把模型总结变成当前事实：
--   - 经历引用症状、对象/环境、假设、实际动作、独立结果、未知项和来源；
--     成功/失败都可形成候选；
--   - 案例保存适用条件、禁用/失效条件、反例、证据和复核时间；未核实根因
--     不写已证实；
--   - 发布按既有审查策略及权限决定，版本不可变；更正/失效/暂停保留历史
--     和引用；
--   - 用对象型号/固件/拓扑/任务类型和权限过滤，不仅匹配目标文本；相似只
--     产生候选；
--   - 记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件。

CREATE TABLE IF NOT EXISTS operational_cases (
  case_id TEXT PRIMARY KEY,
  -- 经历引用（症状、对象/环境、假设、实际动作、独立结果、未知项和来源）。
  symptom TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  environment TEXT NOT NULL DEFAULT '{}',
  hypothesis TEXT NOT NULL DEFAULT '',
  actual_action TEXT NOT NULL DEFAULT '',
  independent_outcome TEXT NOT NULL DEFAULT '',
  unknowns TEXT NOT NULL DEFAULT '[]',
  source_refs TEXT NOT NULL DEFAULT '[]',
  -- 适用条件、禁用/失效条件、反例、证据和复核时间。
  applicability TEXT NOT NULL DEFAULT '{}',
  contraindications TEXT NOT NULL DEFAULT '[]',
  invalidation_conditions TEXT NOT NULL DEFAULT '[]',
  counterexamples TEXT NOT NULL DEFAULT '[]',
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  review_at INTEGER NOT NULL DEFAULT 0,
  -- 根因（未核实根因不写已证实）。
  root_cause TEXT NOT NULL DEFAULT '',
  root_cause_proven INTEGER NOT NULL DEFAULT 0 CHECK(root_cause_proven IN (0,1)),
  -- 状态（candidate/published/suspended/invalidated）。
  state TEXT NOT NULL CHECK(state IN ('candidate','published','suspended','invalidated')) DEFAULT 'candidate',
  -- 版本（发布按既有审查策略及权限决定，版本不可变）。
  version INTEGER NOT NULL DEFAULT 1,
  -- 更正/失效/暂停保留历史和引用。
  supersedes TEXT,
  superseded_reason TEXT NOT NULL DEFAULT '',
  -- 发布者与时间。
  published_by TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS cases_by_state ON operational_cases(state, created_at);
CREATE INDEX IF NOT EXISTS cases_by_object ON operational_cases(object_ref, state);
CREATE INDEX IF NOT EXISTS cases_by_symptom ON operational_cases(symptom);

-- 案例事件（记录召回、过滤、实际装入、被引用和独立验证有效/无效分别发生的事件）。
CREATE TABLE IF NOT EXISTS case_events (
  event_id TEXT PRIMARY KEY,
  case_id TEXT NOT NULL REFERENCES operational_cases(case_id) ON DELETE CASCADE,
  -- 事件类型（recalled/filtered/assembled/cited/verified_valid/verified_invalid）。
  kind TEXT NOT NULL CHECK(kind IN ('recalled','filtered','assembled','cited','verified_valid','verified_invalid')),
  -- 上下文（任务/对象）。
  task_id TEXT NOT NULL DEFAULT '',
  object_ref TEXT NOT NULL DEFAULT '',
  -- 证据与结果。
  evidence_ref TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  -- 时间。
  occurred_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS case_events_by_case ON case_events(case_id, kind, occurred_at);
CREATE INDEX IF NOT EXISTS case_events_by_task ON case_events(task_id, occurred_at);
