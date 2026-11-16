-- P3-16：整段外部工作流委托（V3 §8.1，F02/F08/F10）。
--
-- 已有平台可独自推进其负责的流程，本地持续观察、控制和验收：
--   - 委托记录实际目标、已固定版本、起止边界和双方责任；一次委托关联外部 Job；
--   - 外部负责的步骤，本地不再并行调度同一动作；结果通过 Inbox 更新 Operation，
--     领域验收仍由本地规则决定；
--   - 暂停/取消/补充是否支持以提供者契约为准；外部不支持时停止本地后续工作
--     并保留在途责任；
--   - 需要交回部分步骤时，先确认外部停止边界和未决效果，再生成新本地执行
--     单元，不能无缝假定已停；
--   - 重启恢复按原 Job 对账，不因外部状态查询失败重新启动整个流程。

CREATE TABLE IF NOT EXISTS delegations (
  delegation_id TEXT PRIMARY KEY,
  -- 委托目标与已固定版本。
  task_id TEXT NOT NULL,
  target_refs TEXT NOT NULL,        -- JSON：实际目标
  pinned_versions TEXT NOT NULL DEFAULT '{}',  -- JSON：已固定版本
  -- 起止边界与双方责任。
  boundary_start INTEGER NOT NULL,
  boundary_end INTEGER NOT NULL DEFAULT 0,
  local_responsibility TEXT NOT NULL DEFAULT '',
  remote_responsibility TEXT NOT NULL DEFAULT '',
  -- 关联外部 Job。
  external_job_id TEXT NOT NULL DEFAULT '',
  -- 状态（active/completed/cancelled/handover）。
  state TEXT NOT NULL CHECK(state IN ('active','completed','cancelled','handover')) DEFAULT 'active',
  -- 外部支持的暂停/取消/补充（以提供者契约为准）。
  supports_pause TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_pause IN ('supported','unsupported','unknown')),
  supports_cancel TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_cancel IN ('supported','unsupported','unknown')),
  supports_supplement TEXT NOT NULL DEFAULT 'unknown' CHECK(supports_supplement IN ('supported','unsupported','unknown')),
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS delegations_by_task ON delegations(task_id, state);
CREATE INDEX IF NOT EXISTS delegations_by_job ON delegations(external_job_id, state);

-- 移交记录（需要交回部分步骤时，确认外部停止边界和未决效果）。
CREATE TABLE IF NOT EXISTS delegation_handovers (
  handover_id TEXT PRIMARY KEY,
  delegation_id TEXT NOT NULL REFERENCES delegations(delegation_id) ON DELETE CASCADE,
  -- 外部停止边界与未决效果。
  external_stop_boundary TEXT NOT NULL DEFAULT '',
  pending_effects TEXT NOT NULL DEFAULT '[]',
  -- 新本地执行单元（确认停止边界后生成）。
  new_local_units TEXT NOT NULL DEFAULT '[]',
  -- 状态（pending/confirmed/completed）。
  state TEXT NOT NULL CHECK(state IN ('pending','confirmed','completed')) DEFAULT 'pending',
  created_at INTEGER NOT NULL,
  confirmed_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS handovers_by_delegation ON delegation_handovers(delegation_id, state);
