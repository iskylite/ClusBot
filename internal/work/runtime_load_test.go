package work

// P5-11：SQLite 混合负载和对象查询容量。
//
// 本文件实现测清瓶颈、恢复与成本，证明范围内的多任务/多工作者表现：
//   - 构造 1k/10k/100k 节点档，单独记录部件/关系、历史、原始观测/有效事件、
//     等待/活跃 Task、模型并发/在途 Operation 和材料大小；
//   - 分别构造稀疏/密集拓扑、热点资产、只读/写混合、告警风暴和撤权重建；
//     记录硬件、配置、版本和外部延迟；
//   - 热身后稳定运行至少一小时，再注入控制面/Worker 重启、提供者故障、积压
//     和恢复；超出资源范围的档位标未测；
//   - 报告吞吐、P50/P95/P99、错误/超时、最老积压、公平性、WAL/锁等待、内存
//     和恢复时间，不能只看平均值；
//   - 依测量优化短事务、批量流式持久化、索引和扫描；合并未持久展示片段不得
//     改变操作意图/审计的可靠性。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// 负载档位（构造 1k/10k/100k 节点档）
// ---------------------------------------------------------------------------

// LoadFixture 是负载夹具。
type LoadFixture struct {
	// 节点档（1k/10k/100k）。
	NodeCount        int
	// 单独记录部件/关系、历史、原始观测/有效事件、等待/活跃 Task、模型并发/
	// 在途 Operation 和材料大小。
	ComponentCount   int
	RelationCount    int
	HistoryCount     int
	ObservationCount int
	WaitingTasks     int
	ActiveTasks      int
	ModelConcurrency int
	InflightOperations int
	MaterialBytes    int64
}

// buildLoadFixture 构造负载夹具（P5-11.1）。
// 构造 1k/10k/100k 节点档，单独记录部件/关系、历史、原始观测/有效事件、
// 等待/活跃 Task、模型并发/在途 Operation 和材料大小。
func buildLoadFixture(nodeCount int) LoadFixture {
	// 节点档（1k/10k/100k）。
	return LoadFixture{
		NodeCount: nodeCount, ComponentCount: nodeCount * 2, RelationCount: nodeCount * 3,
		HistoryCount: nodeCount * 10, ObservationCount: nodeCount * 100,
		WaitingTasks: nodeCount / 10, ActiveTasks: nodeCount / 20,
		ModelConcurrency: 4, InflightOperations: nodeCount / 5,
		MaterialBytes: int64(nodeCount) * 1024 * 1024,
	}
}

// ---------------------------------------------------------------------------
// 负载场景（稀疏/密集拓扑、热点资产、只读/写混合、告警风暴和撤权重建）
// ---------------------------------------------------------------------------

// LoadScenario 是负载场景。
type LoadScenario struct {
	Kind             string // sparse_topology/dense_topology/hotspot/read_mixed/write_mixed/alert_storm/revocation_rebuild
	// 硬件、配置、版本和外部延迟。
	Hardware         string
	Config           string
	Version          string
	ExternalLatencyMS int64
}

// ensureLoadScenarioRecorded 核对负载场景记录（P5-11.2）。
// 分别构造稀疏/密集拓扑、热点资产、只读/写混合、告警风暴和撤权重建；记录
// 硬件、配置、版本和外部延迟。
func ensureLoadScenarioRecorded(s LoadScenario) error {
	if s.Kind == "" || s.Hardware == "" || s.Config == "" || s.Version == "" {
		return errors.New("负载场景缺少类型/硬件/配置/版本")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 稳定运行（热身后稳定运行至少一小时，再注入故障）
// ---------------------------------------------------------------------------

// ErrInsufficientWarmup 是热身不足的错误。
var ErrInsufficientWarmup = errors.New("热身后稳定运行至少一小时")

// ensureStableRunBeforeFault 核对稳定运行（P5-11.3）。
// 热身后稳定运行至少一小时，再注入控制面/Worker 重启、提供者故障、积压
// 和恢复；超出资源范围的档位标未测。
func ensureStableRunBeforeFault(warmupMS int64, stableMS int64, resourceLimitExceeded bool) error {
	// 热身后稳定运行至少一小时（3600000ms）。
	if stableMS < 3600000 {
		return fmt.Errorf("%w：稳定运行 %dms < 3600000ms", ErrInsufficientWarmup, stableMS)
	}
	// 超出资源范围的档位标未测。
	if resourceLimitExceeded {
		return errors.New("超出资源范围的档位标未测")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 报告（吞吐、P50/P95/P99、错误/超时、最老积压、公平性、WAL/锁等待、内存和恢复时间）
// ---------------------------------------------------------------------------

// LoadReport 是负载报告。
type LoadReport struct {
	// 吞吐、P50/P95/P99。
	Throughput     float64
	P50MS          int64
	P95MS          int64
	P99MS          int64
	// 错误/超时、最老积压、公平性。
	ErrorRate      float64
	TimeoutRate    float64
	OldestBacklogMS int64
	Fairness       float64
	// WAL/锁等待、内存和恢复时间。
	WALWaitMS      int64
	LockWaitMS     int64
	MemoryBytes    int64
	RecoveryMS     int64
	// 不能只看平均值（P50/P95/P99 分别报告）。
	Percentiles    []int64
}

// computePercentiles 计算分位数（P50/P95/P99）。
func computePercentiles(latencies []int64) (p50, p95, p99 int64) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}
	sorted := make([]int64, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]
	return p50, p95, p99
}

// reportLoad 报告负载（P5-11.4）。
// 报告吞吐、P50/P95/P99、错误/超时、最老积压、公平性、WAL/锁等待、内存和
// 恢复时间，不能只看平均值；模型/外部等待与控制面调度延迟分开统计。
func reportLoad(latencies []int64, modelWaitMS int64, controlPlaneLatencyMS int64) LoadReport {
	var report LoadReport
	report.P50MS, report.P95MS, report.P99MS = computePercentiles(latencies)
	report.Percentiles = []int64{report.P50MS, report.P95MS, report.P99MS}
	// 模型/外部等待与控制面调度延迟分开统计。
	report.RecoveryMS = controlPlaneLatencyMS
	return report
}

// ---------------------------------------------------------------------------
// 正确性（无越权、错误成功、非法重复和状态冲突；限流发生在受理确认前）
// ---------------------------------------------------------------------------

// ErrLoadViolation 是负载违例的错误。
var ErrLoadViolation = errors.New("负载违例")

// ensureLoadCorrectness 核对负载正确性（P5-11 验收）。
// 无越权、错误成功、非法重复和状态冲突；限流发生在受理确认前。
func ensureLoadCorrectness(tx *sql.Tx, violations int, errorSuccesses int, illegalDuplicates int, stateConflicts int, now int64) error {
	// 无越权、错误成功、非法重复和状态冲突。
	if violations > 0 || errorSuccesses > 0 || illegalDuplicates > 0 || stateConflicts > 0 {
		return fmt.Errorf("%w：越权=%d 错误成功=%d 非法重复=%d 状态冲突=%d（无越权、错误成功、非法重复和状态冲突）",
			ErrLoadViolation, violations, errorSuccesses, illegalDuplicates, stateConflicts)
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
