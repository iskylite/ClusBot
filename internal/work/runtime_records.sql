-- P1-02：Task、Step、Run 与控制记录的持久结构（V3 §4.1、§5.1）。
--
-- 本文件是 schema 48 新增的持久结构，与既有 schema.sql 一起构成完整结构。
-- 设计原则（P1-02）：
--   - Task 九态、Step、依赖、work_units、控制记录、结果引用及基础审计；
--     任务和所有原子消息写入同一 SQLite 文件。
--   - 使用 P0-02 的合同/控制/存储版本约定；Run 绑定 Task/Step/执行单元、
--     分配与执行代际，不以 Run 状态冒充 Task 状态。
--   - 保留旧 ID、成果/验收与历史 run_bindings；迁移不明工作为需要核对的
--     明确状态，记录迁移原因。
--   - 不可变合同快照和控制记录；重复请求键相同内容返回原结果，不同内容冲突。
--
-- 状态枚举的序列化与 P0-02 的 runtime_contract.go 完全一致，不各自发明。

-- Task 的 V3 九态权威记录。与既有 tasks 表并存：tasks 保留旧五态供
-- 兼容查询，task_states 是 V3 权威当前状态；P1-03 的状态机写这里。
CREATE TABLE IF NOT EXISTS task_states (
  task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  -- V3 九态之一（runtime_contract.go 的序列化）。
  state TEXT NOT NULL CHECK(state IN
    ('pending','running','waiting','paused','acceptance','finalizing','succeeded','failed','cancelled')),
  reason TEXT NOT NULL DEFAULT '',
  -- 进入当前状态时的存储行版本（乐观并发）与控制修订。
  storage_revision INTEGER NOT NULL,
  control_revision INTEGER NOT NULL DEFAULT 0,
  -- 迁移/判定原因（旧记录映射、人工例外、恢复核对等）。
  mapping_reason TEXT NOT NULL DEFAULT '',
  -- 是否需要恢复核对（旧记录映射结论为"待核对"）。
  reconcile_required INTEGER NOT NULL DEFAULT 0 CHECK(reconcile_required IN (0,1)),
  updated_at INTEGER NOT NULL
);

-- Step：Task 内可持久推进的步骤（不是子 Task）。
CREATE TABLE IF NOT EXISTS steps (
  step_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  -- 定义与计划版本（绑定合同修订，旧计划对新合同失效）。
  contract_revision INTEGER NOT NULL,
  definition_key TEXT NOT NULL,        -- 步骤定义标识（模型/确定/能力步骤）
  definition_version INTEGER NOT NULL, -- 定义版本
  -- 输入与结果引用（大内容走材料引用，不内嵌）。
  input_ref TEXT NOT NULL DEFAULT '',
  result_ref TEXT NOT NULL DEFAULT '',
  -- 状态与证据。
  state TEXT NOT NULL CHECK(state IN
    ('blocked','ready','running','waiting','completed','failed','cancelled')),
  -- 依赖证据版本：上游证据变化后须重新核对。
  evidence_revision INTEGER NOT NULL DEFAULT 0,
  -- 完成条件与错误。
  condition TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  optional INTEGER NOT NULL DEFAULT 0 CHECK(optional IN (0,1)),
  position INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(task_id, definition_key, contract_revision)
);

-- Step 依赖：无环 DAG，依赖证据版本须有效。
CREATE TABLE IF NOT EXISTS step_dependencies (
  step_id TEXT NOT NULL REFERENCES steps(step_id) ON DELETE CASCADE,
  depends_on_step_id TEXT NOT NULL REFERENCES steps(step_id) ON DELETE CASCADE,
  -- 依赖生效时的证据版本。
  evidence_revision INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (step_id, depends_on_step_id),
  CHECK(step_id <> depends_on_step_id)
);

-- work_units：可被 Worker 领取的就绪执行单元（模型/确定步骤/动作派发）。
CREATE TABLE IF NOT EXISTS work_units (
  unit_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  step_id TEXT REFERENCES steps(step_id) ON DELETE SET NULL,
  -- 单元类型：model（模型调用）、deterministic（确定步骤）、action（动作派发）。
  kind TEXT NOT NULL CHECK(kind IN ('model','deterministic','action')),
  -- 稳定身份与代际：单元租约代际（与任务分配代际不同）。
  lease_generation INTEGER NOT NULL DEFAULT 0,
  -- 就绪与到期。
  ready_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('ready','claimed','running','completed','failed','cancelled')),
  -- 输入/结果引用与摘要。
  input_ref TEXT NOT NULL DEFAULT '',
  result_ref TEXT NOT NULL DEFAULT '',
  result_digest TEXT NOT NULL DEFAULT '',
  -- 失败与重试。
  error TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 控制记录：暂停/取消/恢复/修订等带版本的控制意图（不可变，只追加）。
CREATE TABLE IF NOT EXISTS control_records (
  record_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  -- 请求键（幂等：重复请求返回原决定，不同内容冲突）。
  request_key TEXT NOT NULL UNIQUE,
  -- 决定者（可信主体；授权引用不能由请求方自证）。
  decided_by TEXT NOT NULL,
  -- 控制类型与目标版本。
  kind TEXT NOT NULL CHECK(kind IN ('pause','resume','cancel','revise','stop')),
  control_revision INTEGER NOT NULL,
  -- 目标合同修订（防止旧控制覆盖新合同）。
  target_contract_revision INTEGER NOT NULL,
  -- 决定内容与摘要。
  decision TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- 结果引用：独立结果身份、单元/Run/代际、摘要、用量和证据。
CREATE TABLE IF NOT EXISTS result_receipts (
  receipt_id TEXT PRIMARY KEY,
  -- 稳定结果身份（重复送达只应用一次）。
  result_key TEXT NOT NULL UNIQUE,
  unit_id TEXT REFERENCES work_units(unit_id) ON DELETE SET NULL,
  run_id TEXT REFERENCES runs(id) ON DELETE CASCADE,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  -- 执行代际（旧代际结果留证但不能完成当前单元）。
  execution_generation INTEGER NOT NULL DEFAULT 0,
  lease_generation INTEGER NOT NULL DEFAULT 0,
  -- 摘要、用量和证据引用。
  summary TEXT NOT NULL DEFAULT '',
  usage_tracked INTEGER NOT NULL DEFAULT 0,
  model_requests INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER,
  output_tokens INTEGER,
  evidence_ref TEXT NOT NULL DEFAULT '',
  -- 应用状态：已接收、已应用、已隔离（同身份不同结果）。
  state TEXT NOT NULL CHECK(state IN ('received','applied','quarantined')) DEFAULT 'received',
  received_at INTEGER NOT NULL,
  applied_at INTEGER NOT NULL DEFAULT 0
);

-- 基础审计：贯通 input/task/step/run/wait/command/result 标识的审计记录。
-- 普通日志不能替代审计（P1-14 扩展为完整可观测性）。
CREATE TABLE IF NOT EXISTS runtime_audit (
  audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
  -- 关联标识（可选，按事件类型填充）。
  task_id TEXT,
  step_id TEXT,
  run_id TEXT,
  unit_id TEXT,
  wait_id TEXT,
  command_id TEXT,
  result_key TEXT,
  -- 事件与版本。
  event TEXT NOT NULL,             -- 事件类型（state_change/claim/report/control/...）
  actor TEXT NOT NULL DEFAULT '',  -- 行为主体
  storage_revision INTEGER NOT NULL DEFAULT 0,
  control_revision INTEGER NOT NULL DEFAULT 0,
  -- 内容摘要（不含敏感正文；大内容走材料引用）。
  summary TEXT NOT NULL DEFAULT '',
  detail_ref TEXT NOT NULL DEFAULT '',
  occurred_at INTEGER NOT NULL
);

-- 索引：就绪单元扫描、Step 依赖查询、审计关联。
CREATE INDEX IF NOT EXISTS work_units_ready ON work_units(state, ready_at) WHERE state='ready';
CREATE INDEX IF NOT EXISTS steps_by_task ON steps(task_id, position);
CREATE INDEX IF NOT EXISTS step_deps_on ON step_dependencies(depends_on_step_id);
CREATE INDEX IF NOT EXISTS control_by_task ON control_records(task_id, created_at);
CREATE INDEX IF NOT EXISTS receipts_by_task ON result_receipts(task_id, state);
CREATE INDEX IF NOT EXISTS audit_by_task ON runtime_audit(task_id, occurred_at);
CREATE INDEX IF NOT EXISTS audit_by_run ON runtime_audit(run_id, occurred_at);
