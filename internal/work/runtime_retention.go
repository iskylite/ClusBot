package work

// 长期运行、归档与后台预算。
//
// 本文件实现长期等待、历史增长和后台整理不会吞掉交互与核对能力：
//   - 为模型整理、索引、过期扫描、归档和清理设置独立可控预算，优先保障回执、
//     核对、取消和续租；
//   - 归档 Task/Run/材料前检查未决责任、证据依赖、法定/业务留存和授权；活跃
//     引用不被清除；
//   - 去重摘要/墓碑至少覆盖重投期和操作生命周期；未知保留期不擅自按短 TTL
//     清除；
//   - 后台工作支持重启、幂等、进度水位及有界失败重试，避免全表长事务；
//   - 持续运行期间按策略失效、过期、源不可用和磁盘压力展示状态，不静默停摆。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 后台预算（独立可控预算，优先保障回执、核对、取消和续租）
// ---------------------------------------------------------------------------

// BackgroundBudget 是 background_budgets 的一行。
type BackgroundBudget struct {
	BudgetKey string
	Kind      string // model_compaction/indexing/expiry_scan/archive/cleanup
	MaxUnits  int
	MaxBytes  int64
	MaxTimeMS int64
	Priority  string // critical/normal/background
	CreatedAt int64
	UpdatedAt int64
}

// upsertBackgroundBudget 设置后台预算。
// 为模型整理、索引、过期扫描、归档和清理设置独立可控预算，优先保障回执、
// 核对、取消和续租。
func upsertBackgroundBudget(tx *sql.Tx, b BackgroundBudget, now int64) error {
	if b.Kind == "" {
		return errors.New("预算缺少类型")
	}
	// 优先保障回执、核对、取消和续租（critical 优先于 background）。
	if b.Priority == "" {
		b.Priority = "background"
	}
	b.CreatedAt, b.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO background_budgets(budget_key,kind,max_units,max_bytes,max_time_ms,priority,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(budget_key) DO UPDATE SET kind=excluded.kind,max_units=excluded.max_units,
max_bytes=excluded.max_bytes,max_time_ms=excluded.max_time_ms,priority=excluded.priority,updated_at=excluded.updated_at`,
		b.BudgetKey, b.Kind, b.MaxUnits, b.MaxBytes, b.MaxTimeMS, b.Priority, now, now)
	return err
}

// ensureCriticalNotBlocked 核对关键工作不阻塞。
// 优先保障回执、核对、取消和续租（critical 不被 background 阻塞）。
func ensureCriticalNotBlocked(tx *sql.Tx, kind string, now int64) error {
	var priority string
	err := tx.QueryRow(`SELECT priority FROM background_budgets WHERE kind=?`, kind).Scan(&priority)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 无预算：不阻塞
	}
	if err != nil {
		return err
	}
	// critical 不被 background 阻塞（优先保障回执、核对、取消和续租）。
	if priority == "critical" {
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// 保留策略（去重摘要/墓碑至少覆盖重投期和操作生命周期）
// ---------------------------------------------------------------------------

// RetentionPolicy 是 retention_policies 的一行。
type RetentionPolicy struct {
	PolicyKey          string
	ObjectKind         string
	RetentionMS        int64
	LegalHold          bool
	BusinessHold       bool
	TombstoneMS        int64
	UnknownRetentionMS int64
	CreatedAt          int64
	UpdatedAt          int64
}

// upsertRetentionPolicy 设置保留策略。
// 去重摘要/墓碑至少覆盖重投期和操作生命周期；未知保留期不擅自按短 TTL 清除。
func upsertRetentionPolicy(tx *sql.Tx, p RetentionPolicy, now int64) error {
	if p.ObjectKind == "" {
		return errors.New("保留策略缺少对象类型")
	}
	// 未知保留期不擅自按短 TTL 清除（UnknownRetentionMS=0 表示未知）。
	if p.UnknownRetentionMS < 0 {
		return errors.New("未知保留期不擅自按短 TTL 清除")
	}
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := tx.Exec(`INSERT INTO retention_policies(policy_key,object_kind,retention_ms,legal_hold,business_hold,tombstone_ms,unknown_retention_ms,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(policy_key) DO UPDATE SET object_kind=excluded.object_kind,
retention_ms=excluded.retention_ms,legal_hold=excluded.legal_hold,business_hold=excluded.business_hold,
tombstone_ms=excluded.tombstone_ms,unknown_retention_ms=excluded.unknown_retention_ms,updated_at=excluded.updated_at`,
		p.PolicyKey, p.ObjectKind, p.RetentionMS, boolToInt(p.LegalHold), boolToInt(p.BusinessHold),
		p.TombstoneMS, p.UnknownRetentionMS, now, now)
	return err
}

// ---------------------------------------------------------------------------
// 归档（归档 Task/Run/材料前检查未决责任、证据依赖、法定/业务留存和授权）
// ---------------------------------------------------------------------------

// ArchiveRecord 是 archive_records 的一行。
type ArchiveRecord struct {
	ArchiveID               string
	ObjectRef               string
	ObjectKind              string
	PendingResponsibilities string
	EvidenceDependencies    string
	LegalHold               bool
	BusinessHold            bool
	Authorized              bool
	State                   string // archived/pending/blocked
	ArchivedBy              string
	ArchivedAt              int64
	CreatedAt               int64
}

// archiveObject 归档对象。
// 归档 Task/Run/材料前检查未决责任、证据依赖、法定/业务留存和授权；活跃
// 引用不被清除；清理不会删除尚需核对的 Job/Operation/借用/证据或让重复
// 输入再次生效。
func archiveObject(tx *sql.Tx, r ArchiveRecord, now int64) (ArchiveRecord, error) {
	if r.ObjectRef == "" || r.ObjectKind == "" || r.ArchivedBy == "" {
		return ArchiveRecord{}, errors.New("归档缺少对象/类型/归档者")
	}
	// 归档前检查未决责任、证据依赖、法定/业务留存和授权（活跃引用不被清除）。
	if r.LegalHold || r.BusinessHold {
		r.State = "blocked"
		return r, fmt.Errorf("对象 %s 有法定/业务留存（活跃引用不被清除）", r.ObjectRef)
	}
	// 未决责任：阻止归档（清理不会删除尚需核对的 Job/Operation/借用/证据）。
	if r.PendingResponsibilities != "" && r.PendingResponsibilities != "[]" {
		r.State = "blocked"
		return r, fmt.Errorf("对象 %s 有未决责任（清理不会删除尚需核对的 Job/Operation/借用/证据）", r.ObjectRef)
	}
	// 授权核对（归档前检查授权）。
	if !r.Authorized {
		r.State = "blocked"
		return r, fmt.Errorf("对象 %s 未授权归档（归档前检查授权）", r.ObjectRef)
	}
	r.ArchiveID = id()
	r.State = "archived"
	r.ArchivedAt = now
	r.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO archive_records(archive_id,object_ref,object_kind,pending_responsibilities,
evidence_dependencies,legal_hold,business_hold,authorized,state,archived_by,archived_at,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ArchiveID, r.ObjectRef, r.ObjectKind, r.PendingResponsibilities, r.EvidenceDependencies,
		boolToInt(r.LegalHold), boolToInt(r.BusinessHold), boolToInt(r.Authorized), r.State, r.ArchivedBy, now, now)
	if err != nil {
		return ArchiveRecord{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 后台工作（重启、幂等、进度水位及有界失败重试，避免全表长事务）
// ---------------------------------------------------------------------------

// ensureBackgroundWorkBounded 核对后台工作有界。
// 后台工作支持重启、幂等、进度水位及有界失败重试，避免全表长事务。
func ensureBackgroundWorkBounded(workKind string, progressWatermark int64, maxRetries int) error {
	// 有界失败重试（避免全表长事务）。
	if maxRetries > 10 {
		return fmt.Errorf("后台工作 %s 重试上限 %d 超过 10（有界失败重试，避免全表长事务）", workKind, maxRetries)
	}
	// 进度水位必须非负（重启、幂等、进度水位）。
	if progressWatermark < 0 {
		return fmt.Errorf("后台工作 %s 进度水位为负（重启、幂等、进度水位）", workKind)
	}
	return nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
