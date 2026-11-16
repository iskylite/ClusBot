-- P5-03：逐请求 ContextManifest 与恢复重装（V3 §7.6、F07/F10）。
--
-- 每次真正发送的模型请求均能追溯依据、裁剪和缺口：
--   - 装入当前工作合同/控制、事实、必要规则、能力快照、有限证据、适用经验
--     及未知项；硬约束装不下就停止/缩小范围；
--   - 在最终序列化/预算裁剪后记录来源修订、片段区间、实际使用/过滤/省略、
--     token/字节预算、请求摘要、提供者绑定和发送状态；
--   - 发送前重核当前权限和源版本；尚不能证明派生来源有效的候选不装配，不
--     等待 P5-04 才拦截；
--   - 恢复先读持久工作和未决操作，再查当前事实/授权/规则/能力，生成新
--     Manifest；旧清单只解释历史，不作为新授权；
--   - 保留旧 Manifest 格式及版本解释，不能把历史字段重新解释或补写成当时
--     未实际发送的内容。

CREATE TABLE IF NOT EXISTS v3_context_manifests (
  manifest_id TEXT PRIMARY KEY,
  -- 关联工作（当前工作合同/控制）。
  task_id TEXT NOT NULL,
  step_id TEXT,
  contract_revision INTEGER NOT NULL DEFAULT 1,
  -- 内容（事实、必要规则、能力快照、有限证据、适用经验及未知项）。
  facts_refs TEXT NOT NULL DEFAULT '[]',
  rules_refs TEXT NOT NULL DEFAULT '[]',
  capability_snapshot TEXT NOT NULL DEFAULT '{}',
  evidence_refs TEXT NOT NULL DEFAULT '[]',
  experience_refs TEXT NOT NULL DEFAULT '[]',
  unknowns TEXT NOT NULL DEFAULT '[]',
  -- 裁剪（实际使用/过滤/省略）。
  used_refs TEXT NOT NULL DEFAULT '[]',
  filtered_refs TEXT NOT NULL DEFAULT '[]',
  omitted_refs TEXT NOT NULL DEFAULT '[]',
  omission_reasons TEXT NOT NULL DEFAULT '{}',
  -- 预算（token/字节预算、请求摘要、提供者绑定）。
  token_budget INTEGER NOT NULL DEFAULT 0,
  byte_budget INTEGER NOT NULL DEFAULT 0,
  tokens_estimated INTEGER NOT NULL DEFAULT 0,  -- 模型未报 token 明确未知（-1 或 0 表示未知）
  request_digest TEXT NOT NULL,
  provider_ref TEXT NOT NULL DEFAULT '',
  -- 发送状态（prepared/sent/failed/rejected）。
  state TEXT NOT NULL CHECK(state IN ('prepared','sent','failed','rejected')) DEFAULT 'prepared',
  -- 来源修订与片段区间。
  source_revisions TEXT NOT NULL DEFAULT '{}',
  excerpt_ranges TEXT NOT NULL DEFAULT '{}',
  -- 版本与创建。
  manifest_version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  sent_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS manifests_by_task ON v3_context_manifests(task_id, state, created_at);
CREATE INDEX IF NOT EXISTS manifests_by_step ON v3_context_manifests(step_id, state);
