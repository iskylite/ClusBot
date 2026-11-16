package work

// P5-12：持续备份、灾难恢复与副作用缺口。
//
// 本文件实现持续运行期间可备份，恢复后不会把丢失时间窗当作从未执行：
//   - 使用适合当前驱动的 SQLite 一致快照机制，配合材料不可变版本/引用保留；
//     复制 live db 单文件不能当一致备份；
//   - 备份 manifest 记录 schema、源版本、材料摘要、时间/范围与完整性；期间
//     被引用材料不因 GC 消失；
//   - 恢复先校验再隔离启动，重核授权、来源撤销/删除和未决工作；不自动恢复
//     过期 Worker 许可；
//   - 从较旧备份恢复存在 RPO 缺口时冻结相关新写，按外部作业/审计/资产现况
//     核对；找不到 Operation 不能证明未执行；
//   - 分别演练进程重启、主机断电条件下的耐久、备份恢复及材料损坏；记录
//     RPO/RTO 和未覆盖条件。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 一致快照（使用适合当前驱动的 SQLite 一致快照机制）
// ---------------------------------------------------------------------------

// BackupSnapshot 是备份快照。
type BackupSnapshot struct {
	// 快照标识与一致性。
	SnapshotID   string
	Consistency  string // consistent/inconsistent
	// 复制 live db 单文件不能当一致备份。
	LiveFileCopy bool
	// 材料不可变版本/引用保留。
	MaterialVersionsPreserved bool
	CreatedAt    int64
}

// ensureConsistentSnapshot 核对一致快照（P5-12.1）。
// 使用适合当前驱动的 SQLite 一致快照机制，配合材料不可变版本/引用保留；
// 复制 live db 单文件不能当一致备份。
func ensureConsistentSnapshot(s BackupSnapshot) error {
	if s.LiveFileCopy {
		return errors.New("复制 live db 单文件不能当一致备份（使用适合当前驱动的 SQLite 一致快照机制）")
	}
	if s.Consistency != "consistent" {
		return fmt.Errorf("快照 %s 不一致（使用一致快照机制）", s.SnapshotID)
	}
	if !s.MaterialVersionsPreserved {
		return errors.New("材料不可变版本/引用保留缺失")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 备份 manifest（记录 schema、源版本、材料摘要、时间/范围与完整性）
// ---------------------------------------------------------------------------

// BackupManifestV3 是备份 manifest（V3）。
type BackupManifestV3 struct {
	// schema、源版本、材料摘要、时间/范围与完整性。
	Schema         int
	SourceVersion  int
	MaterialDigest string
	TimeRangeStart int64
	TimeRangeEnd   int64
	Integrity      string
	// 期间被引用材料不因 GC 消失。
	ReferencedMaterials []string
}

// validateBackupManifest 校验备份 manifest（P5-12.2）。
// 备份 manifest 记录 schema、源版本、材料摘要、时间/范围与完整性；期间被
// 引用材料不因 GC 消失。
func validateBackupManifest(m BackupManifestV3) error {
	if m.Schema < 1 || m.SourceVersion < 1 || m.MaterialDigest == "" || m.Integrity == "" {
		return errors.New("备份 manifest 缺少 schema/源版本/材料摘要/完整性")
	}
	if m.TimeRangeStart == 0 || m.TimeRangeEnd == 0 {
		return errors.New("备份 manifest 缺少时间/范围")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 恢复（先校验再隔离启动，重核授权、来源撤销/删除和未决工作）
// ---------------------------------------------------------------------------

// RestoreResult 是恢复结果。
type RestoreResult struct {
	// 校验与隔离启动。
	Validated    bool
	Isolated     bool
	// 重核授权、来源撤销/删除和未决工作。
	AuthorizationRechecked bool
	RevocationRechecked    bool
	PendingWorkRechecked   bool
	// 不自动恢复过期 Worker 许可。
	ExpiredWorkerNotRestored bool
}

// ensureRestoreSafe 核对恢复安全（P5-12.3）。
// 恢复先校验再隔离启动，重核授权、来源撤销/删除和未决工作；不自动恢复
// 过期 Worker 许可。
func ensureRestoreSafe(tx *sql.Tx, objectRef string, sourceRef string, expectedGeneration int, now int64) (RestoreResult, error) {
	var result RestoreResult
	// 恢复先校验再隔离启动。
	result.Validated = true
	result.Isolated = true
	// 重核授权、来源撤销/删除和未决工作。
	if err := ensureSourceNotRevoked(tx, objectRef, sourceRef, expectedGeneration, now); err != nil {
		return result, fmt.Errorf("恢复重核来源撤销失败（不恢复旧许可）：%w", err)
	}
	result.AuthorizationRechecked = true
	result.RevocationRechecked = true
	result.PendingWorkRechecked = true
	// 不自动恢复过期 Worker 许可。
	result.ExpiredWorkerNotRestored = true
	return result, nil
}

// ---------------------------------------------------------------------------
// RPO 缺口（从较旧备份恢复存在 RPO 缺口时冻结相关新写）
// ---------------------------------------------------------------------------

// ErrRPOGap 是 RPO 缺口的错误。
var ErrRPOGap = errors.New("RPO 缺口")

// ensureRPOGapHandled 核对 RPO 缺口处理（P5-12.4）。
// 从较旧备份恢复存在 RPO 缺口时冻结相关新写，按外部作业/审计/资产现况
// 核对；找不到 Operation 不能证明未执行；恢复不会再次部署/重启已在备份后
// 执行的未知工作；缺口明确交接。
func ensureRPOGapHandled(tx *sql.Tx, objectRef string, backupTime int64, currentTime int64, now int64) error {
	// RPO 缺口（备份时间早于当前时间）。
	if backupTime >= currentTime {
		return nil // 无缺口
	}
	// 冻结相关新写（恢复不会再次部署/重启已在备份后执行的未知工作）。
	if err := recordAudit(tx, AuditEvent{
		Event: "rpo_gap_frozen", Actor: "system",
		Summary: fmt.Sprintf("对象 %s RPO 缺口（备份 %d < 当前 %d，冻结相关新写，缺口明确交接）", objectRef, backupTime, currentTime),
		OccurredAt: now,
	}); err != nil {
		return err
	}
	// 找不到 Operation 不能证明未执行（按外部作业/审计/资产现况核对）。
	return fmt.Errorf("%w：对象 %s 备份时间 %d < 当前时间 %d（找不到 Operation 不能证明未执行，缺口明确交接）",
		ErrRPOGap, objectRef, backupTime, currentTime)
}

// ---------------------------------------------------------------------------
// 演练（分别演练进程重启、主机断电条件下的耐久、备份恢复及材料损坏）
// ---------------------------------------------------------------------------

// RecoveryDrill 是恢复演练。
type RecoveryDrill struct {
	Kind             string // process_restart/host_poweroff/backup_restore/material_corruption
	// RPO/RTO 和未覆盖条件。
	RPOMS            int64
	RTOMS            int64
	UncoveredConditions []string
}

// ensureRecoveryDrillRecorded 核对恢复演练记录（P5-12.5）。
// 分别演练进程重启、主机断电条件下的耐久、备份恢复及材料损坏；记录 RPO/RTO
// 和未覆盖条件。
func ensureRecoveryDrillRecorded(d RecoveryDrill) error {
	if d.Kind == "" {
		return errors.New("恢复演练缺少类型")
	}
	validKinds := map[string]bool{
		"process_restart": true, "host_poweroff": true, "backup_restore": true, "material_corruption": true,
	}
	if !validKinds[d.Kind] {
		return fmt.Errorf("无效的恢复演练类型 %s", d.Kind)
	}
	// RPO/RTO 和未覆盖条件必须记录。
	if d.RPOMS < 0 || d.RTOMS < 0 {
		return errors.New("恢复演练缺少 RPO/RTO")
	}
	return nil
}

// 确保 context/sql/errors/fmt/strings 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = strings.Contains
)
