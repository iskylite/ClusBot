package work

// P4-06：配置规范、验证证据与交付覆盖。
//
// 本文件实现业务验收逐要求、逐实际对象核对，可定位缺项与失效原因：
//   - 固定规范/规则版本、实际节点集合及对象组拓扑；参数测量定义与业务阈值
//     分离；
//   - 保存验证记录的输入条件、执行配置、样本范围、结果、来源和适用版本；
//     不能仅存布尔通过；
//   - 按要求 × 对象/组建立覆盖表；不适用项须有规范依据，未测/未知不能默认
//     通过；
//   - 关系、部件、固件或规范变化时复查依赖，局部失效不抹掉无关结果；影响
//     不明保留待复核；
//   - 验收通过、准备交付和真实接收交接分别记录，最后才可决定在役。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 验证记录（输入条件、执行配置、样本范围、结果、来源和适用版本）
// ---------------------------------------------------------------------------

// ValidationRecord 是 validation_records 的一行。
type ValidationRecord struct {
	ValidationID       string
	SpecRef            string
	RuleVersion        int
	ObjectRefs         string
	ObjectGroupTopology string
	InputConditions    string
	ExecutionConfig    string
	SampleScope        string
	Result             string
	SourceRef          string
	ApplicableVersions string
	MeasurementDefinition string
	BusinessThreshold  string
	Outcome            string // passed/failed/unknown/pending
	ValidatedBy        string
	ValidatedAt        int64
	CreatedAt          int64
}

// recordValidation 记录验证（P4-06.2）。
// 保存验证记录的输入条件、执行配置、样本范围、结果、来源和适用版本；
// 不能仅存布尔通过；参数测量定义与业务阈值分离。
func recordValidation(tx *sql.Tx, v ValidationRecord, now int64) (ValidationRecord, error) {
	if v.SpecRef == "" || v.RuleVersion < 1 || v.ObjectRefs == "" || v.ValidatedBy == "" {
		return ValidationRecord{}, errors.New("验证记录缺少规范/规则版本/对象集合/验证者")
	}
	// 不能仅存布尔通过：必须有输入条件、执行配置、样本范围、结果、来源。
	if v.InputConditions == "" || v.ExecutionConfig == "" || v.SampleScope == "" || v.Result == "" || v.SourceRef == "" {
		return ValidationRecord{}, errors.New("验证记录不能仅存布尔通过（必须有输入条件/执行配置/样本范围/结果/来源）")
	}
	// 参数测量定义与业务阈值分离。
	if v.MeasurementDefinition == "" || v.BusinessThreshold == "" {
		return ValidationRecord{}, errors.New("验证记录缺少参数测量定义或业务阈值（分离）")
	}
	v.ValidationID = id()
	v.ValidatedAt = now
	v.CreatedAt = now
	if v.Outcome == "" {
		v.Outcome = "pending"
	}
	_, err := tx.Exec(`INSERT INTO validation_records(validation_id,spec_ref,rule_version,object_refs,
object_group_topology,input_conditions,execution_config,sample_scope,result,source_ref,applicable_versions,
measurement_definition,business_threshold,outcome,validated_by,validated_at,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.ValidationID, v.SpecRef, v.RuleVersion, v.ObjectRefs, v.ObjectGroupTopology,
		v.InputConditions, v.ExecutionConfig, v.SampleScope, v.Result, v.SourceRef, v.ApplicableVersions,
		v.MeasurementDefinition, v.BusinessThreshold, v.Outcome, v.ValidatedBy, now, now)
	if err != nil {
		return ValidationRecord{}, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 覆盖表（按要求 × 对象/组建立覆盖表）
// ---------------------------------------------------------------------------

// AcceptanceCoverage 是 acceptance_coverage 的一行。
type AcceptanceCoverage struct {
	CoverageID         string
	RequirementKey     string
	ObjectRef          string
	CoverageState      string // covered/not_applicable/uncovered/unknown
	NotApplicableBasis string
	ValidationRef      string
	CreatedAt          int64
	UpdatedAt          int64
}

// recordAcceptanceCoverage 记录验收覆盖（P4-06.3）。
// 按要求 × 对象/组建立覆盖表；不适用项须有规范依据，未测/未知不能默认通过。
func recordAcceptanceCoverage(tx *sql.Tx, c AcceptanceCoverage, now int64) (AcceptanceCoverage, error) {
	if c.RequirementKey == "" || c.ObjectRef == "" {
		return AcceptanceCoverage{}, errors.New("覆盖记录缺少要求或对象")
	}
	// 不适用项须有规范依据。
	if c.CoverageState == "not_applicable" && c.NotApplicableBasis == "" {
		return AcceptanceCoverage{}, errors.New("不适用项须有规范依据")
	}
	// 未测/未知不能默认通过（默认 unknown）。
	if c.CoverageState == "" {
		c.CoverageState = "unknown"
	}
	c.CoverageID = id()
	c.CreatedAt, c.UpdatedAt = now, now
	var validationRef any
	if c.ValidationRef != "" {
		validationRef = c.ValidationRef
	}
	_, err := tx.Exec(`INSERT INTO acceptance_coverage(coverage_id,requirement_key,object_ref,
coverage_state,not_applicable_basis,validation_ref,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(requirement_key,object_ref) DO UPDATE SET
coverage_state=excluded.coverage_state,not_applicable_basis=excluded.not_applicable_basis,
validation_ref=excluded.validation_ref,updated_at=excluded.updated_at`,
		c.CoverageID, c.RequirementKey, c.ObjectRef, c.CoverageState, c.NotApplicableBasis,
		validationRef, now, now)
	if err != nil {
		return AcceptanceCoverage{}, err
	}
	return c, nil
}

// readAcceptanceCoverage 读取一个要求的覆盖表（逐要求、逐实际对象核对）。
func readAcceptanceCoverage(q recordsQuerier, requirementKey string) ([]AcceptanceCoverage, error) {
	rows, err := q.Query(`SELECT coverage_id,requirement_key,object_ref,coverage_state,
not_applicable_basis,validation_ref,created_at,updated_at FROM acceptance_coverage WHERE requirement_key=?`, requirementKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AcceptanceCoverage
	for rows.Next() {
		var c AcceptanceCoverage
		var validationRef sql.NullString
		if err := rows.Scan(&c.CoverageID, &c.RequirementKey, &c.ObjectRef, &c.CoverageState,
			&c.NotApplicableBasis, &validationRef, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.ValidationRef = validationRef.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 证据依赖（关系、部件、固件或规范变化时复查依赖）
// ---------------------------------------------------------------------------

// EvidenceDependency 是 evidence_dependencies 的一行。
type EvidenceDependency struct {
	DependencyID     string
	DependencyKind   string
	TargetRef        string
	ValidationRef    string
	Invalidated      bool
	InvalidatedReason string
	CreatedAt        int64
	InvalidatedAt    int64
}

// invalidateEvidenceDependency 失效证据依赖（P4-06.4）。
// 关系、部件、固件或规范变化时复查依赖，局部失效不抹掉无关结果；影响不明
// 保留待复核。
func invalidateEvidenceDependency(tx *sql.Tx, dependencyKind, targetRef string, reason string, now int64) (invalidated int, err error) {
	// 失效依赖（局部失效：只失效依赖该目标的验证，不抹掉无关结果）。
	res, err := tx.Exec(`UPDATE evidence_dependencies SET invalidated=1, invalidated_reason=?, invalidated_at=?
WHERE dependency_kind=? AND target_ref=? AND invalidated=0`, reason, now, dependencyKind, targetRef)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// 失效的验证标记为待复核（影响不明保留待复核）。
	if _, err := tx.Exec(`UPDATE validation_records SET outcome='unknown'
WHERE validation_id IN (SELECT validation_ref FROM evidence_dependencies WHERE dependency_kind=? AND target_ref=? AND invalidated=1)`,
		dependencyKind, targetRef); err != nil {
		return int(n), err
	}
	// P2-5：证据失效联动验收覆盖（设计：证据仍适用才可交付）。
	// 依赖失效后，引用失效验证的验收覆盖降级为 unknown（不再视为 covered，
	// 避免证据已失效仍被误判为可交付）。局部失效不抹掉无关结果。
	if _, err := tx.Exec(`UPDATE acceptance_coverage SET coverage_state='unknown', updated_at=?
WHERE object_ref=? AND coverage_state='covered' AND validation_ref IN
(SELECT validation_ref FROM evidence_dependencies WHERE dependency_kind=? AND target_ref=? AND invalidated=1)`,
		now, targetRef, dependencyKind, targetRef); err != nil {
		return int(n), err
	}
	return int(n), nil
}

// recordEvidenceDependency 记录证据依赖。
func recordEvidenceDependency(tx *sql.Tx, d EvidenceDependency, now int64) error {
	d.DependencyID = id()
	d.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO evidence_dependencies(dependency_id,dependency_kind,target_ref,validation_ref,created_at)
VALUES(?,?,?,?,?)`, d.DependencyID, d.DependencyKind, d.TargetRef, d.ValidationRef, now)
	return err
}

// ---------------------------------------------------------------------------
// 交付记录（验收通过、准备交付和真实接收交接分别记录）
// ---------------------------------------------------------------------------

// DeliveryRecord 是 delivery_records 的一行。
type DeliveryRecord struct {
	DeliveryID   string
	ObjectRef    string
	Stage        string // accepted/ready/handover/in_service
	RecordRef    string
	EvidenceRefs string
	OccurredAt   int64
	CreatedAt    int64
}

// recordDeliveryStage 记录交付阶段（P4-06.5）。
// 验收通过、准备交付和真实接收交接分别记录，最后才可决定在役。
func recordDeliveryStage(tx *sql.Tx, d DeliveryRecord, now int64) (DeliveryRecord, error) {
	if d.ObjectRef == "" || d.Stage == "" {
		return DeliveryRecord{}, errors.New("交付记录缺少对象或阶段")
	}
	// 阶段顺序：accepted → ready → handover → in_service（分别记录）。
	d.DeliveryID = id()
	d.OccurredAt = now
	d.CreatedAt = now
	_, err := tx.Exec(`INSERT INTO delivery_records(delivery_id,object_ref,stage,record_ref,evidence_refs,occurred_at,created_at)
VALUES(?,?,?,?,?,?,?)`, d.DeliveryID, d.ObjectRef, d.Stage, d.RecordRef, d.EvidenceRefs, now, now)
	if err != nil {
		return DeliveryRecord{}, err
	}
	return d, nil
}

// checkDeliveryComplete 核对交付完整（P4-06.5）。
// 验收通过、准备交付和真实接收交接分别记录，最后才可决定在役。
func checkDeliveryComplete(q recordsQuerier, objectRef string) (complete bool, missing []string, err error) {
	required := []string{"accepted", "ready", "handover"}
	for _, stage := range required {
		var exists bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM delivery_records WHERE object_ref=? AND stage=?)`,
			objectRef, stage).Scan(&exists); err != nil {
			return false, nil, err
		}
		if !exists {
			missing = append(missing, stage)
		}
	}
	return len(missing) == 0, missing, nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
