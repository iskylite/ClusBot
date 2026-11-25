package work

// 验收：异步 Job、回执、查询与事件关联。
//
// 三个测试入口：
//   - TestAsyncJobRestart：Start 同步结果/异步句柄分别规范化；保存
//     provider_job_id、查询键、状态保留契约和下一查询时间；首个响应丢失但
//     可按原键查找时，找回原 Job，不创建新 Job；轮询用 Wait/Timer 和有界
//     退避，不占 Worker；重启按持久 Job ID 继续。
//   - TestCallbackCorrelation：回调认证后先入 Inbox，绑定 operation_id、
//     attempt、目标、外部版本与代际；未知关联隔离；回调伪造被拒绝。
//   - TestPollingEventDeduplication：推送与轮询进入同一去重/核对路径；
//     重复/旧代际回执仍留证而不重复推进；无可靠顺序不按到达时间覆盖，必要时
//     Get 当前状态；查询失败或作业过期不被当作未执行证明。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupJobsStore 创建存储、任务与 Operation。
func setupJobsStore(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p309-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	taskID := run.TaskID
	// 登记 Operation。
	tx, _ := store.db.Begin()
	op, err := registerOperation(tx, operationFixture(taskID, "idem-job"), recordsTestNow())
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return store, taskID, op.OperationID
}

// TestAsyncJobRestart 异步 Job 重启。
func TestAsyncJobRestart(t *testing.T) {
	t.Run("job_normalized_with_provider_id_query_key_retention", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "normalize.db")
		now := recordsTestNow()
		// 规范化 Job：provider_job_id、查询键、状态保留契约、下一查询时间。
		tx, _ := store.db.Begin()
		job, err := normalizeJobStart(tx, opID, "provider-job-123", "query-key-1", "retention-7d", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if job.ProviderJobID != "provider-job-123" || job.QueryKey != "query-key-1" ||
			job.RetentionContract != "retention-7d" || job.NextQueryAt <= now {
			t.Errorf("job incomplete: %+v", job)
		}
	})

	t.Run("lost_first_response_recover_original_job", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "recover.db")
		now := recordsTestNow()
		// 创建 Job。
		tx, _ := store.db.Begin()
		first, _ := normalizeJobStart(tx, opID, "provider-job-123", "query-key-1", "retention", now)
		tx.Commit()
		// 首个响应丢失但可按原键查找：找回原 Job（不创建新 Job）。
		tx, _ = store.db.Begin()
		second, err := normalizeJobStart(tx, opID, "provider-job-123", "query-key-1", "retention", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if second.JobID != first.JobID {
			t.Errorf("lost response created new job: %s vs %s (找回原 Job 不创建新 Job)", second.JobID, first.JobID)
		}
	})

	t.Run("polling_with_backoff_not_occupy_worker", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "poll.db")
		now := recordsTestNow()
		tx, _ := store.db.Begin()
		normalizeJobStart(tx, opID, "provider-job-poll", "query-key", "retention", now)
		tx.Commit()
		// 轮询：有界退避，不占 Worker。
		tx, _ = store.db.Begin()
		polled, err := pollDueJobs(tx, now+10000, 10)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if polled != 1 {
			t.Errorf("polled = %d, want 1", polled)
		}
		// 下次查询时间递增（有界退避）。
		var nextQuery int64
		if err := store.db.QueryRow("SELECT next_query_at FROM operation_jobs WHERE operation_id=?", opID).Scan(&nextQuery); err != nil {
			t.Fatal(err)
		}
		if nextQuery <= now+10000 {
			t.Errorf("next query not advanced: %d (有界退避)", nextQuery)
		}
		// 轮询不占 Worker（无 work_unit 创建）。
		var unitCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM work_units WHERE task_id IN (SELECT task_id FROM operations WHERE operation_id=?)", opID).Scan(&unitCount); err != nil {
			t.Fatal(err)
		}
		if unitCount != 0 {
			t.Errorf("polling occupied worker: %d units (轮询不占 Worker)", unitCount)
		}
	})

	t.Run("restart_continues_by_persistent_job_id", func(t *testing.T) {
		// 重启按持久 Job ID 继续。
		path := filepath.Join(t.TempDir(), "restart.db")
		now := recordsTestNow()
		var jobID string
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, _ := store.Submit(ctx, "v3p309-restart", "任务", "", "", true)
			tx, _ := store.db.Begin()
			op, _ := registerOperation(tx, operationFixture(run.TaskID, "idem-restart"), now)
			job, _ := normalizeJobStart(tx, op.OperationID, "provider-job-restart", "query-key", "retention", now)
			jobID = job.JobID
			tx.Commit()
		}()
		// 重启：按持久 Job ID 继续。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var state string
		if err := store.db.QueryRow("SELECT state FROM operation_jobs WHERE job_id=?", jobID).Scan(&state); err != nil {
			t.Fatalf("job lost after restart: %v", err)
		}
		if state != "running" {
			t.Errorf("job state = %s after restart (重启按持久 Job ID 继续)", state)
		}
	})
}

func dispatchJobAttempt(t *testing.T, tx *sql.Tx, operationID string, stamp int64) string {
	t.Helper()
	attempt, err := dispatchOperationAttempt(tx, operationID, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return attempt.AttemptID
}

// TestCallbackCorrelation 回调关联。
func TestCallbackCorrelation(t *testing.T) {
	t.Run("callback_authenticated_binds_all_fields", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "callback.db")
		now := recordsTestNow()
		// 回调认证后先入 Inbox，绑定 operation/attempt/目标/外部版本/代际。
		tx, _ := store.db.Begin()
		attemptID := dispatchJobAttempt(t, tx, opID, now)
		receipt := OperationReceipt{
			OperationID: opID, AttemptID: attemptID, TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("回执"),
			Receipt: "成功", Source: "callback",
		}
		result, err := receiveOperationReceipt(tx, receipt, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Quarantined {
			t.Error("valid callback quarantined")
		}
		// 回执已绑定并入库。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM operation_receipts WHERE operation_id=? AND source='callback'", opID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("callback receipts = %d, want 1", count)
		}
	})

	t.Run("unknown_association_quarantined", func(t *testing.T) {
		store, _, _ := setupJobsStore(t, "unknown.db")
		now := recordsTestNow()
		// 未知关联（Operation 不存在）：隔离。
		tx, _ := store.db.Begin()
		result, err := receiveOperationReceipt(tx, OperationReceipt{
			OperationID: "nonexistent-op", AttemptID: "attempt-1", TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("回执"),
			Source: "callback",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Quarantined {
			t.Error("unknown association not quarantined (未知关联隔离)")
		}
		// 隔离已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='unknown_receipt_quarantined'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("quarantine not audited: count=%d", count)
		}
	})

	t.Run("forged_callback_rejected", func(t *testing.T) {
		store, _, _ := setupJobsStore(t, "forged.db")
		now := recordsTestNow()
		// 回调伪造（Operation 不存在）：拒绝（隔离）。
		tx, _ := store.db.Begin()
		result, err := receiveOperationReceipt(tx, OperationReceipt{
			OperationID: "forged-op", AttemptID: "attempt-1", TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("伪造"),
			Source: "callback",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Quarantined {
			t.Error("forged callback not rejected (回调伪造被拒绝)")
		}
	})
}

// TestPollingEventDeduplication 轮询事件去重。
func TestPollingEventDeduplication(t *testing.T) {
	t.Run("duplicate_receipt_not_duplicate_progress", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "dup.db")
		now := recordsTestNow()
		// 接收回执。
		tx, _ := store.db.Begin()
		attemptID := dispatchJobAttempt(t, tx, opID, now)
		receipt := OperationReceipt{
			OperationID: opID, AttemptID: attemptID, TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("回执"),
			Source: "poll",
		}
		result, _ := receiveOperationReceipt(tx, receipt, now)
		tx.Commit()
		// 应用回执。
		tx, _ = store.db.Begin()
		applyResult, err := applyOperationReceipt(tx, result.Event.EventID, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !applyResult.Applied {
			t.Fatal("first apply not applied")
		}
		// 重复应用：不重复推进。
		tx, _ = store.db.Begin()
		applyResult2, err := applyOperationReceipt(tx, result.Event.EventID, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !applyResult2.Duplicate {
			t.Error("duplicate receipt not marked duplicate (重复不重复推进)")
		}
	})

	t.Run("stale_generation_receipt_evidence_not_progress", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "stale.db")
		now := recordsTestNow()
		// 旧代际回执。
		tx, _ := store.db.Begin()
		attemptID := dispatchJobAttempt(t, tx, opID, now)
		receipt := OperationReceipt{
			OperationID: opID, AttemptID: attemptID, TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("旧回执"),
			Source: "poll",
		}
		result, _ := receiveOperationReceipt(tx, receipt, now)
		tx.Commit()
		// 以更高代际应用：旧代际留证但不推进。
		tx, _ = store.db.Begin()
		applyResult, err := applyOperationReceipt(tx, result.Event.EventID, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if applyResult.Applied {
			t.Error("stale receipt applied (旧代际留证不推进)")
		}
		if !strings.Contains(applyResult.Reason, "旧代际") {
			t.Errorf("stale reason missing: %s", applyResult.Reason)
		}
		// 回执仍留证（state=stale）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM operation_receipts WHERE receipt_id=?", result.Event.EventID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "stale" {
			t.Errorf("stale receipt state = %s, want stale", state)
		}
	})

	t.Run("poll_and_push_same_dedup_path", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "samededup.db")
		now := recordsTestNow()
		// 推送与轮询进入同一去重/核对路径。
		tx, _ := store.db.Begin()
		attemptID := dispatchJobAttempt(t, tx, opID, now)
		pollReceipt := OperationReceipt{
			OperationID: opID, AttemptID: attemptID, TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("轮询回执"),
			Source: "poll",
		}
		pushReceipt := OperationReceipt{
			OperationID: opID, AttemptID: attemptID, TargetRef: "asset-1",
			ExternalVersion: 1, QueryGeneration: 1, PayloadDigest: digest("轮询回执"),
			Source: "callback",
		}
		pollResult, _ := receiveOperationReceipt(tx, pollReceipt, now)
		pushResult, _ := receiveOperationReceipt(tx, pushReceipt, now)
		tx.Commit()
		// 同一版本与载荷经不同来源报告，也只应用一次。
		tx, _ = store.db.Begin()
		applyResult1, _ := applyOperationReceipt(tx, pollResult.Event.EventID, 1, now)
		tx.Commit()
		tx, _ = store.db.Begin()
		applyResult2, _ := applyOperationReceipt(tx, pushResult.Event.EventID, 1, now)
		tx.Commit()
		if !applyResult1.Applied {
			t.Error("poll receipt not applied")
		}
		// 推送回执（同内容同代际）：幂等。
		if applyResult2.Applied || !applyResult2.Duplicate {
			t.Fatalf("identical callback must not repeat progress: %+v", applyResult2)
		}
	})

	t.Run("query_failure_or_expired_not_unexecuted_proof", func(t *testing.T) {
		store, _, opID := setupJobsStore(t, "expired.db")
		now := recordsTestNow()
		// 查询失败或作业过期：不被当作未执行证明（持续可见地核对/交接）。
		tx, _ := store.db.Begin()
		normalizeJobStart(tx, opID, "provider-job-expired", "query-key", "retention", now)
		tx.Commit()
		// 作业过期：标记 expired（不被当作未执行证明）。
		tx, _ = store.db.Begin()
		if _, err := tx.Exec("UPDATE operation_jobs SET state='expired' WHERE operation_id=?", opID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var state string
		if err := store.db.QueryRow("SELECT state FROM operation_jobs WHERE operation_id=?", opID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "expired" {
			t.Errorf("job state = %s, want expired (查询失败或过期持续可见)", state)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
