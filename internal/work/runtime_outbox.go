package work

// Outbox 与受控命令接收端。
//
// 本文件实现 §5.3 的持久待发送命令：保证状态变化后的工作请求不会因
// 崩溃丢失，同时不把命令重投变成副作用重做。
//   - 命令记录 command_id、目标/版本、payload 摘要、后端、状态、尝试数和
//     下次投递时间；业务事务同步创建；
//   - 提交后投递；接收端以 command_id/内容去重，返回原受理结果；确认丢失
//     按同键有界退避重投；
//   - 接收端必须再检查当前工作单元/Operation 状态；已派发或待核对走核对，
//     不能重启动作；
//   - 去重记录覆盖重投期和操作生命周期；未知最大重投期保留摘要/墓碑；
//   - 失败、最老年龄、下次重试和不可恢复冲突暴露给运行时查询；不在数据库
//     事务内等待投递。
//
// 与受控接收端的关系：本项使用 的可控接收端做投递验证，不直接接
// 设备写接口；F02 的命令重投子例不产生第二个逻辑动作，完整外部生效验证
// 在 P3 接入。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Outbox 命令
// ---------------------------------------------------------------------------

// OutboxCommand 是 outbox_commands 的一行。
type OutboxCommand struct {
	CommandID      string
	Kind           string
	TargetKind     string
	TargetID       string
	TargetVersion  int
	TaskID         string
	UnitID         string
	OperationID    string
	PayloadDigest  string
	PayloadRef     string
	Backend        string
	State          string // pending/dispatched/acknowledged/failed/dead
	Attempts       int
	NextAttemptAt  int64
	CreatedAt      int64
	DispatchedAt   int64
	AcknowledgedAt int64
	Failure        string
}

// ---------------------------------------------------------------------------
// 登记（业务事务同步创建）
// ---------------------------------------------------------------------------

// enqueueOutbox 在业务事务内同步创建命令。
// 命令与状态变化同事务提交：提交前崩溃没有可投递命令；提交后确认前崩溃
// 可恢复同一命令。
func enqueueOutbox(tx *sql.Tx, c OutboxCommand) error {
	if c.CommandID == "" {
		return errors.New("outbox 命令缺少 command_id")
	}
	if c.Kind == "" {
		return errors.New("outbox 命令缺少类型")
	}
	if c.PayloadDigest == "" {
		return errors.New("outbox 命令缺少载荷摘要")
	}
	c.State = "pending"
	c.Attempts = 0
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := tx.Exec(`INSERT INTO outbox_commands(command_id,kind,target_kind,target_id,target_version,
task_id,unit_id,operation_id,payload_digest,payload_ref,backend,state,attempts,next_attempt_at,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.CommandID, c.Kind, c.TargetKind, c.TargetID, c.TargetVersion,
		nullable(c.TaskID), nullable(c.UnitID), nullable(c.OperationID),
		c.PayloadDigest, c.PayloadRef, c.Backend, c.State, c.Attempts, c.NextAttemptAt, c.CreatedAt)
	return err
}

// ---------------------------------------------------------------------------
// 投递（提交后执行，不在事务内等待）
// ---------------------------------------------------------------------------

// CommandReceiver 是受控命令接收端：以 command_id/内容去重，返回原受理结果。
type CommandReceiver interface {
	// Receive 接收一个命令；重复 command_id 返回原受理结果，不同载荷冲突报警。
	Receive(ctx context.Context, c OutboxCommand) (receipt string, err error)
}

// dispatchDue 投递到期命令（提交后执行；不在数据库事务内等待投递）。
// 返回成功/失败/跳过的数量。接收成功后丢确认造成重投，只受理一次。
func dispatchDue(ctx context.Context, db *sql.DB, receiver CommandReceiver, backend string, now int64, limit int) (sent, failed, skipped int, err error) {
	// 读取到期命令（不在事务内等待投递）。
	rows, err := db.QueryContext(ctx, `SELECT command_id,kind,target_kind,target_id,target_version,task_id,unit_id,operation_id,
payload_digest,payload_ref,backend,state,attempts,next_attempt_at,created_at,dispatched_at,acknowledged_at,failure
FROM outbox_commands WHERE backend=? AND state IN ('pending','failed') AND next_attempt_at<=?
ORDER BY next_attempt_at LIMIT ?`, backend, now, limit)
	if err != nil {
		return 0, 0, 0, err
	}
	var commands []OutboxCommand
	for rows.Next() {
		var c OutboxCommand
		var taskID, unitID, operationID sql.NullString
		if err := rows.Scan(&c.CommandID, &c.Kind, &c.TargetKind, &c.TargetID, &c.TargetVersion,
			&taskID, &unitID, &operationID, &c.PayloadDigest, &c.PayloadRef, &c.Backend,
			&c.State, &c.Attempts, &c.NextAttemptAt, &c.CreatedAt, &c.DispatchedAt, &c.AcknowledgedAt, &c.Failure); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		c.TaskID, c.UnitID, c.OperationID = taskID.String, unitID.String, operationID.String
		commands = append(commands, c)
	}
	rows.Close()

	for _, c := range commands {
		// 标记已投递（条件更新，并发投递只一个成功）。
		res, err := db.ExecContext(ctx, `UPDATE outbox_commands SET state='dispatched', attempts=attempts+1, dispatched_at=?
WHERE command_id=? AND state IN ('pending','failed')`, now, c.CommandID)
		if err != nil {
			failed++
			continue
		}
		if n, _ := res.RowsAffected(); n != 1 {
			skipped++ // 并发竞争：另一投递者已取得
			continue
		}
		// 调用接收端（提交后外部 I/O，不占写事务）。
		receipt, recvErr := receiver.Receive(ctx, c)
		if recvErr != nil {
			// 确认丢失/失败：按同键有界退避重投。
			nextRetry := now + outboxRetryBackoff(c.Attempts)
			db.ExecContext(ctx, `UPDATE outbox_commands SET state='failed', next_attempt_at=?, failure=? WHERE command_id=?`,
				nextRetry, recvErr.Error(), c.CommandID)
			failed++
			continue
		}
		// 接收成功：标记确认（接收成功后丢确认造成重投，只受理一次）。
		if _, err := db.ExecContext(ctx, `UPDATE outbox_commands SET state='acknowledged', acknowledged_at=? WHERE command_id=?`,
			now, c.CommandID); err != nil {
			failed++
			continue
		}
		_ = receipt
		sent++
	}
	return sent, failed, skipped, nil
}

// outboxRetryBackoff 计算重投退避（有界，含抖动上限）。
func outboxRetryBackoff(attempts int) int64 {
	ms := int64(1000) << uint(attempts)
	if ms > 300000 {
		ms = 300000
	}
	return ms
}

// ---------------------------------------------------------------------------
// 受控接收端（去重 + 状态守卫）
// ---------------------------------------------------------------------------

// DedupReceiver 是一个带去重记录和状态守卫的接收端。
// 重复 command_id 返回原受理结果；不同载荷冲突报警；接收前再检查当前
// 工作单元/Operation 状态，已派发或待核对走核对，不能重启动作。
type DedupReceiver struct {
	DB *sql.DB
	// OnReceive 是实际处理回调（由测试/适配器注入）。
	OnReceive func(ctx context.Context, c OutboxCommand) (string, error)
	// GuardDispatch 是状态守卫：返回 true 表示"已派发/待核对，走核对路径，
	// 不重启动作"（F02 命令重投不产生第二个逻辑动作）。
	GuardDispatch func(ctx context.Context, c OutboxCommand) (alreadyHandled bool, err error)
}

// Receive 实现 CommandReceiver：去重 + 状态守卫。
func (r *DedupReceiver) Receive(ctx context.Context, c OutboxCommand) (string, error) {
	// 1. 去重：同 command_id 同载荷返回原受理结果；同 ID 不同载荷冲突报警。
	if r.DB != nil {
		var existingDigest, existingReceipt string
		err := r.DB.QueryRowContext(ctx, `SELECT payload_digest, receipt FROM outbox_receipts WHERE command_id=? AND receiver='default'`,
			c.CommandID).Scan(&existingDigest, &existingReceipt)
		if err == nil {
			if existingDigest != c.PayloadDigest {
				return "", fmt.Errorf("载荷冲突：命令 %s 已有不同载荷", c.CommandID)
			}
			// 重复投递：返回原受理结果（只受理一次）。
			return existingReceipt, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	// 2. 状态守卫：接收前再检查当前工作单元/Operation 状态。
	if r.GuardDispatch != nil {
		handled, err := r.GuardDispatch(ctx, c)
		if err != nil {
			return "", err
		}
		if handled {
			// 已派发或待核对：走核对路径，不重启动作（F02 重投守卫）。
			return "reconcile:" + c.CommandID, nil
		}
	}
	// 3. 实际接收。
	if r.OnReceive == nil {
		return "", errors.New("接收端未配置处理回调")
	}
	receipt, err := r.OnReceive(ctx, c)
	if err != nil {
		return "", err
	}
	// 4. 记录去重（覆盖重投期和操作生命周期；未知最大重投期保留墓碑）。
	if r.DB != nil {
		r.DB.ExecContext(ctx, `INSERT INTO outbox_receipts(command_id,receiver,payload_digest,receipt,received_at,tombstone)
VALUES(?, 'default', ?, ?, ?, ?) ON CONFLICT(command_id,receiver) DO NOTHING`,
			c.CommandID, c.PayloadDigest, receipt, time.Now().UnixMilli(), 1)
	}
	return receipt, nil
}

// ---------------------------------------------------------------------------
// 运行时查询
// ---------------------------------------------------------------------------

// OutboxBacklog 是 Outbox 的运行时查询快照。
type OutboxBacklog struct {
	Pending       int   // 待投递数
	Failed        int   // 失败待重试数
	OldestPendingMS int64 // 最老待投递年龄（毫秒）
	NextRetryAt   int64 // 下次重试时间
	Dead          int   // 不可恢复冲突数
}

// readOutboxBacklog 暴露失败、最老年龄、下次重试和不可恢复冲突。
func readOutboxBacklog(q recordsQuerier, now int64) (OutboxBacklog, error) {
	var b OutboxBacklog
	if err := q.QueryRow(`SELECT COUNT(*) FROM outbox_commands WHERE state='pending'`).Scan(&b.Pending); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM outbox_commands WHERE state='failed'`).Scan(&b.Failed); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM outbox_commands WHERE state='dead'`).Scan(&b.Dead); err != nil {
		return b, err
	}
	var oldest sql.NullInt64
	if err := q.QueryRow(`SELECT min(created_at) FROM outbox_commands WHERE state IN ('pending','failed')`).Scan(&oldest); err != nil {
		return b, err
	}
	if oldest.Valid {
		b.OldestPendingMS = now - oldest.Int64
	}
	var nextRetry sql.NullInt64
	if err := q.QueryRow(`SELECT min(next_attempt_at) FROM outbox_commands WHERE state='failed' AND next_attempt_at>0`).Scan(&nextRetry); err != nil {
		return b, err
	}
	if nextRetry.Valid {
		b.NextRetryAt = nextRetry.Int64
	}
	return b, nil
}

// markOutboxDead 把不可恢复冲突的命令标记为 dead（不无限重试）。
func markOutboxDead(tx *sql.Tx, commandID, reason string, now int64) error {
	_, err := tx.Exec(`UPDATE outbox_commands SET state='dead', failure=? WHERE command_id=?`, reason, commandID)
	return err
}

// 确保 context/time 引用保留。
var (
	_ = context.Background
	_ = time.Now
)
