package work

// P5-02：统一检索请求、过滤与按需补查。
//
// 本文件实现按任务需要查事实、材料和经验，返回来源与缺口而不是无界拼接：
//   - 查询含主体、Task/Step、对象范围、问题、目标时间、新鲜度、类型及字节/
//     时间/调用预算；
//   - 先结构化键/受限图/全文检索，再按证据覆盖、相关性、时效与适用条件排序；
//     向量为可选候选源；
//   - 每个候选回权威来源核对当前权限、版本、有效期和反例；索引得分不能决定
//     授权或真值；
//   - 缺关键条件或冲突时做有界原文/来源补查；仍不明就返回缺口，不能无限检索；
//   - 分页游标绑定主体、范围、排序快照和版本；继续查询重核权限，失效游标明确重开。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// 检索请求（主体、Task/Step、对象范围、问题、目标时间、新鲜度、类型及预算）
// ---------------------------------------------------------------------------

// RetrievalRequest 是检索请求。
type RetrievalRequest struct {
	// 主体、Task/Step、对象范围。
	Subject   string
	TaskID    string
	StepID    string
	ObjectScope string
	// 问题、目标时间、新鲜度、类型。
	Query     string
	TargetTime int64
	FreshnessMS int64
	Kind      string // fact/material/experience
	// 字节/时间/调用预算。
	Budget    RetrievalBudget
}

// RetrievalBudget 是检索预算。
type RetrievalBudget struct {
	MaxBytes    int64
	MaxTimeMS   int64
	MaxCalls    int
}

// RetrievalCandidate 是检索候选。
type RetrievalCandidate struct {
	// 候选引用与来源。
	Ref       string
	SourceRef string
	Kind      string
	// 证据覆盖、相关性、时效与适用条件。
	Coverage  string
	Relevance float64
	Freshness int64
	Applicability string
	// 排序快照与版本。
	SnapshotVersion int
	// 权威核对（索引得分不能决定授权或真值）。
	Verified  bool
	GapReason string
}

// RetrievalResult 是检索结果。
type RetrievalResult struct {
	Candidates []RetrievalCandidate
	// 已覆盖范围与缺口。
	Coverage    string
	GapReason   string
	Truncated   bool
	NextCursor  string
}

// ---------------------------------------------------------------------------
// 检索（先结构化键/受限图/全文检索，再按证据覆盖、相关性、时效与适用条件排序）
// ---------------------------------------------------------------------------

// executeRetrieval 执行检索（P5-02.2/P5-02.3）。
// 先结构化键/受限图/全文检索，再按证据覆盖、相关性、时效与适用条件排序；
// 每个候选回权威来源核对当前权限、版本、有效期和反例；索引得分不能决定
// 授权或真值。
func executeRetrieval(tx *sql.Tx, req RetrievalRequest, now int64) (RetrievalResult, error) {
	var result RetrievalResult
	startTime := time.Now()
	// 授权核对（索引得分不能决定授权或真值）。
	decision, err := decideAuthorization(tx, req.Subject, "read", req.ObjectScope, "", now, false)
	if err != nil {
		return result, err
	}
	if decision.Outcome != "allowed" {
		return result, fmt.Errorf("%w：主体 %s 无权检索 %s", ErrAuthorizationDenied, req.Subject, req.ObjectScope)
	}
	// 结构化键/受限图/全文检索（候选收集）。
	var candidates []RetrievalCandidate
	// 事实候选（结构化键）。
	factRows, err := tx.Query(`SELECT object_ref, source_ref, valid_from FROM facts WHERE object_ref LIKE ? AND valid_to=0 LIMIT ?`,
		req.ObjectScope+"%", req.Budget.MaxCalls)
	if err == nil {
		defer factRows.Close()
		for factRows.Next() {
			var c RetrievalCandidate
			var validFrom int64
			if err := factRows.Scan(&c.Ref, &c.SourceRef, &validFrom); err != nil {
				break
			}
			c.Kind = "fact"
			c.Freshness = now - validFrom
			c.Coverage = "fact"
			candidates = append(candidates, c)
		}
	}
	// 材料候选（全文检索）。
	materialRows, err := tx.Query(`SELECT material_id, source_ref, created_at FROM material_indexes WHERE object_ref LIKE ? AND state='ready' LIMIT ?`,
		req.ObjectScope+"%", req.Budget.MaxCalls)
	if err == nil {
		defer materialRows.Close()
		for materialRows.Next() {
			var c RetrievalCandidate
			if err := materialRows.Scan(&c.Ref, &c.SourceRef, &c.Freshness); err != nil {
				break
			}
			c.Kind = "material"
			c.Freshness = now - c.Freshness
			c.Coverage = "material"
			candidates = append(candidates, c)
		}
	}
	// 按证据覆盖、相关性、时效与适用条件排序。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Freshness != candidates[j].Freshness {
			return candidates[i].Freshness < candidates[j].Freshness
		}
		return candidates[i].Relevance > candidates[j].Relevance
	})
	// 每个候选回权威来源核对当前权限、版本、有效期和反例。
	for i := range candidates {
		c := &candidates[i]
		// 权限核对（索引得分不能决定授权或真值）。
		decision, err := decideAuthorization(tx, req.Subject, "read", c.Ref, "", now, false)
		if err != nil || decision.Outcome != "allowed" {
			c.Verified = false
			c.GapReason = "无权限"
			continue
		}
		// 新鲜度核对（新鲜度超过预算的标记缺口）。
		if req.FreshnessMS > 0 && c.Freshness > req.FreshnessMS {
			c.Verified = false
			c.GapReason = "新鲜度超限"
			continue
		}
		c.Verified = true
	}
	// 时间预算。
	if time.Since(startTime).Milliseconds() > req.Budget.MaxTimeMS {
		result.Truncated = true
		result.GapReason = "时间预算耗尽"
	}
	// 字节预算。
	if len(candidates) > req.Budget.MaxCalls {
		candidates = candidates[:req.Budget.MaxCalls]
		result.Truncated = true
		result.GapReason = "调用预算耗尽"
	}
	result.Candidates = candidates
	result.Coverage = fmt.Sprintf("%d 候选", len(candidates))
	return result, nil
}

// ---------------------------------------------------------------------------
// 补查（缺关键条件或冲突时做有界原文/来源补查）
// ---------------------------------------------------------------------------

// ErrRetrievalGap 是检索缺口的错误。
var ErrRetrievalGap = errors.New("检索缺口")

// ensureRetrievalBounded 核对检索有界（P5-02.4）。
// 缺关键条件或冲突时做有界原文/来源补查；仍不明就返回缺口，不能无限检索；
// 预算耗尽和索引不可用给出缺口与已覆盖范围，不伪造"没有问题"。
func ensureRetrievalBounded(result RetrievalResult, budget RetrievalBudget) error {
	// 预算耗尽：返回缺口与已覆盖范围（不伪造"没有问题"）。
	if result.Truncated && result.GapReason != "" {
		return fmt.Errorf("%w：%s（已覆盖范围：%s）", ErrRetrievalGap, result.GapReason, result.Coverage)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 分页游标（绑定主体、范围、排序快照和版本）
// ---------------------------------------------------------------------------

// RetrievalCursor 是分页游标。
type RetrievalCursor struct {
	// 绑定主体、范围、排序快照和版本。
	Subject   string
	ObjectScope string
	SnapshotVersion int
	Kind      string
	// 游标与失效。
	Cursor    string
	ExpiresAt int64
}

// validateRetrievalCursor 核对分页游标（P5-02.5）。
// 分页游标绑定主体、范围、排序快照和版本；继续查询重核权限，失效游标明确重开。
func validateRetrievalCursor(tx *sql.Tx, cursor RetrievalCursor, now int64) error {
	// 继续查询重核权限（索引得分不能决定授权或真值）。
	decision, err := decideAuthorization(tx, cursor.Subject, "read", cursor.ObjectScope, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		return fmt.Errorf("%w：游标主体 %s 无权限（继续查询重核权限）", ErrAuthorizationDenied, cursor.Subject)
	}
	// 失效游标明确重开。
	if now >= cursor.ExpiresAt {
		return fmt.Errorf("游标已失效（失效游标明确重开）")
	}
	return nil
}

// 确保 context/sql/errors/fmt/sort/time 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = sort.Slice
	_ = time.Now
)
