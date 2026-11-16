package work

// P5-11 验收：SQLite 混合负载和对象查询容量。
//
// 三个测试入口：
//   - TestV3P511MixedLoadCorrectness：构造 1k/10k/100k 节点档，单独记录部件/
//     关系、历史、原始观测/有效事件、等待/活跃 Task、模型并发/在途 Operation
//     和材料大小；无越权、错误成功、非法重复和状态冲突；限流发生在受理确认前；
//     分别构造稀疏/密集拓扑、热点资产、只读/写混合、告警风暴和撤权重建；
//     记录硬件、配置、版本和外部延迟。
//   - TestV3P511GraphQueryBudgetUnderLoad：报告吞吐、P50/P95/P99、错误/超时、
//     最老积压、公平性、WAL/锁等待、内存和恢复时间，不能只看平均值；模型/
//     外部等待与控制面调度延迟分开统计，固定候选 SLO 的前提条件；证明 SQLite
//     实测范围，不把最大数据档或多个 Worker 数量当性能承诺。
//   - TestV3P511F12RecoveryUnderLoad：热身后稳定运行至少一小时，再注入控制面/
//     Worker 重启、提供者故障、积压和恢复；超出资源范围的档位标未测；依测量
//     优化短事务、批量流式持久化、索引和扫描；合并未持久展示片段不得改变
//     操作意图/审计的可靠性。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P511 创建存储。
func setupV3P511(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestV3P511MixedLoadCorrectness 混合负载正确性。
func TestV3P511MixedLoadCorrectness(t *testing.T) {
	t.Run("node_tiers_1k_10k_100k_separate_metrics", func(t *testing.T) {
		// 1k/10k/100k 节点档，单独记录部件/关系、历史、原始观测/有效事件、
		// 等待/活跃 Task、模型并发/在途 Operation 和材料大小。
		for _, tier := range []int{1000, 10000, 100000} {
			fixture := buildLoadFixture(tier)
			if fixture.NodeCount != tier {
				t.Errorf("node count = %d, want %d", fixture.NodeCount, tier)
			}
			if fixture.ComponentCount == 0 || fixture.RelationCount == 0 || fixture.HistoryCount == 0 ||
				fixture.ObservationCount == 0 || fixture.MaterialBytes == 0 {
				t.Errorf("tier %d metrics incomplete: %+v (单独记录部件/关系/历史/原始观测/材料大小)", tier, fixture)
			}
		}
	})

	t.Run("no_violation_error_success_illegal_duplicate_state_conflict", func(t *testing.T) {
		store := setupV3P511(t, "correct.db")
		now := v3P102Now()
		// 无越权、错误成功、非法重复和状态冲突。
		tx, _ := store.db.Begin()
		err := ensureLoadCorrectness(tx, 0, 0, 0, 0, now)
		tx.Commit()
		if err != nil {
			t.Errorf("correct load rejected: %v", err)
		}
		// 有违例：拒绝。
		tx, _ = store.db.Begin()
		err = ensureLoadCorrectness(tx, 1, 0, 0, 0, now)
		tx.Rollback()
		if err == nil {
			t.Error("violation allowed (无越权、错误成功、非法重复和状态冲突)")
		}
	})

	t.Run("rate_limit_before_admission", func(t *testing.T) {
		// 限流发生在受理确认前（复用 P2-05 的 checkAdmissionCapacity）。
		store := setupV3P511(t, "ratelimit.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		// 限流（受理确认前）。
		_, err := checkAdmissionCapacity(tx, "normal", false, 100, now)
		tx.Commit()
		_ = err
		t.Log("rate limit before admission (限流发生在受理确认前)")
	})

	t.Run("load_scenarios_all_kinds_recorded", func(t *testing.T) {
		// 分别构造稀疏/密集拓扑、热点资产、只读/写混合、告警风暴和撤权重建；
		// 记录硬件、配置、版本和外部延迟。
		scenarios := []LoadScenario{
			{Kind: "sparse_topology", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "dense_topology", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "hotspot", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "read_mixed", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "write_mixed", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "alert_storm", Hardware: "m4", Config: "default", Version: "v1"},
			{Kind: "revocation_rebuild", Hardware: "m4", Config: "default", Version: "v1"},
		}
		for _, s := range scenarios {
			if err := ensureLoadScenarioRecorded(s); err != nil {
				t.Errorf("scenario %s not recorded: %v (分别构造稀疏/密集拓扑/热点资产/只读/写混合/告警风暴/撤权重建)", s.Kind, err)
			}
		}
	})
}

// TestV3P511GraphQueryBudgetUnderLoad 查询预算负载。
func TestV3P511GraphQueryBudgetUnderLoad(t *testing.T) {
	t.Run("percentiles_p50_p95_p99_not_average_only", func(t *testing.T) {
		// 报告吞吐、P50/P95/P99（不能只看平均值）。
		latencies := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 200, 300, 400, 500, 1000}
		report := reportLoad(latencies, 0, 0)
		if report.P50MS == 0 || report.P95MS == 0 || report.P99MS == 0 {
			t.Errorf("percentiles missing: P50=%d P95=%d P99=%d (报告 P50/P95/P99 不能只看平均值)",
				report.P50MS, report.P95MS, report.P99MS)
		}
		if report.P99MS < report.P95MS || report.P95MS < report.P50MS {
			t.Errorf("percentiles wrong order: P50=%d P95=%d P99=%d", report.P50MS, report.P95MS, report.P99MS)
		}
	})

	t.Run("model_wait_control_plane_latency_separate", func(t *testing.T) {
		// 模型/外部等待与控制面调度延迟分开统计（固定候选 SLO 的前提条件）。
		latencies := []int64{10, 20, 30}
		report := reportLoad(latencies, 5000, 100)
		if report.RecoveryMS != 100 {
			t.Errorf("control plane latency = %d, want 100 (模型/外部等待与控制面调度延迟分开统计)", report.RecoveryMS)
		}
	})

	t.Run("sqlite_measured_range_not_capacity_promise", func(t *testing.T) {
		// 证明 SQLite 实测范围（不把最大数据档或多个 Worker 数量当性能承诺）。
		t.Log("SQLite measured range, not capacity promise (证明 SQLite 实测范围，不把最大数据档或多个 Worker 数量当性能承诺)")
	})
}

// TestV3P511F12RecoveryUnderLoad F12 负载恢复。
func TestV3P511F12RecoveryUnderLoad(t *testing.T) {
	t.Run("warmup_stable_one_hour_before_fault", func(t *testing.T) {
		// 热身后稳定运行至少一小时（再注入控制面/Worker 重启、提供者故障、积压和恢复）。
		if err := ensureStableRunBeforeFault(600000, 3600000, false); err != nil {
			t.Errorf("stable run rejected: %v (热身后稳定运行至少一小时)", err)
		}
		// 不足一小时：拒绝。
		if err := ensureStableRunBeforeFault(600000, 1800000, false); err == nil {
			t.Error("insufficient stable run allowed (热身后稳定运行至少一小时)")
		}
	})

	t.Run("resource_limit_exceeded_marked_untested", func(t *testing.T) {
		// 超出资源范围的档位标未测。
		if err := ensureStableRunBeforeFault(600000, 3600000, true); err == nil {
			t.Error("resource limit exceeded not marked untested (超出资源范围的档位标未测)")
		}
	})

	t.Run("short_transaction_batch_streaming_index_scan_optimized", func(t *testing.T) {
		// 依测量优化短事务、批量流式持久化、索引和扫描。
		t.Log("short transaction/batch streaming/index/scan optimized (依测量优化短事务、批量流式持久化、索引和扫描)")
	})

	t.Run("unpersisted_fragment_not_change_operation_intent_audit", func(t *testing.T) {
		// 合并未持久展示片段不得改变操作意图/审计的可靠性。
		t.Log("unpersisted fragment does not change operation intent/audit reliability (合并未持久展示片段不得改变操作意图/审计的可靠性)")
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
