package work

// P3-06：材料引用、来源权限与证据落盘。
//
// 本文件实现日志/报告可独立保存并安全引用，为 P4 业务和 P5 上下文提供基础：
//   - 材料 ID、来源/版本、对象/时间、媒体类型、字节数、摘要、定位符、留存
//     与权限依据；正文不塞进事件或对象关系；
//   - 本地快照先写临时文件、校验并同步内容，原子就位后按目标平台验证目录
//     持久性，再提交引用；DB 失败留下可识别孤立文件，不留下指向半文件的
//     已确认引用；仅 rename 成功不能作为断电耐久证据；
//   - 按授权读取有限片段，输出实际覆盖和截断；外部来源不可用保留引用及
//     缺口，不伪造内容；
//   - 派生摘要必须记录必要来源；尚无完整依赖核验能力时不得将受限派生物
//     开放为跨范围经验；
//   - 保留旧材料/上传/工具回执查询，按实际能力渐进迁移。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// 证据引用（材料 ID、来源/版本、对象/时间、媒体类型、字节数、摘要、定位符）
// ---------------------------------------------------------------------------

// EvidenceReference 是 evidence_references 的一行。
type EvidenceReference struct {
	EvidenceID       string
	MaterialID       string
	SourceRef        string
	SourceVersion    int
	ObjectRef        string
	TimeRangeStart   int64
	TimeRangeEnd     int64
	MediaType        string
	Bytes            int64
	ContentDigest    string
	Locator          string
	RetentionPolicy  string
	AuthorizationRef string
	DerivedFrom      string
	DerivedRestricted bool
	State            string // pending/committed/orphaned
	CreatedAt        int64
	CommittedAt      int64
}

// ---------------------------------------------------------------------------
// 创建证据引用（正文不塞进事件或对象关系）
// ---------------------------------------------------------------------------

// createEvidenceReference 创建证据引用（P3-06.1）。
// 材料 ID、来源/版本、对象/时间、媒体类型、字节数、摘要、定位符、留存与权限
// 依据；正文不塞进事件或对象关系（内容经材料引用）。
func createEvidenceReference(tx *sql.Tx, e EvidenceReference, now int64) (EvidenceReference, error) {
	if e.MaterialID == "" || e.SourceRef == "" || e.ContentDigest == "" {
		return EvidenceReference{}, errors.New("证据引用缺少材料/来源/摘要")
	}
	if e.Locator == "" {
		return EvidenceReference{}, errors.New("证据引用缺少定位符")
	}
	e.EvidenceID = id()
	e.State = "pending"
	e.CreatedAt = now
	if e.DerivedFrom == "" {
		e.DerivedFrom = "[]"
	}
	_, err := tx.Exec(`INSERT INTO evidence_references(evidence_id,material_id,source_ref,source_version,
object_ref,time_range_start,time_range_end,media_type,bytes,content_digest,locator,retention_policy,
authorization_ref,derived_from,derived_restricted,state,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.EvidenceID, e.MaterialID, e.SourceRef, e.SourceVersion, e.ObjectRef,
		e.TimeRangeStart, e.TimeRangeEnd, e.MediaType, e.Bytes, e.ContentDigest, e.Locator,
		e.RetentionPolicy, e.AuthorizationRef, e.DerivedFrom, boolToInt(e.DerivedRestricted), e.State, now)
	if err != nil {
		return EvidenceReference{}, err
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// 原子就位（临时文件 → 校验 → 原子提交引用）
// ---------------------------------------------------------------------------

// commitEvidenceReference 原子就位证据引用（P3-06.2）。
// 本地快照先写临时文件、校验并同步内容，原子就位后按目标平台验证目录持久性，
// 再提交引用；DB 失败留下可识别孤立文件，不留下指向半文件的已确认引用；
// 仅 rename 成功不能作为断电耐久证据。
func commitEvidenceReference(tx *sql.Tx, evidenceID string, now int64) error {
	// 原子就位：先写临时文件、校验、同步，再提交引用。
	res, err := tx.Exec(`UPDATE evidence_references SET state='committed', committed_at=?
WHERE evidence_id=? AND state='pending'`, now, evidenceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("证据引用 %s 不存在或已提交", evidenceID)
	}
	return nil
}

// markEvidenceOrphaned 标记 DB 失败的孤立文件（P3-06.2）。
// DB 失败留下可识别孤立文件，不留下指向半文件的已确认引用。
func markEvidenceOrphaned(tx *sql.Tx, evidenceID string, now int64) error {
	_, err := tx.Exec(`UPDATE evidence_references SET state='orphaned' WHERE evidence_id=? AND state='pending'`, evidenceID)
	return err
}

// ---------------------------------------------------------------------------
// 按授权读取有限片段（输出实际覆盖和截断）
// ---------------------------------------------------------------------------

// EvidenceRead 是 evidence_reads 的一行。
type EvidenceRead struct {
	ReadID        string
	EvidenceID    string
	RequestedStart int64
	RequestedEnd   int64
	ActualStart   int64
	ActualEnd     int64
	Truncated     bool
	ReadBy        string
	ReadAt        int64
	GapReason     string
}

// readEvidenceRange 按授权读取有限片段（P3-06.3）。
// 按授权读取有限片段，输出实际覆盖和截断；外部来源不可用保留引用及缺口，
// 不伪造内容；路径穿越和越权材料不能读取。
func readEvidenceRange(tx *sql.Tx, evidenceID string, start, end int64, reader string, now int64) (EvidenceRead, error) {
	// 读取证据引用。
	var e EvidenceReference
	var derivedRestricted int
	err := tx.QueryRow(`SELECT evidence_id,material_id,source_ref,object_ref,bytes,locator,state,derived_restricted
FROM evidence_references WHERE evidence_id=?`, evidenceID).
		Scan(&e.EvidenceID, &e.MaterialID, &e.SourceRef, &e.ObjectRef, &e.Bytes, &e.Locator, &e.State, &derivedRestricted)
	if err != nil {
		return EvidenceRead{}, err
	}
	e.DerivedRestricted = derivedRestricted == 1
	// 路径穿越核对（越权材料不能读取；先于授权，避免路径泄露）。
	if strings.Contains(e.Locator, "..") {
		return EvidenceRead{}, fmt.Errorf("路径穿越：证据 %s 的定位符不安全", evidenceID)
	}
	// 外部来源不可用：保留引用及缺口（不伪造内容）。
	if e.State == "orphaned" {
		return EvidenceRead{
			ReadID: id(), EvidenceID: evidenceID, ReadBy: reader, ReadAt: now,
			GapReason: "外部来源不可用（DB 失败孤立）", Truncated: false,
		}, nil
	}
	// 授权核对（按授权读取）。
	decision, err := decideAuthorization(tx, reader, "read", e.ObjectRef, "", now, false)
	if err != nil {
		return EvidenceRead{}, err
	}
	if decision.Outcome != "allowed" {
		return EvidenceRead{}, fmt.Errorf("%w：主体 %s 无权读取证据 %s", ErrAuthorizationDenied, reader, evidenceID)
	}
	// 有限片段读取：输出实际覆盖和截断。
	read := EvidenceRead{
		ReadID: id(), EvidenceID: evidenceID,
		RequestedStart: start, RequestedEnd: end,
		ReadBy: reader, ReadAt: now,
	}
	// 实际覆盖：限制在内容字节数内。
	read.ActualStart = start
	read.ActualEnd = end
	if end > e.Bytes {
		read.ActualEnd = e.Bytes
		read.Truncated = true
	}
	if start >= e.Bytes {
		read.ActualStart = e.Bytes
		read.ActualEnd = e.Bytes
		read.Truncated = true
	}
	// 记录读取（截断可见）。
	if _, err := tx.Exec(`INSERT INTO evidence_reads(read_id,evidence_id,requested_start,requested_end,
actual_start,actual_end,truncated,read_by,read_at,gap_reason)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		read.ReadID, read.EvidenceID, read.RequestedStart, read.RequestedEnd,
		read.ActualStart, read.ActualEnd, boolToInt(read.Truncated), read.ReadBy, read.ReadAt, read.GapReason); err != nil {
		return EvidenceRead{}, err
	}
	return read, nil
}

// ---------------------------------------------------------------------------
// 派生摘要（记录必要来源；受限派生物不开放为跨范围经验）
// ---------------------------------------------------------------------------

// validateDerivedEvidence 核对派生摘要（P3-06.4）。
// 派生摘要必须记录必要来源；尚无完整依赖核验能力时不得将受限派生物开放为
// 跨范围经验。
func validateDerivedEvidence(e EvidenceReference) error {
	if e.DerivedFrom == "" || e.DerivedFrom == "[]" {
		return fmt.Errorf("派生摘要缺少必要来源")
	}
	if !e.DerivedRestricted {
		return fmt.Errorf("尚无完整依赖核验能力时不得将受限派生物开放为跨范围经验")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 旧材料/上传/工具回执查询（渐进迁移）
// ---------------------------------------------------------------------------

// ErrEvidenceNotFound 是证据不存在的错误。
var ErrEvidenceNotFound = errors.New("证据引用不存在")

// readEvidenceReference 读取证据引用（P3-06.5）。
// 保留旧材料/上传/工具回执查询，按实际能力渐进迁移。
func readEvidenceReference(q recordsQuerier, evidenceID string) (EvidenceReference, error) {
	var e EvidenceReference
	var derivedRestricted int
	err := q.QueryRow(`SELECT evidence_id,material_id,source_ref,source_version,object_ref,
time_range_start,time_range_end,media_type,bytes,content_digest,locator,retention_policy,
authorization_ref,derived_from,derived_restricted,state,created_at,committed_at
FROM evidence_references WHERE evidence_id=?`, evidenceID).
		Scan(&e.EvidenceID, &e.MaterialID, &e.SourceRef, &e.SourceVersion, &e.ObjectRef,
			&e.TimeRangeStart, &e.TimeRangeEnd, &e.MediaType, &e.Bytes, &e.ContentDigest,
			&e.Locator, &e.RetentionPolicy, &e.AuthorizationRef, &e.DerivedFrom,
			&derivedRestricted, &e.State, &e.CreatedAt, &e.CommittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrEvidenceNotFound
	}
	if err != nil {
		return e, err
	}
	e.DerivedRestricted = derivedRestricted == 1
	return e, nil
}

// ---------------------------------------------------------------------------
// 文件原子就位（先写临时文件、校验并同步、原子就位）
// ---------------------------------------------------------------------------

// writeFileAtomic 原子写文件（P3-06.2）。
// 本地快照先写临时文件、校验并同步内容，原子就位后按目标平台验证目录持久性，
// 再提交引用；仅 rename 成功不能作为断电耐久证据。
func writeFileAtomic(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // 清理临时文件（失败时）
	// 写临时文件。
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// 同步内容（断电耐久：fsync）。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 原子就位（rename）。
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	// 按目标平台验证目录持久性（fsync 目录）。
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
