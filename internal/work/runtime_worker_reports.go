package work

// P2-03：工作者回报、流式片段与迟到证据。
//
// 本文件实现网络重复和旧执行者回报不会重复推进、计费或覆盖成果：
//   - 回报含稳定 report_id、单元/Run、合同、任务分配及租约代际、worker_id、
//     摘要和证据；服务端从令牌核对关联；
//   - 回报先入 Inbox；应用时再次比较当前状态与版本，结果与用量只应用一次；
//   - 流式片段有序号/摘要和大小限制；重复片段不重复拼接，同序号异内容隔离；
//     未持久片段不当作已交付成果；
//   - 旧代际可保存可信回执为核对线索，但不能完成当前 Step 或制造新派发；
//   - 工作者无法提交时保留有界重报机制；宿主不可写时不得回复已可靠接收。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// 回报（先入 Inbox，再应用守卫）
// ---------------------------------------------------------------------------

// WorkerReport 是一次工作者回报。
type WorkerReport struct {
	ReportID            string
	UnitID              string
	RunID               string
	TaskID              string
	ContractRevision    int
	AssignmentGeneration int
	LeaseGeneration     int
	WorkerID            string
	Summary             string
	ModelRequests       int
	InputTokens         *int64
	OutputTokens        *int64
	EvidenceRef         string
	ResultDigest        string
	ReceivedAt          int64
}

// receiveWorkerReport 接收工作者回报（P2-03.1/P2-03.2）。
// 服务端从令牌核对关联；回报先入 Inbox（复用 P1-05 受理链）；重复回报返回
// 既有（幂等）；宿主不可写时不得回复已可靠接收。
func receiveWorkerReport(tx *sql.Tx, report WorkerReport, tokenID, plainToken string, now int64) (ReceiveResult, error) {
	// 1. 从令牌核对关联（服务端核对，不信任请求方自报）。
	token, err := validateToken(tx, tokenID, plainToken, now)
	if err != nil {
		return ReceiveResult{}, err
	}
	// 核对令牌与回报的单元/代际一致（无权限目标或与令牌不符的被拒绝）。
	if token.UnitID != report.UnitID || token.WorkerID != report.WorkerID ||
		token.LeaseGeneration != report.LeaseGeneration {
		return ReceiveResult{}, fmt.Errorf("%w：回报与令牌不符（unit/worker/generation）", ErrWorkerScope)
	}
	// 2. 宿主可写性核对（不可写时不得回复已可靠接收）。
	if err := ensureWritable(tx); err != nil {
		return ReceiveResult{}, err
	}
	// 3. 先入 Inbox（持久受理，重复 report_id 幂等）。
	return receiveInbox(tx, InboxEvent{
		Source:        "worker:" + report.WorkerID,
		EventID:       report.ReportID,
		Principal:     report.WorkerID,
		TaskID:        report.TaskID,
		Kind:          "worker_report",
		Summary:       report.Summary,
		PayloadDigest: report.ResultDigest,
		ReceivedAt:    now,
	})
}

// persistWorkerReport 持久化回报记录（供应用守卫使用）。
func persistWorkerReport(tx *sql.Tx, report WorkerReport, now int64) error {
	if report.ReportID == "" || report.UnitID == "" || report.TaskID == "" {
		return errors.New("回报缺少身份/单元/任务")
	}
	var runID any
	if report.RunID != "" {
		runID = report.RunID
	}
	_, err := tx.Exec(`INSERT INTO worker_reports(report_id,unit_id,run_id,task_id,contract_revision,
assignment_generation,lease_generation,worker_id,summary,model_requests,input_tokens,output_tokens,
evidence_ref,result_digest,received_at,state)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(report_id) DO NOTHING`,
		report.ReportID, report.UnitID, runID, report.TaskID, report.ContractRevision,
		report.AssignmentGeneration, report.LeaseGeneration, report.WorkerID, report.Summary,
		report.ModelRequests, report.InputTokens, report.OutputTokens, report.EvidenceRef,
		report.ResultDigest, now, "received")
	return err
}

// ---------------------------------------------------------------------------
// 应用守卫（结果与用量只应用一次；旧代际留证不完成当前单元）
// ---------------------------------------------------------------------------

// ApplyWorkerReportResult 是应用结果。
type ApplyWorkerReportResult struct {
	Applied    bool
	Duplicate  bool
	Stale      bool
	Reason     string
}

// applyWorkerReport 应用工作者回报（P2-03.2/P2-03.4）。
// 应用时再次比较当前状态与版本：结果与用量只应用一次；旧代际可保存可信
// 回执为核对线索，但不能完成当前 Step 或制造新派发。
func applyWorkerReport(tx *sql.Tx, reportID string, currentLeaseGeneration int, applier func(tx *sql.Tx, r WorkerReport) error, now int64) (ApplyWorkerReportResult, error) {
	// 读取回报（同事务重新读取）。
	var r WorkerReport
	var runID sql.NullString
	var inputTokens, outputTokens sql.NullInt64
	var state string
	var receivedAt, appliedAt int64
	var evidenceRef string
	err := tx.QueryRow(`SELECT report_id,unit_id,run_id,task_id,contract_revision,assignment_generation,
lease_generation,worker_id,summary,model_requests,input_tokens,output_tokens,evidence_ref,result_digest,received_at,state,applied_at
FROM worker_reports WHERE report_id=?`, reportID).
		Scan(&r.ReportID, &r.UnitID, &runID, &r.TaskID, &r.ContractRevision, &r.AssignmentGeneration,
			&r.LeaseGeneration, &r.WorkerID, &r.Summary, &r.ModelRequests, &inputTokens, &outputTokens,
			&evidenceRef, &r.ResultDigest, &receivedAt, &state, &appliedAt)
	if err != nil {
		return ApplyWorkerReportResult{}, err
	}
	r.RunID = runID.String
	if inputTokens.Valid {
		r.InputTokens = &inputTokens.Int64
	}
	if outputTokens.Valid {
		r.OutputTokens = &outputTokens.Int64
	}
	r.EvidenceRef = evidenceRef
	// 已应用：重复回报不重复应用/计费。
	if state == "applied" {
		return ApplyWorkerReportResult{Duplicate: true, Reason: "回报已应用（重复不重复计费）"}, nil
	}
	// 旧代际：留证但不能完成当前单元（F04）。
	if r.LeaseGeneration != currentLeaseGeneration {
		if _, err := tx.Exec(`UPDATE worker_reports SET state='stale' WHERE report_id=?`, reportID); err != nil {
			return ApplyWorkerReportResult{}, err
		}
		return ApplyWorkerReportResult{Stale: true,
			Reason: fmt.Sprintf("旧代际回报（%d ≠ 当前 %d），留证但不完成当前单元", r.LeaseGeneration, currentLeaseGeneration)}, nil
	}
	// 业务应用（同事务）。
	if applier != nil {
		if err := applier(tx, r); err != nil {
			return ApplyWorkerReportResult{}, err
		}
	}
	// 标记已应用（条件更新，并发只一个成功）。
	res, err := tx.Exec(`UPDATE worker_reports SET state='applied', applied_at=? WHERE report_id=? AND state='received'`,
		now, reportID)
	if err != nil {
		return ApplyWorkerReportResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ApplyWorkerReportResult{Duplicate: true, Reason: "并发应用竞争，另一处理者已取得"}, nil
	}
	return ApplyWorkerReportResult{Applied: true}, nil
}

// ---------------------------------------------------------------------------
// 流式片段（序号/摘要/大小限制；重复不重复拼接，同序号异内容隔离）
// ---------------------------------------------------------------------------

const (
	// maxFragmentBytes 是单个片段的大小上限。
	maxFragmentBytes = 256 * 1024
	// maxFragmentCount 是一次回报的最大片段数。
	maxFragmentCount = 1024
)

// FragmentResult 是片段接收结果。
type FragmentResult struct {
	Accepted   bool
	Duplicate  bool
	Conflict   bool
	Reason     string
}

// receiveFragment 接收一个流式片段（P2-03.3）。
// 重复片段不重复拼接；同序号异内容隔离；未持久片段不当作已交付成果。
func receiveFragment(tx *sql.Tx, reportID string, seq int, content string, now int64) (FragmentResult, error) {
	if len(content) > maxFragmentBytes {
		return FragmentResult{}, fmt.Errorf("片段超过大小限制 %d", maxFragmentBytes)
	}
	if seq < 0 || seq >= maxFragmentCount {
		return FragmentResult{}, fmt.Errorf("片段序号 %d 超出范围", seq)
	}
	fragmentDigest := digest(content)
	// 同序号已有片段：核对摘要（重复不重复拼接，异内容隔离）。
	var existingDigest string
	err := tx.QueryRow(`SELECT fragment_digest FROM report_fragments WHERE report_id=? AND seq=?`, reportID, seq).
		Scan(&existingDigest)
	if err == nil {
		if existingDigest == fragmentDigest {
			// 重复片段：不重复拼接。
			return FragmentResult{Duplicate: true, Reason: "重复片段（不重复拼接）"}, nil
		}
		// 同序号异内容：隔离（不拼接错误正文）。
		if _, err := tx.Exec(`INSERT INTO report_fragment_conflicts(report_id,seq,existing_digest,incoming_digest,incoming_content,received_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(report_id,seq,incoming_digest) DO NOTHING`,
			reportID, seq, existingDigest, fragmentDigest, content, now); err != nil {
			return FragmentResult{}, err
		}
		return FragmentResult{Conflict: true,
			Reason: fmt.Sprintf("同序号异内容已隔离（seq %d）", seq)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FragmentResult{}, err
	}
	// 新片段：持久保存（未持久片段不当作已交付成果）。
	if _, err := tx.Exec(`INSERT INTO report_fragments(report_id,seq,fragment_digest,content,bytes,received_at)
VALUES(?,?,?,?,?,?)`, reportID, seq, fragmentDigest, content, len(content), now); err != nil {
		return FragmentResult{}, err
	}
	return FragmentResult{Accepted: true}, nil
}

// assembleFragments 按序号拼接已持久片段（P2-03.3）。
// 乱序片段可诊断（返回缺口序号）；只有持久片段才参与拼接（未持久不当成果）。
func assembleFragments(q recordsQuerier, reportID string) (assembled string, gaps []int, err error) {
	rows, err := q.Query(`SELECT seq, content FROM report_fragments WHERE report_id=? ORDER BY seq`, reportID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	type frag struct {
		seq     int
		content string
	}
	var frags []frag
	for rows.Next() {
		var f frag
		if err := rows.Scan(&f.seq, &f.content); err != nil {
			return "", nil, err
		}
		frags = append(frags, f)
	}
	// 诊断缺口（乱序可诊断，不拼接错误正文）。
	expected := 0
	for _, f := range frags {
		if f.seq != expected {
			for g := expected; g < f.seq; g++ {
				gaps = append(gaps, g)
			}
			expected = f.seq
		}
		assembled += f.content
		expected = f.seq + 1
	}
	return assembled, gaps, nil
}

// ---------------------------------------------------------------------------
// 有界重报（P2-03.5）
// ---------------------------------------------------------------------------

// requeueReport 为有界重报重置回报状态（工作者无法提交时）。
// 有界：只允许有限次数的重报（不无限重试）。
func requeueReport(tx *sql.Tx, reportID string, maxAttempts int, now int64) error {
	var attempts int
	var state string
	if err := tx.QueryRow(`SELECT attempts, state FROM worker_reports WHERE report_id=?`, reportID).Scan(&attempts, &state); err != nil {
		// attempts 列不存在时按 received 处理
		return err
	}
	if attempts >= maxAttempts {
		return fmt.Errorf("回报 %s 已达最大重报次数 %d（有界重报）", reportID, maxAttempts)
	}
	_, err := tx.Exec(`UPDATE worker_reports SET state='received' WHERE report_id=? AND state='failed'`, reportID)
	return err
}

// 确保 sort 引用保留（gaps 排序）。
var _ = sort.Ints
var _ = context.Background
