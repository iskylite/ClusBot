-- P3-07：Operation 七态、尝试与逐目标结果（V3 §5.2/5.4、§8.3，F01/F03/F08）。
--
-- 每个外部副作用有稳定身份、固定意图和可核对历史：
--   - Operation 绑定 Task/Step、能力/连接、目标快照、参数摘要、授权依据和
--     幂等键；登记与 Outbox 同事务；
--   - 实现七态守卫；outcome unknown 映射待核对，不能新增随意状态或直接失败重试；
--   - 同一逻辑操作跨 attempt 保持幂等键和参数；参数/目标变更创建新关联 Operation；
--   - 批量保存逐目标状态与证据；按成功余集补做是新操作，不改写原参数；
--   - 迟到矛盾回执保留并进入核对，不直接改写终态；失败可包含已发生效果。

CREATE TABLE IF NOT EXISTS operations (
  operation_id TEXT PRIMARY KEY,
  -- 绑定：Task/Step、能力/连接。
  task_id TEXT NOT NULL,
  step_id TEXT,
  capability_key TEXT NOT NULL,
  capability_version INTEGER NOT NULL DEFAULT 1,
  connection_id TEXT NOT NULL DEFAULT '',
  -- 目标快照、参数摘要、授权依据和幂等键。
  target_refs TEXT NOT NULL,        -- JSON：目标清单（含各对象 revision/epoch）
  params_digest TEXT NOT NULL,
  authorization_ref TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL UNIQUE,
  -- 状态（七态）。
  state TEXT NOT NULL CHECK(state IN ('registered','dispatched','running','reconcile','succeeded','failed','cancelled')) DEFAULT 'registered',
  -- 尝试数与当前 attempt。
  attempt_count INTEGER NOT NULL DEFAULT 0,
  current_attempt_id TEXT,
  -- 关联（参数/目标变更创建新关联 Operation）。
  predecessor_id TEXT,
  -- 时间。
  registered_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  completed_at INTEGER NOT NULL DEFAULT 0,
  -- 失败可包含已发生效果。
  effect_refs TEXT NOT NULL DEFAULT '[]',
  failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS operations_by_task ON operations(task_id, state);
CREATE INDEX IF NOT EXISTS operations_by_idempotency ON operations(idempotency_key);

-- 尝试记录（同一逻辑操作跨 attempt 保持幂等键和参数）。
CREATE TABLE IF NOT EXISTS operation_attempts (
  attempt_id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE CASCADE,
  -- 尝试序号与回执。
  attempt_number INTEGER NOT NULL,
  dispatched_at INTEGER NOT NULL,
  receipt TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL CHECK(outcome IN ('success','failure','unknown','pending')) DEFAULT 'pending',
  -- 证据引用。
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  completed_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(operation_id, attempt_number)
);

-- 逐目标结果（批量保存逐目标状态与证据）。
CREATE TABLE IF NOT EXISTS operation_target_results (
  operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE CASCADE,
  target_ref TEXT NOT NULL,
  -- 逐目标状态与证据。
  state TEXT NOT NULL CHECK(state IN ('pending','succeeded','failed','unknown')),
  evidence_ref TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (operation_id, target_ref)
);

CREATE INDEX IF NOT EXISTS target_results_by_state ON operation_target_results(operation_id, state);
