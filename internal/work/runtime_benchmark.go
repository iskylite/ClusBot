package work

// P4-12：单机基准测试与测量规范。
//
// 本文件实现测试参数和结果可比较，阈值来自领域规范：
//   - 保存测试套件/配置/环境版本、单位、持续时间、并发度、样本范围及证据引用；
//   - 提供者描述测试字段和输出格式，领域包决定适用条件、阈值与通过规则；
//   - 测试作为可能有影响的受控能力，检查占用和业务影响，不因名称含
//     benchmark 就视为只读；
//   - 记录失败、超时、未测和异常数据，不用零值替代未知；统计结果附采样条件；
//   - 固定版本的复测建立新记录并关联旧记录，保留条件差异。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 基准测试（测试套件/配置/环境版本、单位、持续时间、并发度、样本范围及证据引用）
// ---------------------------------------------------------------------------

// BenchmarkSpec 是基准测试规范。
type BenchmarkSpec struct {
	// 测试套件/配置/环境版本。
	SuiteKey         string
	ConfigVersion    int
	EnvironmentVersion string
	// 单位、持续时间、并发度、样本范围。
	Unit             string
	DurationMS       int64
	Concurrency      int
	SampleScope      string
	// 证据引用。
	EvidenceRefs     []string
	// 领域阈值（领域包决定适用条件、阈值与通过规则）。
	DomainThreshold  string
	PassRule         string
}

// BenchmarkResult 是基准测试结果。
type BenchmarkResult struct {
	// 测试标识与版本。
	ResultID         string
	SpecKey          string
	SpecVersion      int
	ObjectRef        string
	// 结果（记录失败、超时、未测和异常数据，不用零值替代未知）。
	Outcome          string // passed/failed/timeout/untested/abnormal
	Value            string
	Unit             string
	// 采样条件（统计结果附采样条件）。
	SamplingConditions string
	// 证据引用。
	EvidenceRefs     []string
	// 复测关联（固定版本的复测建立新记录并关联旧记录）。
	SupersedesResultID string
	ConditionDifferences string
	// 时间与执行者。
	ExecutedAt       int64
	ExecutedBy       string
}

// ---------------------------------------------------------------------------
// 基准测试执行（测试作为可能有影响的受控能力）
// ---------------------------------------------------------------------------

// ErrBenchmarkNotReadonly 是基准测试误为只读的错误。
var ErrBenchmarkNotReadonly = errors.New("基准测试是可能有影响的受控能力（不因名称含 benchmark 就视为只读）")

// executeBenchmark 执行基准测试（P4-12.3）。
// 测试作为可能有影响的受控能力，检查占用和业务影响，不因名称含 benchmark
// 就视为只读。
func executeBenchmark(tx *sql.Tx, objectRef string, subject string, spec BenchmarkSpec, now int64) error {
	// 测试作为可能有影响的受控能力：检查占用和业务影响。
	asset, err := readAsset(tx, objectRef)
	if err == nil {
		if asset.FrozenAt > 0 {
			return fmt.Errorf("对象 %s 已冻结（测试作为可能有影响的受控能力检查占用）", objectRef)
		}
		if asset.State == AssetStateLent {
			return fmt.Errorf("对象 %s 已借出（测试检查业务影响）", objectRef)
		}
	}
	// 授权核对（受控能力）。
	decision, err := decideAuthorization(tx, subject, "execute", objectRef, "", now, false)
	if err != nil {
		return err
	}
	if decision.Outcome != "allowed" {
		return fmt.Errorf("%w：主体 %s 无测试执行权限（测试作为受控能力）", ErrAuthorizationDenied, subject)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 结果记录（失败、超时、未测和异常数据，不用零值替代未知）
// ---------------------------------------------------------------------------

// recordBenchmarkResult 记录基准测试结果（P4-12.4/P4-12.5）。
// 记录失败、超时、未测和异常数据，不用零值替代未知；统计结果附采样条件；
// 固定版本的复测建立新记录并关联旧记录，保留条件差异。
func recordBenchmarkResult(tx *sql.Tx, r BenchmarkResult, now int64) (BenchmarkResult, error) {
	if r.SpecKey == "" || r.SpecVersion < 1 || r.ObjectRef == "" || r.ExecutedBy == "" {
		return BenchmarkResult{}, errors.New("基准测试结果缺少套件/版本/对象/执行者")
	}
	// 不用零值替代未知：outcome 必须明确（失败、超时、未测和异常数据分别记录）。
	validOutcomes := map[string]bool{"passed": true, "failed": true, "timeout": true, "untested": true, "abnormal": true}
	if !validOutcomes[r.Outcome] {
		return BenchmarkResult{}, fmt.Errorf("无效的结果状态 %s（不用零值替代未知）", r.Outcome)
	}
	// 统计结果附采样条件。
	if r.Outcome == "passed" && r.SamplingConditions == "" {
		return BenchmarkResult{}, errors.New("通过结果缺少采样条件（统计结果附采样条件）")
	}
	r.ResultID = id()
	r.ExecutedAt = now
	// 复测关联（固定版本的复测建立新记录并关联旧记录，保留条件差异）。
	if r.SupersedesResultID != "" && r.ConditionDifferences == "" {
		return BenchmarkResult{}, errors.New("复测缺少条件差异（保留条件差异）")
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 结果比较（不同配置/采样条件的指标不能无依据比较）
// ---------------------------------------------------------------------------

// ErrNotComparable 是指标不可比较的错误。
var ErrNotComparable = errors.New("不同配置/采样条件的指标不能无依据比较")

// compareBenchmarkResults 比较基准测试结果（P4-12 验收）。
// 不同配置/采样条件的指标不能无依据比较。
func compareBenchmarkResults(r1, r2 BenchmarkResult) (comparable bool, reason string, err error) {
	// 不同配置版本：不能无依据比较。
	if r1.SpecVersion != r2.SpecVersion {
		return false, fmt.Sprintf("配置版本不同（%d≠%d）", r1.SpecVersion, r2.SpecVersion), ErrNotComparable
	}
	// 不同单位：不能无依据比较。
	if r1.Unit != r2.Unit {
		return false, fmt.Sprintf("单位不同（%s≠%s）", r1.Unit, r2.Unit), ErrNotComparable
	}
	// 不同采样条件：不能无依据比较。
	if r1.SamplingConditions != r2.SamplingConditions {
		return false, fmt.Sprintf("采样条件不同（%s≠%s）", r1.SamplingConditions, r2.SamplingConditions), ErrNotComparable
	}
	return true, "可比较", nil
}

// 确保 context/sql/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
)
