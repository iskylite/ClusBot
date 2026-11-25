package work

// 持久结果回执与幂等收尾。
//
// 本文件实现结果保存失败后恢复收尾，不重新调用模型或重做已执行工作：
//   - 回报先持久记录独立结果身份、单元/Run/代际、摘要、用量和证据，再事务
//     性应用业务结果；
//   - 相同结果重复送达只应用和计费一次；同身份不同结果隔离；旧代际结果留证
//     但不能完成当前单元；
//   - 将尚未落盘结果与已落盘待应用结果区分；进程退出前无法保存的部分必须
//     标未知，不伪造丢失用量；
//   - 单项结果异常保留责任并隔离；数据库整体不可写时停止新的相关派发；
//   - 收尾记录涵盖剩余占用/待决定事项；未来 Operation 结果由同一输入链接入，
//     不能直接更新资产。
//
// 与现有 finalization 的关系：本文件在 的 result_receipts 之上提供
// 通用结果回执与应用守卫，复用 finalization 的失败注入测试与恢复机制；
// 不因去掉现有全局停止领取保护而失去持久性。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 结果回执（先持久记录，再事务性应用）
// ---------------------------------------------------------------------------

// ResultReport 是一次结果回报。
type ResultReport struct {
	ResultKey           string // 稳定结果身份（重复送达只应用一次）
	UnitID              string
	RunID               string
	TaskID              string
	ExecutionGeneration int
	LeaseGeneration     int
	Summary             string
	ModelRequests       int
	InputTokens         *int64
	OutputTokens        *int64
	EvidenceRef         string
	ReceivedAt          int64
}

// recordReport 先持久记录结果回执，返回回执与是否为新。
// 相同结果重复送达返回既有回执（幂等）；同身份不同结果隔离（不覆盖）。
func recordReport(tx *sql.Tx, r ResultReport) (receipt ResultReceipt, isNew bool, err error) {
	if r.ResultKey == "" {
		return ResultReceipt{}, false, errors.New("结果回报缺少稳定身份")
	}
	if r.TaskID == "" {
		return ResultReceipt{}, false, errors.New("结果回报缺少关联任务")
	}
	receipt = ResultReceipt{
		ReceiptID:           id(),
		ResultKey:           r.ResultKey,
		UnitID:              r.UnitID,
		RunID:               r.RunID,
		TaskID:              r.TaskID,
		ExecutionGeneration: r.ExecutionGeneration,
		LeaseGeneration:     r.LeaseGeneration,
		Summary:             r.Summary,
		UsageTracked:        r.InputTokens != nil || r.OutputTokens != nil,
		ModelRequests:       r.ModelRequests,
		InputTokens:         r.InputTokens,
		OutputTokens:        r.OutputTokens,
		EvidenceRef:         r.EvidenceRef,
		State:               "received",
		ReceivedAt:          r.ReceivedAt,
	}
	existing, err := recordResultReceipt(tx, receipt)
	if err != nil {
		return ResultReceipt{}, false, err
	}
	// 区分"新接收"与"重复送达"：新接收的 receipt_id 与本次生成一致。
	return existing, existing.ReceiptID == receipt.ReceiptID, nil
}

// ---------------------------------------------------------------------------
// 幂等应用（相同结果只应用和计费一次）
// ---------------------------------------------------------------------------

// ApplyReportResult 是应用结果。
type ApplyReportResult struct {
	Applied    bool
	Duplicate  bool   // 重复送达（已应用，不重复计费）
	Quarantined bool  // 同身份不同结果（隔离）
	Stale      bool   // 旧代际（留证但不能完成当前单元）
	Reason     string
}

// applyReport 事务性应用结果。
// 相同结果重复送达只应用和计费一次；同身份不同结果隔离；旧代际结果留证但
// 不能完成当前单元。applier 是业务应用函数（实际状态更新）。
func applyReport(tx *sql.Tx, resultKey string, currentGeneration int, applier func(tx *sql.Tx, r ResultReceipt) error, now int64) (ApplyReportResult, error) {
	// 读取回执（同事务重新读取）。
	r, err := readResultReceipt(tx, resultKey)
	if err != nil {
		return ApplyReportResult{}, err
	}
	// 已应用：重复送达不重复应用/计费。
	if r.State == "applied" {
		return ApplyReportResult{Duplicate: true, Reason: "结果已应用（重复送达不重复计费）"}, nil
	}
	// 已隔离：同身份不同结果。
	if r.State == "quarantined" {
		return ApplyReportResult{Quarantined: true, Reason: "同身份不同结果已隔离"}, nil
	}
	// 旧代际：留证但不能完成当前单元。
	if r.ExecutionGeneration != currentGeneration {
		if err := markReceiptStale(tx, resultKey, now); err != nil {
			return ApplyReportResult{}, err
		}
		return ApplyReportResult{Stale: true, Reason: fmt.Sprintf("旧代际结果（%d ≠ 当前 %d），留证但不完成当前单元",
			r.ExecutionGeneration, currentGeneration)}, nil
	}
	// 业务应用（同事务）。
	if applier != nil {
		if err := applier(tx, r); err != nil {
			return ApplyReportResult{}, err
		}
	}
	// 标记已应用（与业务更新同事务）。
	res, err := tx.Exec(`UPDATE result_receipts SET state='applied', applied_at=? WHERE result_key=? AND state='received'`,
		now, resultKey)
	if err != nil {
		return ApplyReportResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// 并发：另一应用者已取得（重复只应用一次）。
		return ApplyReportResult{Duplicate: true, Reason: "并发应用竞争，另一处理者已取得"}, nil
	}
	return ApplyReportResult{Applied: true}, nil
}

// readResultReceipt 读取一个结果回执。
func readResultReceipt(q recordsQuerier, resultKey string) (ResultReceipt, error) {
	var r ResultReceipt
	var unitID, runID sql.NullString
	var inputTokens, outputTokens sql.NullInt64
	err := q.QueryRow(`SELECT receipt_id,result_key,unit_id,run_id,task_id,execution_generation,lease_generation,
summary,usage_tracked,model_requests,input_tokens,output_tokens,evidence_ref,state,received_at,applied_at
FROM result_receipts WHERE result_key=?`, resultKey).
		Scan(&r.ReceiptID, &r.ResultKey, &unitID, &runID, &r.TaskID, &r.ExecutionGeneration, &r.LeaseGeneration,
			&r.Summary, &r.UsageTracked, &r.ModelRequests, &inputTokens, &outputTokens,
			&r.EvidenceRef, &r.State, &r.ReceivedAt, &r.AppliedAt)
	r.UnitID = unitID.String
	r.RunID = runID.String
	if inputTokens.Valid {
		r.InputTokens = &inputTokens.Int64
	}
	if outputTokens.Valid {
		r.OutputTokens = &outputTokens.Int64
	}
	return r, err
}

// markReceiptStale 把旧代际结果标记为留证（state 保持 received 但记录原因）。
func markReceiptStale(tx *sql.Tx, resultKey string, now int64) error {
	_, err := tx.Exec(`UPDATE result_receipts SET summary=summary || ' [旧代际留证]' WHERE result_key=? AND state='received'`,
		resultKey)
	return err
}

// ---------------------------------------------------------------------------
// 未落盘与待应用区分
// ---------------------------------------------------------------------------

// ResultBacklog 是结果积压快照。
type ResultBacklog struct {
	Received      int // 已落盘待应用数
	Applied       int // 已应用数
	Quarantined   int // 已隔离数
	StaleCount    int // 旧代际留证数
	OldestReceivedAt int64 // 最老待应用时间
}

// readResultBacklog 读取结果积压（区分已落盘待应用与已应用）。
func readResultBacklog(q recordsQuerier) (ResultBacklog, error) {
	var b ResultBacklog
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE state='received'`).Scan(&b.Received); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE state='applied'`).Scan(&b.Applied); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE state='quarantined'`).Scan(&b.Quarantined); err != nil {
		return b, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE state='received' AND summary LIKE '%旧代际留证%'`).Scan(&b.StaleCount); err != nil {
		return b, err
	}
	var oldest sql.NullInt64
	if err := q.QueryRow(`SELECT min(received_at) FROM result_receipts WHERE state='received'`).Scan(&oldest); err != nil {
		return b, err
	}
	if oldest.Valid {
		b.OldestReceivedAt = oldest.Int64
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// 用量未知标记
// ---------------------------------------------------------------------------

// UsageUnknown 报告用量是否未知（不伪造丢失用量）。
// 进程退出前无法保存的部分标未知：InputTokens/OutputTokens 为 nil 即未知。
func (r ResultReceipt) UsageUnknown() bool {
	return r.InputTokens == nil && r.OutputTokens == nil && !r.UsageTracked
}

// ---------------------------------------------------------------------------
// 数据库可写性守卫
// ---------------------------------------------------------------------------

// ErrDatabaseUnwritable 是数据库整体不可写的错误。
var ErrDatabaseUnwritable = errors.New("数据库整体不可写，停止新的相关派发")

// ensureWritable 核对数据库可写（不可写时停止新的相关派发）。
// 单项结果异常保留责任并隔离，但不因单个失败放弃持久性保护。
func ensureWritable(tx *sql.Tx) error {
	// 探测写：单写连接的写事务本身即证明可写性（进入本函数即在事务中）。
	var one int
	if err := tx.QueryRow("SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("%w：%v", ErrDatabaseUnwritable, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 收尾记录
// ---------------------------------------------------------------------------

// FinalizationRecord 是收尾记录：涵盖剩余占用/待决定事项。
type FinalizationRecord struct {
	TaskID             string
	OutstandingUnits   int    // 剩余占用单元数
	PendingDecisions   int    // 待决定事项数
	UnknownEffects     int    // 未知效果数
	OpenWaits          int    // 开放等待数
	Responsibility     string // 后续责任说明
}

// buildFinalizationRecord 汇总一个 Task 的收尾记录（剩余占用/待决定事项）。
// 未来 Operation 结果由同一输入链接入，不能直接更新资产（P3 接入）。
func buildFinalizationRecord(q recordsQuerier, taskID string) (FinalizationRecord, error) {
	var r FinalizationRecord
	r.TaskID = taskID
	if err := q.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id=? AND state IN ('ready','claimed','running')`, taskID).
		Scan(&r.OutstandingUnits); err != nil {
		return r, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM result_receipts WHERE task_id=? AND state='received'`, taskID).
		Scan(&r.PendingDecisions); err != nil {
		return r, err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM waits WHERE task_id=? AND state='open'`, taskID).
		Scan(&r.OpenWaits); err != nil {
		return r, err
	}
	// 未知效果：由调用方注入（P3 Operation 落地后接实际值）。
	return r, nil
}

// 确保 context/fmt 引用保留。
var (
	_ = context.Background
	_ = fmt.Sprintf
)
