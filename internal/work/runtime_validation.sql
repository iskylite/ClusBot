-- 配置规范、验证证据与交付覆盖（§6.5—6.7、E01/E03、F10）。
--
-- 业务验收逐要求、逐实际对象核对，可定位缺项与失效原因：
--   - 固定规范/规则版本、实际节点集合及对象组拓扑；参数测量定义与业务阈值
--     分离；
--   - 保存验证记录的输入条件、执行配置、样本范围、结果、来源和适用版本；
--     不能仅存布尔通过；
--   - 按要求 × 对象/组建立覆盖表；不适用项须有规范依据，未测/未知不能默认
--     通过；
--   - 关系、部件、固件或规范变化时复查依赖，局部失效不抹掉无关结果；影响
--     不明保留待复核；
--   - 验收通过、准备交付和真实接收交接分别记录，最后才可决定在役。

CREATE TABLE IF NOT EXISTS validation_records (
  validation_id TEXT PRIMARY KEY,
  -- 固定规范/规则版本与实际节点集合。
  spec_ref TEXT NOT NULL,
  rule_version INTEGER NOT NULL,
  object_refs TEXT NOT NULL,        -- JSON：实际节点集合
  object_group_topology TEXT NOT NULL DEFAULT '{}',
  -- 输入条件、执行配置、样本范围、结果、来源和适用版本（不能仅存布尔通过）。
  input_conditions TEXT NOT NULL DEFAULT '{}',
  execution_config TEXT NOT NULL DEFAULT '{}',
  sample_scope TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  applicable_versions TEXT NOT NULL DEFAULT '[]',
  -- 参数测量定义与业务阈值分离。
  measurement_definition TEXT NOT NULL DEFAULT '{}',
  business_threshold TEXT NOT NULL DEFAULT '{}',
  -- 结果状态（passed/failed/unknown/pending；未测/未知不能默认通过）。
  outcome TEXT NOT NULL CHECK(outcome IN ('passed','failed','unknown','pending')) DEFAULT 'pending',
  -- 验证者与时间。
  validated_by TEXT NOT NULL,
  validated_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS validations_by_spec ON validation_records(spec_ref, rule_version);
CREATE INDEX IF NOT EXISTS validations_by_outcome ON validation_records(outcome, validated_at);

-- 验收覆盖表（按要求 × 对象/组建立覆盖表）。
CREATE TABLE IF NOT EXISTS acceptance_coverage (
  coverage_id TEXT PRIMARY KEY,
  -- 要求与对象/组。
  requirement_key TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  -- 覆盖状态（covered/not_applicable/uncovered/unknown）。
  coverage_state TEXT NOT NULL CHECK(coverage_state IN ('covered','not_applicable','uncovered','unknown')) DEFAULT 'unknown',
  -- 不适用项须有规范依据。
  not_applicable_basis TEXT NOT NULL DEFAULT '',
  -- 验证引用。
  validation_ref TEXT,
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(requirement_key, object_ref)
);

CREATE INDEX IF NOT EXISTS coverage_by_requirement ON acceptance_coverage(requirement_key, coverage_state);
CREATE INDEX IF NOT EXISTS coverage_by_object ON acceptance_coverage(object_ref, coverage_state);

-- 证据依赖（关系、部件、固件或规范变化时复查依赖）。
CREATE TABLE IF NOT EXISTS evidence_dependencies (
  dependency_id TEXT PRIMARY KEY,
  -- 依赖类型（relation/component/firmware/spec）。
  dependency_kind TEXT NOT NULL CHECK(dependency_kind IN ('relation','component','firmware','spec')),
  -- 依赖目标与验证引用。
  target_ref TEXT NOT NULL,
  validation_ref TEXT NOT NULL,
  -- 失效（局部失效不抹掉无关结果）。
  invalidated INTEGER NOT NULL DEFAULT 0 CHECK(invalidated IN (0,1)),
  invalidated_reason TEXT NOT NULL DEFAULT '',
  -- 创建与失效时间。
  created_at INTEGER NOT NULL,
  invalidated_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS dependencies_by_target ON evidence_dependencies(target_ref, dependency_kind);
CREATE INDEX IF NOT EXISTS dependencies_by_validation ON evidence_dependencies(validation_ref, invalidated);

-- 交付记录（验收通过、准备交付和真实接收交接分别记录）。
CREATE TABLE IF NOT EXISTS delivery_records (
  delivery_id TEXT PRIMARY KEY,
  -- 对象与阶段（验收通过/准备交付/真实接收交接分别记录）。
  object_ref TEXT NOT NULL,
  stage TEXT NOT NULL CHECK(stage IN ('accepted','ready','handover','in_service')),
  -- 记录与证据。
  record_ref TEXT NOT NULL DEFAULT '',
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  -- 时间。
  occurred_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS delivery_by_object ON delivery_records(object_ref, stage, occurred_at);

-- 设计交付六阶段事件（发现/配置/单机测试/联机测试/验收/交接）。
-- 与 delivery_records 的四阶段生命周期记录并存：这里是面向交付作业视图的阶段日志。
CREATE TABLE IF NOT EXISTS delivery_stage_events (
  event_id TEXT PRIMARY KEY,
  object_ref TEXT NOT NULL,
  stage TEXT NOT NULL CHECK(stage IN ('discovered','configured','unit_test','group_test','accepted','handover')),
  record_ref TEXT NOT NULL DEFAULT '',
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  occurred_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_stage_by_object ON delivery_stage_events(object_ref, stage, occurred_at);
