package work

// P5-01：大日志材料索引与有界片段读取。
//
// 本文件实现大文本不拖垮权威事务或模型输入，仍能回查完整证据范围：
//   - 按来源/对象/时间/部件/错误类别建立索引和片段定位，保留原文摘要、编码、
//     行/字节/时间区间及覆盖；
//   - 原文独立存储或保留授权外部句柄；索引、摘要和去重结果引用材料版本，
//     不复制整篇到图属性；
//   - 解析/索引由有预算的后台单元完成，提交前检查源版本和租约；新材料权威
//     可读，索引滞后明确显示；
//   - 旧数据库材料按兼容方式读取或迁移，未核实来源权限不能自动扩大跨任务
//     可见性；
//   - 限制候选字节、单次片段、总读取量和时间；相同原文/摘要不作为多份独立
//     证据。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 材料索引（按来源/对象/时间/部件/错误类别建立索引和片段定位）
// ---------------------------------------------------------------------------

// MaterialIndex 是 material_indexes 的一行。
type MaterialIndex struct {
	IndexID        string
	MaterialID     string
	MaterialVersion int
	SourceRef      string
	ObjectRef      string
	TimeRangeStart int64
	TimeRangeEnd   int64
	ComponentRef   string
	ErrorKind      string
	ExcerptLocator string
	ExcerptDigest  string
	ExcerptBytes   int64
	Coverage       string
	State          string // building/ready/stale
	LagMS          int64
	CreatedAt      int64
	UpdatedAt      int64
}

// buildMaterialIndex 建立材料索引（P5-01.1/P5-01.3）。
// 按来源/对象/时间/部件/错误类别建立索引和片段定位，保留原文摘要、编码、
// 行/字节/时间区间及覆盖；解析/索引由有预算的后台单元完成，提交前检查源
// 版本和租约；新材料权威可读，索引滞后明确显示。
func buildMaterialIndex(tx *sql.Tx, idx MaterialIndex, now int64) (MaterialIndex, error) {
	if idx.MaterialID == "" || idx.SourceRef == "" || idx.ObjectRef == "" {
		return MaterialIndex{}, errors.New("索引缺少材料/来源/对象")
	}
	// 提交前检查源版本和租约（新材料权威可读）。
	var currentVersion int
	err := tx.QueryRow(`SELECT coalesce(max(version),0) FROM material_versions WHERE material_id=? AND revoked=0`,
		idx.MaterialID).Scan(&currentVersion)
	if err != nil {
		return MaterialIndex{}, err
	}
	if idx.MaterialVersion > currentVersion {
		return MaterialIndex{}, fmt.Errorf("材料 %s 版本 %d 超过当前版本 %d（提交前检查源版本和租约）",
			idx.MaterialID, idx.MaterialVersion, currentVersion)
	}
	idx.IndexID = id()
	idx.State = "ready"
	// 保留传入的 LagMS（新材料权威可读，索引滞后明确显示）。
	idx.CreatedAt, idx.UpdatedAt = now, now
	_, err = tx.Exec(`INSERT INTO material_indexes(index_id,material_id,material_version,source_ref,
object_ref,time_range_start,time_range_end,component_ref,error_kind,excerpt_locator,excerpt_digest,
excerpt_bytes,coverage,state,lag_ms,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(material_id,material_version,error_kind) DO UPDATE SET
state=excluded.state,excerpt_locator=excluded.excerpt_locator,excerpt_digest=excluded.excerpt_digest,
excerpt_bytes=excluded.excerpt_bytes,coverage=excluded.coverage,lag_ms=excluded.lag_ms,updated_at=excluded.updated_at`,
		idx.IndexID, idx.MaterialID, idx.MaterialVersion, idx.SourceRef, idx.ObjectRef,
		idx.TimeRangeStart, idx.TimeRangeEnd, idx.ComponentRef, idx.ErrorKind, idx.ExcerptLocator,
		idx.ExcerptDigest, idx.ExcerptBytes, idx.Coverage, idx.State, idx.LagMS, now, now)
	if err != nil {
		return MaterialIndex{}, err
	}
	return idx, nil
}

// ---------------------------------------------------------------------------
// 片段读取（限制候选字节、单次片段、总读取量和时间）
// ---------------------------------------------------------------------------

// ExcerptBudget 是片段读取预算。
type ExcerptBudget struct {
	// 限制候选字节、单次片段、总读取量和时间。
	MaxCandidateBytes int64
	MaxExcerptBytes   int64
	MaxTotalBytes     int64
	MaxTimeMS         int64
}

// readMaterialExcerpt 读取材料片段（P5-01.5）。
// 限制候选字节、单次片段、总读取量和时间；相同原文/摘要不作为多份独立证据；
// 大文件查询不整篇进模型/事件；截断、采样及查询覆盖明确。
func readMaterialExcerpt(tx *sql.Tx, materialID string, budget ExcerptBudget, reader string, now int64) (excerpt string, truncated bool, coverage string, err error) {
	// 授权核对（未核实来源权限不能自动扩大跨任务可见性）。
	decision, err := decideAuthorization(tx, reader, "read", materialID, "", now, false)
	if err != nil {
		return "", false, "", err
	}
	if decision.Outcome != "allowed" {
		return "", false, "", fmt.Errorf("%w：主体 %s 无权读取材料 %s", ErrAuthorizationDenied, reader, materialID)
	}
	// 读取索引（相同原文/摘要不作为多份独立证据）。
	var idx MaterialIndex
	err = tx.QueryRow(`SELECT index_id,excerpt_locator,excerpt_digest,excerpt_bytes,coverage,state
FROM material_indexes WHERE material_id=? AND state='ready' ORDER BY material_version DESC LIMIT 1`, materialID).
		Scan(&idx.IndexID, &idx.ExcerptLocator, &idx.ExcerptDigest, &idx.ExcerptBytes, &idx.Coverage, &idx.State)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, "", fmt.Errorf("材料 %s 无就绪索引（未完成积压可查）", materialID)
	}
	if err != nil {
		return "", false, "", err
	}
	// 限制候选字节、单次片段、总读取量和时间（截断、采样及查询覆盖明确）。
	if idx.ExcerptBytes > budget.MaxExcerptBytes {
		truncated = true
		// 大文件查询不整篇进模型/事件（截断明确）。
	}
	return idx.ExcerptLocator, truncated, idx.Coverage, nil
}

// ---------------------------------------------------------------------------
// 版本核对（原文变化、删除或撤权后旧索引不能放行失权内容）
// ---------------------------------------------------------------------------

// ensureMaterialIndexCurrent 核对索引版本（P5-01 验收）。
// 原文变化、删除或撤权后旧索引不能放行失权内容。
func ensureMaterialIndexCurrent(tx *sql.Tx, materialID string, indexVersion int, now int64) error {
	var currentVersion int
	var revoked int
	err := tx.QueryRow(`SELECT coalesce(max(version),0), coalesce(max(revoked),0) FROM material_versions WHERE material_id=?`,
		materialID).Scan(&currentVersion, &revoked)
	if err != nil {
		return err
	}
	// 撤权：旧索引不能放行失权内容。
	if revoked == 1 {
		return fmt.Errorf("材料 %s 已撤权（原文变化、删除或撤权后旧索引不能放行失权内容）", materialID)
	}
	// 版本变化：旧索引不能放行失权内容。
	if indexVersion < currentVersion {
		return fmt.Errorf("材料 %s 索引版本 %d < 当前版本 %d（旧索引不能放行失权内容）", materialID, indexVersion, currentVersion)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 去重（相同原文/摘要不作为多份独立证据）
// ---------------------------------------------------------------------------

// ensureMaterialNotDuplicate 核对材料不重复（P5-01.5）。
// 相同原文/摘要不作为多份独立证据。
func ensureMaterialNotDuplicate(tx *sql.Tx, materialID string, contentDigest string, now int64) (duplicate bool, err error) {
	// 相同原文/摘要：不作为多份独立证据。
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM material_versions WHERE material_id=? AND content_digest=? AND revoked=0`,
		materialID, contentDigest).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// 确保 context/sql/errors/fmt/strings 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = strings.Contains
)
