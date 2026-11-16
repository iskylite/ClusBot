-- P3-06：材料引用、来源权限与证据落盘（V3 §6.5、§7.2—7.3、§8.3，F07/F10/F11）。
--
-- 日志/报告可独立保存并安全引用，为 P4 业务和 P5 上下文提供基础：
--   - 材料 ID、来源/版本、对象/时间、媒体类型、字节数、摘要、定位符、留存
--     与权限依据；正文不塞进事件或对象关系；
--   - 本地快照先写临时文件、校验并同步内容，原子就位后按目标平台验证目录
--     持久性，再提交引用；DB 失败留下可识别孤立文件，不留下指向半文件的
--     已确认引用；仅 rename 成功不能作为断电耐久证据；
--   - 按授权读取有限片段，输出实际覆盖和截断；外部来源不可用保留引用及
--     缺口，不伪造内容；
--   - 派生摘要必须记录必要来源；尚无完整依赖核验能力时不得将受限派生物
--     开放为跨范围经验；
--   - 保留旧材料/上传/工具回执查询，按实际能力渐进迁移。

CREATE TABLE IF NOT EXISTS evidence_references (
  evidence_id TEXT PRIMARY KEY,
  -- 材料 ID 与来源/版本。
  material_id TEXT NOT NULL,
  source_ref TEXT NOT NULL,        -- 来源（connection/tool/run）
  source_version INTEGER NOT NULL DEFAULT 1,
  -- 对象/时间。
  object_ref TEXT NOT NULL DEFAULT '',
  time_range_start INTEGER NOT NULL DEFAULT 0,
  time_range_end INTEGER NOT NULL DEFAULT 0,
  -- 媒体类型、字节数、摘要、定位符。
  media_type TEXT NOT NULL DEFAULT '',
  bytes INTEGER NOT NULL DEFAULT 0,
  content_digest TEXT NOT NULL,
  locator TEXT NOT NULL DEFAULT '',  -- 定位符（文件路径/URL）
  -- 留存与权限依据。
  retention_policy TEXT NOT NULL DEFAULT '',
  authorization_ref TEXT NOT NULL DEFAULT '',  -- 权限依据
  -- 派生摘要的必要来源（尚无完整依赖核验能力时不得开放为跨范围经验）。
  derived_from TEXT NOT NULL DEFAULT '[]',  -- JSON：来源引用列表
  derived_restricted INTEGER NOT NULL DEFAULT 1 CHECK(derived_restricted IN (0,1)),
  -- 状态：pending（未就位）/committed（已就位）/orphaned（DB 失败孤立）。
  state TEXT NOT NULL CHECK(state IN ('pending','committed','orphaned')) DEFAULT 'pending',
  created_at INTEGER NOT NULL,
  committed_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS evidence_by_object ON evidence_references(object_ref, state);
CREATE INDEX IF NOT EXISTS evidence_by_source ON evidence_references(source_ref, source_version);

-- 证据读取记录（有限片段与截断）。
CREATE TABLE IF NOT EXISTS evidence_reads (
  read_id TEXT PRIMARY KEY,
  evidence_id TEXT NOT NULL REFERENCES evidence_references(evidence_id) ON DELETE CASCADE,
  -- 读取范围与截断。
  requested_start INTEGER NOT NULL DEFAULT 0,
  requested_end INTEGER NOT NULL DEFAULT 0,
  actual_start INTEGER NOT NULL DEFAULT 0,
  actual_end INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0 CHECK(truncated IN (0,1)),
  -- 读取者与时间。
  read_by TEXT NOT NULL,
  read_at INTEGER NOT NULL,
  -- 缺口（外部来源不可用保留引用及缺口）。
  gap_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS reads_by_evidence ON evidence_reads(evidence_id, read_at);
