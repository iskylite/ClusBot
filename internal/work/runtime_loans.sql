-- P4-19：借出冻结、在途核对与交接事务（V3 §5.5、§6.7、F06、E04）。
--
-- 管理责任转移时旧工作不能继续新写，未知在途效果不被隐藏：
--   - 先登记交接意图与冻结并递增 ownership_epoch；交接意图不是新增资产末级
--     状态；
--   - 列出冻结前已派发的冲突写并逐项核对完成/停止和效果；已登记未派发的
--     旧操作拒绝继续；
--   - 正常交接前处理占用、责任、双方、期限、只读范围与例外条件；未决效果
--     不明时保持原状态与交接待办；
--   - 实际交接确认时再次递增 epoch，同事务建立唯一有效借用并改为借出；保持
--     状态与有效 loan 双向一致；
--   - 人工例外必须完整记录授权、接收人、未知效果与后续责任；无外部 fencing
--     不保证旧在途请求不再生效。

CREATE TABLE IF NOT EXISTS loan_handovers (
  handover_id TEXT PRIMARY KEY,
  -- 对象与交接意图（交接意图不是新增资产末级状态）。
  object_ref TEXT NOT NULL,
  intent TEXT NOT NULL CHECK(intent IN ('lend','return')),
  -- 冻结（先登记交接意图与冻结并递增 ownership_epoch）。
  frozen_at INTEGER NOT NULL DEFAULT 0,
  ownership_epoch_before INTEGER NOT NULL,
  -- 冻结前已派发的冲突写（逐项核对完成/停止和效果）。
  conflict_writes TEXT NOT NULL DEFAULT '[]',
  -- 占用、责任、双方、期限、只读范围与例外条件。
  occupancy TEXT NOT NULL DEFAULT '',
  responsibility TEXT NOT NULL DEFAULT '',
  lender TEXT NOT NULL DEFAULT '',
  borrower TEXT NOT NULL DEFAULT '',
  due_at INTEGER NOT NULL DEFAULT 0,
  read_only_scope TEXT NOT NULL DEFAULT '',
  exception_conditions TEXT NOT NULL DEFAULT '',
  -- 状态（intent/frozen/reconciling/confirmed/cancelled）。
  state TEXT NOT NULL CHECK(state IN ('intent','frozen','reconciling','confirmed','cancelled')) DEFAULT 'intent',
  -- 创建与更新。
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS handovers_by_object ON loan_handovers(object_ref, state);

-- 借用记录（唯一有效借用）。
CREATE TABLE IF NOT EXISTS loans (
  loan_id TEXT PRIMARY KEY,
  -- 对象与交接（实际交接确认时再次递增 epoch，同事务建立唯一有效借用）。
  object_ref TEXT NOT NULL,
  handover_id TEXT NOT NULL,
  -- 双方、期限、只读范围。
  lender TEXT NOT NULL,
  borrower TEXT NOT NULL,
  due_at INTEGER NOT NULL DEFAULT 0,
  read_only_scope TEXT NOT NULL DEFAULT '',
  -- 状态（active/returned/expired；唯一有效借用）。
  state TEXT NOT NULL CHECK(state IN ('active','returned','expired')) DEFAULT 'active',
  -- 借出时的 epoch（保持状态与有效 loan 双向一致）。
  ownership_epoch_at_lend INTEGER NOT NULL,
  -- 创建与归还。
  lent_at INTEGER NOT NULL,
  returned_at INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS loans_one_active_per_object ON loans(object_ref) WHERE state='active';
CREATE INDEX IF NOT EXISTS loans_by_object ON loans(object_ref, state);

-- 人工例外（完整记录授权、接收人、未知效果与后续责任）。
CREATE TABLE IF NOT EXISTS loan_exceptions (
  exception_id TEXT PRIMARY KEY,
  -- 关联交接与对象。
  handover_id TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  -- 授权、接收人、未知效果与后续责任。
  authorized_by TEXT NOT NULL,
  received_by TEXT NOT NULL,
  unknown_effects TEXT NOT NULL DEFAULT '[]',
  follow_up_responsibility TEXT NOT NULL DEFAULT '',
  -- 创建。
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS exceptions_by_handover ON loan_exceptions(handover_id);
