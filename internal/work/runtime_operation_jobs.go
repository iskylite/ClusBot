package work

// 异步 Job、回执、查询与事件关联。
//
// 本文件实现外部长作业跨进程与会话持续跟踪，回执有可信来源与关联：
//   - Start 同步结果/异步句柄分别规范化；保存 provider_job_id、查询键、
//     状态保留契约和下一查询时间；
//   - 回调认证后先入 Inbox，绑定 operation_id、attempt、目标、外部版本与
//     代际；未知关联隔离；
//   - 推送与轮询进入同一去重/核对路径；无可靠顺序不按到达时间覆盖，必要时
//     Get 当前状态；
//   - 轮询用 Wait/Timer 和有界退避，不占 Worker；重启按持久 Job ID 继续；
//   - 纯外部 MCP Task 只作为远端句柄，不替代本地 Task；无推送/查询时明确
//     保留未知。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 异步 Job（provider_job_id、查询键、状态保留契约、下一查询时间）
// ---------------------------------------------------------------------------

// OperationJob 是 operation_jobs 的一行。
type OperationJob struct {
	JobID             string
	OperationID       string
	ProviderJobID     string
	QueryKey          string
	RetentionContract string
	NextQueryAt       int64
	State             string // running/succeeded/failed/expired
	QueryGeneration   int
	CreatedAt         int64
	UpdatedAt         int64
}

// ---------------------------------------------------------------------------
// 规范化（Start 同步结果/异步句柄分别规范化）
// ---------------------------------------------------------------------------

// normalizeJobStart 规范化 Job 启动。
// Start 同步结果/异步句柄分别规范化；保存 provider_job_id、查询键、状态保留
// 契约和下一查询时间；首个响应丢失但可按原键查找时，找回原 Job，不创建新 Job。
func normalizeJobStart(tx *sql.Tx, operationID, providerJobID, queryKey, retentionContract string, now int64) (OperationJob, error) {
	if operationID == "" || providerJobID == "" || queryKey == "" {
		return OperationJob{}, errors.New("Job 缺少操作/提供者 ID/查询键")
	}
	// 首个响应丢失但可按原键查找：找回原 Job（不创建新 Job）。
	var existing OperationJob
	err := tx.QueryRow(`SELECT job_id,operation_id,provider_job_id,query_key,retention_contract,
next_query_at,state,query_generation,created_at,updated_at FROM operation_jobs WHERE operation_id=? AND provider_job_id=?`,
		operationID, providerJobID).
		Scan(&existing.JobID, &existing.OperationID, &existing.ProviderJobID, &existing.QueryKey,
			&existing.RetentionContract, &existing.NextQueryAt, &existing.State, &existing.QueryGeneration,
			&existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		if existing.QueryKey != queryKey || existing.RetentionContract != retentionContract {
			return OperationJob{}, fmt.Errorf("%w：作业查询或保留契约不匹配", ErrConflict)
		}
		return existing, nil // 找回原 Job（不创建新 Job）
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperationJob{}, err
	}
	// 创建新 Job。
	job := OperationJob{
		JobID: id(), OperationID: operationID, ProviderJobID: providerJobID, QueryKey: queryKey,
		RetentionContract: retentionContract, NextQueryAt: now + 5000, State: "running",
		QueryGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	_, err = tx.Exec(`INSERT INTO operation_jobs(job_id,operation_id,provider_job_id,query_key,
retention_contract,next_query_at,state,query_generation,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		job.JobID, job.OperationID, job.ProviderJobID, job.QueryKey, job.RetentionContract,
		job.NextQueryAt, job.State, job.QueryGeneration, now, now)
	if err != nil {
		return OperationJob{}, err
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// 回执关联（回调认证后先入 Inbox，绑定 operation/attempt/目标/版本/代际）
// ---------------------------------------------------------------------------

// OperationReceipt 是 operation_receipts 的一行。
type OperationReceipt struct {
	ReceiptID       string
	OperationID     string
	AttemptID       string
	TargetRef       string
	ExternalVersion int
	QueryGeneration int
	PayloadDigest   string
	Receipt         string
	Source          string // callback/poll; adapters normalize the payload digest across sources
	ReceivedAt      int64
	State           string // received/applied/stale/quarantined
}

// receiveOperationReceipt 接收回执。
// 回调认证后先入 Inbox，绑定 operation_id、attempt、目标、外部版本与代际；
// 未知关联隔离；推送与轮询进入同一去重/核对路径；无可靠顺序不按到达时间
// 覆盖，必要时 Get 当前状态。
func receiveOperationReceipt(tx *sql.Tx, r OperationReceipt, now int64) (ReceiveResult, error) {
	if r.OperationID == "" || r.PayloadDigest == "" || r.AttemptID == "" || r.TargetRef == "" ||
		r.ExternalVersion < 1 || r.QueryGeneration < 1 || (r.Source != "callback" && r.Source != "poll") {
		return ReceiveResult{}, errors.New("回执缺少有效的操作、尝试、目标、版本、摘要或来源")
	}
	op, err := readOperation(tx, r.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return quarantineOperationReceipt(tx, Operation{OperationID: r.OperationID}, "未知 Operation 回执", now)
	}
	if err != nil {
		return ReceiveResult{}, err
	}
	var attemptMatches, targetMatches bool
	if err := tx.QueryRow(`SELECT
EXISTS(SELECT 1 FROM operation_attempts WHERE operation_id=? AND attempt_id=?),
EXISTS(SELECT 1 FROM operation_jobs WHERE operation_id=? AND provider_job_id=?)`,
		r.OperationID, r.AttemptID, r.OperationID, r.TargetRef).Scan(&attemptMatches, &targetMatches); err != nil {
		return ReceiveResult{}, err
	}
	if !attemptMatches {
		return quarantineOperationReceipt(tx, op, "回执的尝试不属于原操作", now)
	}
	var targets []struct{ Ref string }
	if !targetMatches {
		if err := json.Unmarshal([]byte(op.TargetRefs), &targets); err != nil {
			return quarantineOperationReceipt(tx, op, "无法读取原操作目标快照", now)
		}
		for _, target := range targets {
			if target.Ref == r.TargetRef {
				targetMatches = true
				break
			}
		}
		if !targetMatches {
			return quarantineOperationReceipt(tx, op, "回执目标不在原操作的作业或目标快照中", now)
		}
	}
	var previous OperationReceipt
	err = tx.QueryRow(`SELECT receipt_id,query_generation,payload_digest,state FROM operation_receipts
WHERE operation_id=? AND attempt_id=? AND target_ref=? AND external_version=? AND source=?`,
		r.OperationID, r.AttemptID, r.TargetRef, r.ExternalVersion, r.Source).
		Scan(&previous.ReceiptID, &previous.QueryGeneration, &previous.PayloadDigest, &previous.State)
	if err == nil {
		if previous.PayloadDigest != r.PayloadDigest {
			if _, err := tx.Exec(`UPDATE operation_receipts SET state='quarantined' WHERE receipt_id=? AND state='received'`, previous.ReceiptID); err != nil {
				return ReceiveResult{}, err
			}
			return quarantineOperationReceipt(tx, op, "相同来源与版本的回执内容冲突", now)
		}
		if previous.State != "stale" || r.QueryGeneration <= previous.QueryGeneration {
			return ReceiveResult{Duplicate: true, Event: InboxEvent{EventID: previous.ReceiptID}}, nil
		}
		// Same verified content read again after an obsolete poll may be
		// considered in the new generation; the old application stays inert.
		if _, err := tx.Exec(`UPDATE operation_receipts SET query_generation=?, state='received' WHERE receipt_id=? AND state='stale'`,
			r.QueryGeneration, previous.ReceiptID); err != nil {
			return ReceiveResult{}, err
		}
		return ReceiveResult{Event: InboxEvent{EventID: previous.ReceiptID}}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReceiveResult{}, err
	}
	// 先入 Inbox（回调认证后）。
	r.ReceiptID = id()
	r.State = "received"
	r.ReceivedAt = now
	_, err = tx.Exec(`INSERT INTO operation_receipts(receipt_id,operation_id,attempt_id,target_ref,
external_version,query_generation,payload_digest,receipt,source,received_at,state)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.ReceiptID, r.OperationID, r.AttemptID, r.TargetRef, r.ExternalVersion,
		r.QueryGeneration, r.PayloadDigest, r.Receipt, r.Source, now, r.State)
	if err != nil {
		return ReceiveResult{}, err
	}
	return ReceiveResult{Event: InboxEvent{EventID: r.ReceiptID}}, nil
}

func quarantineOperationReceipt(tx *sql.Tx, op Operation, reason string, stamp int64) (ReceiveResult, error) {
	if err := recordAudit(tx, AuditEvent{TaskID: op.TaskID, DetailRef: op.OperationID,
		Event: "unknown_receipt_quarantined", Actor: "system", Summary: reason, OccurredAt: stamp}); err != nil {
		return ReceiveResult{}, err
	}
	return ReceiveResult{Quarantined: true}, nil
}

// ---------------------------------------------------------------------------
// 去重/核对路径（重复/旧代际回执留证不重复推进）
// ---------------------------------------------------------------------------

// ApplyReceiptResult 是应用回执结果。
type ApplyReceiptResult struct {
	Applied   bool
	Duplicate bool
	Reason    string
}

// applyOperationReceipt 应用回执。
// 推送与轮询进入同一去重/核对路径；重复/旧代际回执留证而不重复推进；
// 无可靠顺序不按到达时间覆盖，必要时 Get 当前状态。
func applyOperationReceipt(tx *sql.Tx, receiptID string, currentGeneration int, now int64) (ApplyReceiptResult, error) {
	var result ApplyReceiptResult
	// 读取回执。
	var r OperationReceipt
	err := tx.QueryRow(`SELECT receipt_id,operation_id,attempt_id,target_ref,external_version,
query_generation,payload_digest,state FROM operation_receipts WHERE receipt_id=?`, receiptID).
		Scan(&r.ReceiptID, &r.OperationID, &r.AttemptID, &r.TargetRef, &r.ExternalVersion,
			&r.QueryGeneration, &r.PayloadDigest, &r.State)
	if err != nil {
		return result, err
	}
	// 重复回执：不重复推进。
	if r.State == "applied" {
		result.Duplicate = true
		result.Reason = "回执已应用（重复不重复推进）"
		return result, nil
	}
	if r.State != "received" {
		result.Reason = "回执已隔离或只留证"
		return result, nil
	}
	var attemptID string
	if err := tx.QueryRow(`SELECT coalesce(current_attempt_id,'') FROM operations WHERE operation_id=?`, r.OperationID).Scan(&attemptID); err != nil {
		return result, err
	}
	if r.QueryGeneration > currentGeneration {
		op, err := readOperation(tx, r.OperationID)
		if err != nil {
			return result, err
		}
		if _, err := tx.Exec(`UPDATE operation_receipts SET state='quarantined' WHERE receipt_id=?`, receiptID); err != nil {
			return result, err
		}
		reason := "回执代际超前，隔离不推进"
		_, err = quarantineOperationReceipt(tx, op, reason, now)
		result.Reason = reason
		return result, err
	}
	if r.QueryGeneration < currentGeneration || r.AttemptID != attemptID {
		if _, err := tx.Exec(`UPDATE operation_receipts SET state='stale' WHERE receipt_id=?`, receiptID); err != nil {
			return result, err
		}
		result.Reason = "旧代际或旧尝试回执，留证不推进"
		return result, nil
	}
	// Source is provenance, not a separate stream of business truth. Compare
	// semantic digests only after the adapter has canonicalized the result.
	var newerVersion int
	var earlierDigest string
	err = tx.QueryRow(`SELECT external_version,payload_digest FROM operation_receipts
WHERE operation_id=? AND attempt_id=? AND target_ref=? AND state='applied' AND external_version>=?
ORDER BY external_version DESC LIMIT 1`, r.OperationID, r.AttemptID, r.TargetRef, r.ExternalVersion).
		Scan(&newerVersion, &earlierDigest)
	if err == nil {
		state := "stale"
		result.Reason = "相同或更新版本已应用，留证不重复推进"
		result.Duplicate = newerVersion == r.ExternalVersion && earlierDigest == r.PayloadDigest
		if newerVersion == r.ExternalVersion && earlierDigest != r.PayloadDigest {
			state = "quarantined"
			result.Reason = "相同版本的回执内容不一致，隔离不推进"
			op, err := readOperation(tx, r.OperationID)
			if err != nil {
				return result, err
			}
			if _, err := quarantineOperationReceipt(tx, op, result.Reason, now); err != nil {
				return result, err
			}
		}
		_, err := tx.Exec(`UPDATE operation_receipts SET state=? WHERE receipt_id=? AND state='received'`, state, receiptID)
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	// 应用回执（条件更新，只一次）。
	res, err := tx.Exec(`UPDATE operation_receipts SET state='applied' WHERE receipt_id=? AND state='received'`,
		receiptID)
	if err != nil {
		return result, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		result.Duplicate = true
		result.Reason = "并发应用竞争，另一处理者已取得"
		return result, nil
	}
	result.Applied = true
	return result, nil
}

// ---------------------------------------------------------------------------
// 轮询（Wait/Timer 和有界退避，不占 Worker；重启按持久 Job ID 继续）
// ---------------------------------------------------------------------------

// pollDueJobs 轮询到期 Job。
// 轮询用 Wait/Timer 和有界退避，不占 Worker；重启按持久 Job ID 继续；
// 查询失败或作业过期不被当作未执行证明。
func pollDueJobs(tx *sql.Tx, now int64, limit int) (polled int, err error) {
	// 读取到期 running Job（不占 Worker）。
	rows, err := tx.Query(`SELECT job_id,operation_id,provider_job_id,query_generation FROM operation_jobs
WHERE state='running' AND next_query_at<=? ORDER BY next_query_at LIMIT ?`, now, limit)
	if err != nil {
		return 0, err
	}
	var jobs []OperationJob
	for rows.Next() {
		var j OperationJob
		if err := rows.Scan(&j.JobID, &j.OperationID, &j.ProviderJobID, &j.QueryGeneration); err != nil {
			rows.Close()
			return polled, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, job := range jobs {
		// 有界退避：下次查询时间递增。
		nextQuery := now + jobBackoff(job.QueryGeneration)
		if _, err := tx.Exec(`UPDATE operation_jobs SET next_query_at=?, query_generation=query_generation+1, updated_at=?
WHERE job_id=? AND state='running'`, nextQuery, now, job.JobID); err != nil {
			return polled, err
		}
		polled++
	}
	return polled, nil
}

// jobBackoff 计算 Job 查询退避（有界）。
func jobBackoff(generation int) int64 {
	ms := int64(5000) << uint(min(max(generation, 0), 6))
	if ms > 300000 {
		ms = 300000
	}
	return ms
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)

// migrateOperationReceiptKeys preserves historical receipt IDs while giving each
// operation attempt and target its own provider version sequence.
func migrateOperationReceiptKeys(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE operation_receipts RENAME TO operation_receipts_old;
DROP INDEX IF EXISTS receipts_by_operation`); err != nil {
		return err
	}
	if _, err := tx.Exec(runtimeOperationJobsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO operation_receipts(
receipt_id,operation_id,attempt_id,target_ref,external_version,query_generation,payload_digest,receipt,source,received_at,state)
SELECT receipt_id,operation_id,attempt_id,target_ref,external_version,query_generation,payload_digest,receipt,source,received_at,state
FROM operation_receipts_old`); err != nil {
		return err
	}
	_, err := tx.Exec(`DROP TABLE operation_receipts_old`)
	return err
}
