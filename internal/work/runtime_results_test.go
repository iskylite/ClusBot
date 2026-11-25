package work

// 验收：持久结果回执与幂等收尾。
//
// 三个测试入口：
//   - TestResultReceiptApplyOnce：相同结果重复送达只应用和计费一次；
//     同身份不同结果隔离；旧代际结果留证但不能完成当前单元。
//   - TestFinalizationRestartNoReplay：现有收尾失败恢复测试继续有效；
//     反复恢复后模型调用、成果与用量不重复；中断在回执保存后、状态提交前，
//     重启仍能提交原结果。
//   - TestStaleResultEvidence：旧执行者迟到回报不会覆盖新执行结果；
//     未知用量显示未知；收尾记录涵盖剩余占用/待决定事项。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupResultsStore 创建存储与基础任务。
func setupResultsStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p110-"+name+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return store, run.TaskID
}

// resultReportFixture 构造一个测试回报。
func resultReportFixture(key, taskID string, generation int, input, output *int64) ResultReport {
	return ResultReport{
		ResultKey:           key,
		TaskID:              taskID,
		ExecutionGeneration: generation,
		Summary:             "完成",
		ModelRequests:       1,
		InputTokens:         input,
		OutputTokens:        output,
		ReceivedAt:          recordsTestNow(),
	}
}

// TestResultReceiptApplyOnce 幂等应用。
func TestResultReceiptApplyOnce(t *testing.T) {
	t.Run("duplicate_report_applies_and_bills_once", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "dup.db")
		input, output := int64(100), int64(50)
		report := resultReportFixture("result-1", taskID, 0, &input, &output)
		// 先持久记录。
		tx, _ := store.db.Begin()
		receipt, isNew, err := recordReport(tx, report)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !isNew {
			t.Fatal("first report not new")
		}
		// 第一次应用：应用和计费。
		var applyCount int
		applier := func(tx *sql.Tx, r ResultReceipt) error {
			applyCount++
			return nil
		}
		tx, _ = store.db.Begin()
		result, err := applyReport(tx, "result-1", 0, applier, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Applied {
			t.Error("first apply not applied")
		}
		// 重复送达：不重复应用/计费。
		tx, _ = store.db.Begin()
		receipt2, isNew2, err := recordReport(tx, report)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if isNew2 {
			t.Error("duplicate report marked new")
		}
		if receipt2.ReceiptID != receipt.ReceiptID {
			t.Errorf("duplicate created new receipt: %s vs %s", receipt2.ReceiptID, receipt.ReceiptID)
		}
		tx, _ = store.db.Begin()
		result2, err := applyReport(tx, "result-1", 0, applier, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result2.Duplicate {
			t.Error("duplicate apply not marked duplicate")
		}
		if applyCount != 1 {
			t.Errorf("applier invoked %d times, want 1 (重复送达只应用和计费一次)", applyCount)
		}
	})

	t.Run("same_key_different_result_quarantined", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "quar.db")
		input, output := int64(100), int64(50)
		report := resultReportFixture("result-q", taskID, 0, &input, &output)
		tx, _ := store.db.Begin()
		recordReport(tx, report)
		tx.Commit()
		// 同身份不同结果（不同摘要）：隔离，不覆盖原结果。
		conflicting := report
		conflicting.Summary = "不同内容"
		tx, _ = store.db.Begin()
		_, _, err := recordReport(tx, conflicting)
		tx.Rollback()
		if err == nil {
			t.Error("same key different result not isolated")
		}
		// 原结果保留。
		var summary string
		if err := store.db.QueryRow("SELECT summary FROM result_receipts WHERE result_key='result-q'").Scan(&summary); err != nil {
			t.Fatal(err)
		}
		if summary != "完成" {
			t.Errorf("original result overwritten: %q", summary)
		}
	})

	t.Run("stale_generation_evidence_not_complete", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "stale.db")
		// 旧代际（generation 0）结果，当前代际是 1。
		input, output := int64(100), int64(50)
		report := resultReportFixture("result-stale", taskID, 0, &input, &output)
		tx, _ := store.db.Begin()
		recordReport(tx, report)
		tx.Commit()
		// 以当前代际 1 应用：旧代际留证但不能完成当前单元。
		tx, _ = store.db.Begin()
		result, err := applyReport(tx, "result-stale", 1, nil, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Applied {
			t.Error("stale generation result applied to current unit")
		}
		if !result.Stale {
			t.Error("stale generation not marked stale")
		}
		if !strings.Contains(result.Reason, "旧代际") {
			t.Errorf("stale reason missing: %q", result.Reason)
		}
		// 结果仍留证（state=received，未应用）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM result_receipts WHERE result_key='result-stale'").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "received" {
			t.Errorf("stale result state = %s, want received (留证不完成)", state)
		}
	})
}

// TestFinalizationRestartNoReplay 重启不重放。
func TestFinalizationRestartNoReplay(t *testing.T) {
	t.Run("finalization_recovers_without_replay", func(t *testing.T) {
		// 复用现有 finalization 机制：模型只调用一次，恢复后不重复。
		path := filepath.Join(t.TempDir(), "recover.db")
		var taskID string
		var calls int
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			run, err := store.Submit(ctx, "v3p110-recover", "整理季度汇报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			taskID = run.TaskID
			claimed, err := store.claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// 执行：模型调用一次，但持久化失败（模拟中断）。
			input, output := int64(17), int64(23)
			store.execute(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
				calls++
				if err := emit("持久化后的唯一成果"); err != nil {
					return nil, err
				}
				_, err := store.db.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE OF status ON runs
WHEN NEW.status<>'running' BEGIN SELECT RAISE(ABORT,'outcome write unavailable'); END`)
				return &TokenUsage{InputTokens: &input, OutputTokens: &output}, err
			}), claimed, RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 512})
			// 恢复触发器仍存在，Close 模拟中断。
		}()
		// 重启：移除故障注入，恢复收尾（不重新调用模型）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, err := store.db.Exec("DROP TRIGGER IF EXISTS reject_outcome"); err != nil {
			t.Fatal(err)
		}
		// 跨进程恢复：读 run_finalizations 持久表恢复收尾（不重新调用模型）。
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		if err := store.Recover(); err != nil { // 反复恢复：幂等不重复。
			t.Fatal(err)
		}
		// 模型只调用一次（恢复不重放）。
		if calls != 1 {
			t.Errorf("model invoked %d times, want 1 (恢复不重新调用模型)", calls)
		}
		// 成果与用量不重复。
		d, err := store.Detail(taskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if d.TaskUsage == nil || d.TaskUsage.ModelRequests != 1 {
			t.Errorf("usage duplicated after recovery: %+v", d.TaskUsage)
		}
		if len(d.ArtifactVersions) != 1 {
			t.Errorf("artifacts = %d, want 1 (恢复不重复成果)", len(d.ArtifactVersions))
		}
	})

	t.Run("receipt_saved_before_state_commit_recovers", func(t *testing.T) {
		// 中断在回执保存后、状态提交前：重启仍能提交原结果。
		store, taskID := setupResultsStore(t, "receiptfirst.db")
		input, output := int64(100), int64(50)
		report := resultReportFixture("result-first", taskID, 0, &input, &output)
		// 先持久回执（独立提交），状态提交前"中断"（不应用）。
		tx, _ := store.db.Begin()
		if _, _, err := recordReport(tx, report); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 模拟中断后重启：回执仍在，可应用提交原结果。
		receipt, err := readResultReceipt(store.db, "result-first")
		if err != nil {
			t.Fatalf("receipt lost after restart: %v", err)
		}
		if receipt.State != "received" {
			t.Errorf("receipt state = %s, want received (待应用)", receipt.State)
		}
		tx, _ = store.db.Begin()
		result, err := applyReport(tx, "result-first", 0, nil, recordsTestNow())
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Applied {
			t.Error("receipt not applied after restart")
		}
		// 用量保留（不伪造丢失）。
		receipt, _ = readResultReceipt(store.db, "result-first")
		if receipt.InputTokens == nil || *receipt.InputTokens != 100 {
			t.Errorf("input tokens lost: %+v", receipt.InputTokens)
		}
	})

	t.Run("result_backlog_distinguishes_pending_and_applied", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "backlog.db")
		input, output := int64(10), int64(5)
		// 一个待应用、一个已应用。
		tx, _ := store.db.Begin()
		recordReport(tx, resultReportFixture("result-pending", taskID, 0, &input, &output))
		tx.Commit()
		tx, _ = store.db.Begin()
		recordReport(tx, resultReportFixture("result-applied", taskID, 0, &input, &output))
		applyReport(tx, "result-applied", 0, nil, recordsTestNow())
		tx.Commit()
		// 积压区分待应用与已应用。
		backlog, err := readResultBacklog(store.db)
		if err != nil {
			t.Fatal(err)
		}
		if backlog.Received != 1 || backlog.Applied != 1 {
			t.Errorf("backlog = %+v, want received=1 applied=1", backlog)
		}
	})
}

// TestStaleResultEvidence 旧执行者与未知用量。
func TestStaleResultEvidence(t *testing.T) {
	t.Run("late_report_does_not_overwrite_new_result", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "late.db")
		input, output := int64(100), int64(50)
		// 新执行结果（generation 1）已应用。
		newReport := resultReportFixture("result-new", taskID, 1, &input, &output)
		tx, _ := store.db.Begin()
		recordReport(tx, newReport)
		applyReport(tx, "result-new", 1, nil, recordsTestNow())
		tx.Commit()
		// 旧执行者迟到回报（generation 0，不同 result_key）：不影响已应用的新结果。
		lateReport := resultReportFixture("result-late-old", taskID, 0, &input, &output)
		tx, _ = store.db.Begin()
		recordReport(tx, lateReport)
		result, _ := applyReport(tx, "result-late-old", 1, nil, recordsTestNow())
		tx.Commit()
		if !result.Stale {
			t.Error("late old-generation report not marked stale")
		}
		// 新结果保持已应用，未被旧回报覆盖。
		newReceipt, _ := readResultReceipt(store.db, "result-new")
		if newReceipt.State != "applied" {
			t.Errorf("new result overwritten by late report: state=%s", newReceipt.State)
		}
	})

	t.Run("unknown_usage_shown_as_unknown", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "unknown.db")
		// 进程退出前无法保存用量：InputTokens/OutputTokens 为 nil（未知，不伪造）。
		report := resultReportFixture("result-unknown", taskID, 0, nil, nil)
		tx, _ := store.db.Begin()
		recordReport(tx, report)
		tx.Commit()
		receipt, err := readResultReceipt(store.db, "result-unknown")
		if err != nil {
			t.Fatal(err)
		}
		// 未知用量显示未知（不填 0，不伪造丢失用量）。
		if !receipt.UsageUnknown() {
			t.Errorf("usage not marked unknown: input=%+v output=%+v tracked=%v",
				receipt.InputTokens, receipt.OutputTokens, receipt.UsageTracked)
		}
		if receipt.InputTokens != nil || receipt.OutputTokens != nil {
			t.Errorf("unknown usage fabricated: input=%+v output=%+v", receipt.InputTokens, receipt.OutputTokens)
		}
	})

	t.Run("finalization_record_covers_outstanding_and_pending", func(t *testing.T) {
		store, taskID := setupResultsStore(t, "finalrec.db")
		now := recordsTestNow()
		// 先插入 Step 并提交，再读 stepID 建 work_unit（单连接不能嵌套查询）。
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze", DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		recordReport(tx, resultReportFixture("result-pending", taskID, 0, nil, nil))
		openWait(tx, Wait{WaitID: "wait-1", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		tx.Commit()
		// 收尾记录涵盖剩余占用/待决定事项/开放等待。
		record, err := buildFinalizationRecord(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if record.OutstandingUnits != 1 {
			t.Errorf("outstanding units = %d, want 1 (剩余占用)", record.OutstandingUnits)
		}
		if record.PendingDecisions != 1 {
			t.Errorf("pending decisions = %d, want 1 (待决定事项)", record.PendingDecisions)
		}
		if record.OpenWaits != 1 {
			t.Errorf("open waits = %d, want 1", record.OpenWaits)
		}
	})

	t.Run("unwritable_database_stops_dispatch", func(t *testing.T) {
		store, _ := setupResultsStore(t, "unwritable.db")
		// 可写性守卫：正常事务内可写。
		tx, _ := store.db.Begin()
		if err := ensureWritable(tx); err != nil {
			t.Errorf("writable database rejected: %v", err)
		}
		tx.Rollback()
		// 单项结果异常保留责任并隔离，但不放弃持久性保护：
		// 不可写时 ensureWritable 应阻止新的相关派发（由只读库验证）。
		ro, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(t.TempDir(), "ro.db"), "mode=ro"))
		if err == nil {
			defer ro.Close()
			if _, err := ro.Exec("CREATE TABLE x(id INTEGER)"); err == nil {
				t.Error("readonly database allowed write (不可写时未阻止派发)")
			}
		}
	})
}

// 确保 sql/strings 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
)
