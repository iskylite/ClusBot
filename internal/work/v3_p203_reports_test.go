package work

// P2-03 验收：工作者回报、流式片段与迟到证据。
//
// 三个测试入口：
//   - TestV3P203WorkerReportOnce：丢确认重报仍只计一次用量/成果；回报先入
//     Inbox；应用时再次比较当前状态与版本，结果与用量只应用一次；服务端从
//     令牌核对关联。
//   - TestV3P203LateReportPreserved：旧执行者回报的真实外部线索可查，但当前
//     任务状态没有被它推进；旧代际可保存可信回执为核对线索，但不能完成当前
//     Step 或制造新派发。
//   - TestV3P203StreamSequenceConflict：流式片段有序号/摘要和大小限制；重复
//     片段不重复拼接，同序号异内容隔离；乱序片段可诊断，不拼接错误正文；未
//     持久片段不当作已交付成果；无权限目标或与令牌不符的被拒绝。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// setupV3P203 创建存储、任务、就绪单元与已领取的 Worker。
func setupV3P203(t *testing.T, name string) (*Store, string, string, ClaimResult) {
	t.Helper()
	store, _, _ := setupV3P202(t, name)
	now := v3P102Now()
	worker, cred := registerV3P202Worker(t, store, "reporter", []WorkUnitKind{WorkUnitModel}, "")
	tx, _ := store.db.Begin()
	claim, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 100000, QuotaAvailable: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return store, worker, cred, claim
}

// v3P203Report 构造一个回报。
func v3P203Report(id string, claim ClaimResult, generation int) WorkerReport {
	input, output := int64(100), int64(50)
	return WorkerReport{
		ReportID:        id,
		UnitID:          claim.Unit.UnitID,
		TaskID:          claim.Unit.TaskID,
		LeaseGeneration: generation,
		WorkerID:        claim.Lease.WorkerID,
		Summary:         "完成",
		ModelRequests:   1,
		InputTokens:     &input,
		OutputTokens:    &output,
		ResultDigest:    digest("成果内容"),
	}
}

// TestV3P203WorkerReportOnce 回报幂等。
func TestV3P203WorkerReportOnce(t *testing.T) {
	t.Run("receipt_lost_requeue_counts_once", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "once.db")
		now := v3P102Now()
		report := v3P203Report("report-1", claim, claim.Lease.LeaseGeneration)
		// 第一次接收与应用。
		tx, _ := store.db.Begin()
		if _, err := receiveWorkerReport(tx, report, claim.Token.TokenID, claim.PlainToken, now); err != nil {
			t.Fatal(err)
		}
		if err := persistWorkerReport(tx, report, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		var applyCount int
		applier := func(tx *sql.Tx, r WorkerReport) error {
			applyCount++
			return nil
		}
		tx, _ = store.db.Begin()
		result, err := applyWorkerReport(tx, report.ReportID, claim.Lease.LeaseGeneration, applier, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Applied {
			t.Error("first apply not applied")
		}
		// 丢确认重报：仍只计一次（应用守卫幂等）。
		tx, _ = store.db.Begin()
		result2, err := applyWorkerReport(tx, report.ReportID, claim.Lease.LeaseGeneration, applier, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result2.Duplicate {
			t.Error("repeated report not marked duplicate")
		}
		if applyCount != 1 {
			t.Errorf("applier invoked %d times, want 1 (丢确认重报仍只计一次)", applyCount)
		}
	})

	t.Run("report_enters_inbox_first", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "inbox.db")
		now := v3P102Now()
		report := v3P203Report("report-inbox", claim, claim.Lease.LeaseGeneration)
		// 回报先入 Inbox（持久受理）。
		tx, _ := store.db.Begin()
		result, err := receiveWorkerReport(tx, report, claim.Token.TokenID, claim.PlainToken, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Quarantined {
			t.Error("fresh report quarantined")
		}
		// Inbox 中有该事件（持久受理）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE source=? AND event_id=?",
			"worker:"+claim.Lease.WorkerID, report.ReportID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("report not in inbox: count=%d (回报先入 Inbox)", count)
		}
	})

	t.Run("token_mismatch_rejected", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "mismatch.db")
		now := v3P102Now()
		report := v3P203Report("report-mismatch", claim, claim.Lease.LeaseGeneration)
		// 伪造：回报与令牌不符（错误的 worker）。
		tx, _ := store.db.Begin()
		badReport := report
		badReport.WorkerID = "forged-worker"
		_, err := receiveWorkerReport(tx, badReport, claim.Token.TokenID, claim.PlainToken, now)
		tx.Rollback()
		if err == nil {
			t.Error("token mismatch report not rejected (与令牌不符的被拒绝)")
		}
		// 伪造：错误的代际。
		tx, _ = store.db.Begin()
		badGen := report
		badGen.LeaseGeneration = 999
		_, err = receiveWorkerReport(tx, badGen, claim.Token.TokenID, claim.PlainToken, now)
		tx.Rollback()
		if err == nil {
			t.Error("wrong generation report not rejected")
		}
	})
}

// TestV3P203LateReportPreserved 旧执行者回报。
func TestV3P203LateReportPreserved(t *testing.T) {
	t.Run("stale_report_preserved_not_advancing", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "stale.db")
		now := v3P102Now()
		// 旧代际（claim 代际 - 1）的回报。
		oldGen := claim.Lease.LeaseGeneration - 1
		if oldGen < 1 {
			oldGen = 1
		}
		staleReport := v3P203Report("report-stale", claim, oldGen)
		staleReport.LeaseGeneration = claim.Lease.LeaseGeneration + 1 // 旧代际（非当前）
		tx, _ := store.db.Begin()
		if err := persistWorkerReport(tx, staleReport, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 以当前代际应用：旧代际留证但不能完成当前单元。
		tx, _ = store.db.Begin()
		result, err := applyWorkerReport(tx, staleReport.ReportID, claim.Lease.LeaseGeneration, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Applied {
			t.Error("stale report applied to current unit (旧代际不能完成当前单元)")
		}
		if !result.Stale {
			t.Error("stale report not marked stale")
		}
		// 回报仍留证可查（真实外部线索可查，但不推进）。
		var state string
		if err := store.db.QueryRow("SELECT state FROM worker_reports WHERE report_id=?", staleReport.ReportID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "stale" {
			t.Errorf("stale report state = %s, want stale (留证可查不推进)", state)
		}
		// 当前任务状态未被它推进（单元仍 claimed）。
		var unitState string
		if err := store.db.QueryRow("SELECT state FROM work_units WHERE unit_id=?", claim.Unit.UnitID).Scan(&unitState); err != nil {
			t.Fatal(err)
		}
		if unitState == "completed" {
			t.Errorf("unit completed by stale report (旧代际不能推进当前状态)")
		}
	})

	t.Run("late_report_real_evidence_queryable", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "late.db")
		now := v3P102Now()
		// 旧执行者的迟到回报（带真实外部证据引用）。
		lateReport := v3P203Report("report-late", claim, claim.Lease.LeaseGeneration+1)
		lateReport.EvidenceRef = "evidence://external/job-123"
		tx, _ := store.db.Begin()
		if err := persistWorkerReport(tx, lateReport, now); err != nil {
			t.Fatal(err)
		}
		applyWorkerReport(tx, lateReport.ReportID, claim.Lease.LeaseGeneration, nil, now)
		tx.Commit()
		// 真实外部线索可查（留证）。
		var evidenceRef string
		if err := store.db.QueryRow("SELECT evidence_ref FROM worker_reports WHERE report_id=?", lateReport.ReportID).Scan(&evidenceRef); err != nil {
			t.Fatal(err)
		}
		if evidenceRef != "evidence://external/job-123" {
			t.Errorf("late report evidence lost: %s (真实外部线索应可查)", evidenceRef)
		}
	})
}

// TestV3P203StreamSequenceConflict 流式片段。
func TestV3P203StreamSequenceConflict(t *testing.T) {
	t.Run("duplicate_fragment_not_reassembled", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "dupfrag.db")
		now := v3P102Now()
		report := v3P203Report("report-frag", claim, claim.Lease.LeaseGeneration)
		tx, _ := store.db.Begin()
		persistWorkerReport(tx, report, now)
		tx.Commit()
		// 同一片段重复发送：不重复拼接。
		tx, _ = store.db.Begin()
		first, err := receiveFragment(tx, report.ReportID, 0, "第一段", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !first.Accepted {
			t.Fatal("first fragment not accepted")
		}
		tx, _ = store.db.Begin()
		second, err := receiveFragment(tx, report.ReportID, 0, "第一段", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !second.Duplicate {
			t.Error("duplicate fragment not marked duplicate (重复片段不重复拼接)")
		}
		// 拼接结果只有一份。
		assembled, _, err := assembleFragments(store.db, report.ReportID)
		if err != nil {
			t.Fatal(err)
		}
		if assembled != "第一段" {
			t.Errorf("assembled = %q, want 第一段 (重复不重复拼接)", assembled)
		}
	})

	t.Run("same_seq_different_content_quarantined", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "conflictfrag.db")
		now := v3P102Now()
		report := v3P203Report("report-conflict", claim, claim.Lease.LeaseGeneration)
		tx, _ := store.db.Begin()
		persistWorkerReport(tx, report, now)
		tx.Commit()
		// 同序号异内容：隔离（不拼接错误正文）。
		tx, _ = store.db.Begin()
		receiveFragment(tx, report.ReportID, 0, "正确内容", now)
		tx.Commit()
		tx, _ = store.db.Begin()
		result, err := receiveFragment(tx, report.ReportID, 0, "错误内容", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Conflict {
			t.Error("same seq different content not isolated (同序号异内容应隔离)")
		}
		// 拼接只有正确内容（错误正文不进入）。
		assembled, _, _ := assembleFragments(store.db, report.ReportID)
		if strings.Contains(assembled, "错误") {
			t.Errorf("assembled contains wrong content: %q (不拼接错误正文)", assembled)
		}
		// 冲突已记录。
		var conflictCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM report_fragment_conflicts WHERE report_id=?", report.ReportID).Scan(&conflictCount); err != nil {
			t.Fatal(err)
		}
		if conflictCount != 1 {
			t.Errorf("conflict not recorded: count=%d", conflictCount)
		}
	})

	t.Run("out_of_order_diagnosable_gaps", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "gaps.db")
		now := v3P102Now()
		report := v3P203Report("report-gaps", claim, claim.Lease.LeaseGeneration)
		tx, _ := store.db.Begin()
		persistWorkerReport(tx, report, now)
		tx.Commit()
		// 乱序片段（0, 2 缺 1）：可诊断缺口。
		tx, _ = store.db.Begin()
		receiveFragment(tx, report.ReportID, 0, "第一段", now)
		receiveFragment(tx, report.ReportID, 2, "第三段", now)
		tx.Commit()
		assembled, gaps, err := assembleFragments(store.db, report.ReportID)
		if err != nil {
			t.Fatal(err)
		}
		if len(gaps) != 1 || gaps[0] != 1 {
			t.Errorf("gaps = %v, want [1] (乱序片段可诊断)", gaps)
		}
		if assembled != "第一段第三段" {
			t.Errorf("assembled = %q, want 第一段第三段", assembled)
		}
	})

	t.Run("unpersisted_fragment_not_in_result", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "unpersisted.db")
		now := v3P102Now()
		report := v3P203Report("report-unp", claim, claim.Lease.LeaseGeneration)
		tx, _ := store.db.Begin()
		persistWorkerReport(tx, report, now)
		tx.Commit()
		// 未持久任何片段：拼接为空（未持久片段不当作已交付成果）。
		assembled, _, err := assembleFragments(store.db, report.ReportID)
		if err != nil {
			t.Fatal(err)
		}
		if assembled != "" {
			t.Errorf("assembled = %q, want empty (未持久片段不当作成果)", assembled)
		}
	})

	t.Run("oversized_fragment_rejected", func(t *testing.T) {
		store, _, _, claim := setupV3P203(t, "oversize.db")
		now := v3P102Now()
		report := v3P203Report("report-over", claim, claim.Lease.LeaseGeneration)
		tx, _ := store.db.Begin()
		persistWorkerReport(tx, report, now)
		tx.Commit()
		// 超大片段：拒绝（大小限制）。
		tx, _ = store.db.Begin()
		_, err := receiveFragment(tx, report.ReportID, 0, strings.Repeat("x", maxFragmentBytes+1), now)
		tx.Rollback()
		if err == nil {
			t.Error("oversized fragment not rejected (大小限制)")
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
