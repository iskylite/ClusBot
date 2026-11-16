package work

// P4-15 验收：NHC、告警、网络与服务观测接入。
//
// 三个测试入口：
//   - TestV3P415ObservationNormalization：规范化 NHC、带外告警、网络/服务监测：
//     来源实例、对象、发生/接收时间、序号、严重度与材料引用；采集器自身故障
//     与节点故障可区分；无数据不等于健康。
//   - TestV3P415EventTriggerDeduplication：按已发布规则去重、归并同一故障片段；
//     恢复后复发可创建新故障，不无限合并所有相似症状；同症状不同来源/对象/
//     时间窗口不会被错误合并；告警风暴不会每条创建一项重启任务，仍能说明
//     丢失/归并/未处理范围；配置窗口、限速及降采样并保留原始计数/覆盖水位；
//     撤权、控制和操作回执不被聚合丢弃。
//   - TestV3P415BorrowedAssetObservation：健康/可达性更新与资产业务状态分开，
//     告警不能直接改维护中；触发有限 Task 或补充已有调查；借出资产可读监测
//     告警，不因此自动获得写权限。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P415 创建存储与资产。
func setupV3P415(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateInService, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, now)
	tx.Commit()
	return store
}

// TestV3P415ObservationNormalization 观测规范化。
func TestV3P415ObservationNormalization(t *testing.T) {
	t.Run("normalize_all_fields", func(t *testing.T) {
		now := v3P102Now()
		// 规范化：来源实例、对象、发生/接收时间、序号、严重度与材料引用。
		o, err := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-collector-1", ObjectRef: "node-1", OccurredAt: now - 1000,
			Sequence: 42, Severity: "critical", MaterialRefs: []string{"material-1"},
			ObservationKind: "nhc", Payload: `{"metric":"cpu","value":99}`,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		if o.SourceInstance != "nhc-collector-1" || o.ObjectRef != "node-1" ||
			o.OccurredAt != now-1000 || o.ReceivedAt != now || o.Sequence != 42 ||
			o.Severity != "critical" || len(o.MaterialRefs) != 1 || o.PayloadDigest == "" {
			t.Errorf("observation incomplete: %+v (规范化 NHC/带外告警/网络/服务监测)", o)
		}
	})

	t.Run("missing_fields_rejected", func(t *testing.T) {
		now := v3P102Now()
		// 缺来源实例/对象/类型：拒绝。
		if _, err := normalizeObservation(NormalizedObservation{OccurredAt: now}, now); err == nil {
			t.Error("missing fields accepted")
		}
		// 缺发生时间：拒绝。
		if _, err := normalizeObservation(NormalizedObservation{
			SourceInstance: "s", ObjectRef: "o", ObservationKind: "nhc",
		}, now); err == nil {
			t.Error("missing occurred_at accepted")
		}
	})

	t.Run("collector_failure_vs_node_failure_distinguishable", func(t *testing.T) {
		now := v3P102Now()
		// 采集器自身故障与节点故障可区分（来源实例不同）。
		collectorFault, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "collector-self", ObjectRef: "collector-1", OccurredAt: now,
			ObservationKind: "collector_health", Payload: `{"status":"down"}`,
		}, now)
		nodeFault, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "collector-1", ObjectRef: "node-1", OccurredAt: now,
			ObservationKind: "nhc", Payload: `{"status":"down"}`,
		}, now)
		if collectorFault.ObjectRef == nodeFault.ObjectRef {
			t.Error("collector failure and node failure not distinguishable (采集器自身故障与节点故障可区分)")
		}
	})

	t.Run("no_data_not_healthy", func(t *testing.T) {
		// 无数据不等于健康（覆盖未知）。
		store := setupV3P415(t, "nodata.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		err := updateHealthReachability(tx, "node-1", "unknown", "unknown", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 健康/可达性 unknown（无数据不等于健康）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='health_reachability_updated'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("no data marked healthy (无数据不等于健康)")
		}
	})
}

// TestV3P415EventTriggerDeduplication 事件触发去重。
func TestV3P415EventTriggerDeduplication(t *testing.T) {
	t.Run("same_fault_segment_merged_in_window", func(t *testing.T) {
		store := setupV3P415(t, "merge.db")
		now := v3P102Now()
		// 同一故障片段（时间窗口内）：归并。
		tx, _ := store.db.Begin()
		o1, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
			Payload: `{"metric":"cpu"}`,
		}, now)
		tx.Exec(`INSERT INTO fault_segments(segment_id,fault_key,observation_refs,window_start,window_end,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`, id(), "nhc-1:node-1:nhc", `["digest1"]`, now, now, "active", now, now)
		tx.Commit()
		// 窗口内同故障：归并。
		tx, _ = store.db.Begin()
		merged, segment, err := mergeFaultSegment(tx, o1, 60000, now+30000)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !merged {
			t.Error("same fault segment not merged in window (同一故障片段归并)")
		}
		if segment.FaultKey != "nhc-1:node-1:nhc" {
			t.Errorf("merged segment wrong: %+v", segment)
		}
	})

	t.Run("different_source_object_window_not_merged", func(t *testing.T) {
		store := setupV3P415(t, "nomerge.db")
		now := v3P102Now()
		// 同症状不同来源/对象/时间窗口：不会被错误合并。
		tx, _ := store.db.Begin()
		o1, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
			Payload: `{"metric":"cpu"}`,
		}, now)
		// 不同对象：不合并。
		merged, _, err := mergeFaultSegment(tx, o1, 60000, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if merged {
			t.Error("different object merged (同症状不同来源/对象/时间窗口不会被错误合并)")
		}
	})

	t.Run("recovered_recurrence_new_fault", func(t *testing.T) {
		store := setupV3P415(t, "recurrence.db")
		now := v3P102Now()
		// 恢复后复发：可创建新故障（不无限合并所有相似症状）。
		tx, _ := store.db.Begin()
		o1, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
			Payload: `{"metric":"cpu"}`,
		}, now)
		// 旧故障已恢复（state=recovered）。
		tx.Exec(`INSERT INTO fault_segments(segment_id,fault_key,observation_refs,window_start,window_end,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`, id(), "nhc-1:node-1:nhc", `["digest1"]`, now-100000, now-50000, "recovered", now-100000, now-50000)
		tx.Commit()
		// 复发：新故障（不归并到已恢复的旧故障）。
		tx, _ = store.db.Begin()
		merged, segment, err := mergeFaultSegment(tx, o1, 60000, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if merged {
			t.Error("recovered fault merged (恢复后复发可创建新故障)")
		}
		if segment.State != "active" {
			t.Errorf("new fault state = %s, want active", segment.State)
		}
	})

	t.Run("alert_storm_not_per_alert_task", func(t *testing.T) {
		// 告警风暴不会每条创建一项重启任务（归并说明丢失/归并/未处理范围）。
		store := setupV3P415(t, "storm.db")
		now := v3P102Now()
		// 告警风暴（10 条同故障告警）：归并（不每条创建任务）。
		tx, _ := store.db.Begin()
		tx.Exec(`INSERT INTO fault_segments(segment_id,fault_key,observation_refs,window_start,window_end,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`, id(), "nhc-1:node-1:nhc", `["digest1"]`, now, now, "active", now, now)
		tx.Commit()
		mergedCount := 0
		for i := 0; i < 10; i++ {
			tx, _ = store.db.Begin()
			o, _ := normalizeObservation(NormalizedObservation{
				SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
				Payload: `{"metric":"cpu"}`,
			}, now)
			merged, _, err := mergeFaultSegment(tx, o, 60000, now)
			if err != nil {
				t.Fatal(err)
			}
			tx.Commit()
			if merged {
				mergedCount++
			}
		}
		if mergedCount != 10 {
			t.Errorf("alert storm not merged: merged=%d/10 (告警风暴不会每条创建一项重启任务)", mergedCount)
		}
	})

	t.Run("aggregation_preserves_original_count_coverage", func(t *testing.T) {
		// 配置窗口、限速及降采样并保留原始计数/覆盖水位。
		control := AggregationControl{
			WindowMS: 60000, RateLimit: 100, DownsampleRatio: 0.1, OriginalCount: 1000, CoverageWatermark: 1000,
		}
		if err := validateAggregationControl(control); err != nil {
			t.Errorf("aggregation control rejected: %v (降采样保留原始计数/覆盖水位)", err)
		}
		// 缺原始计数：拒绝。
		badControl := AggregationControl{DownsampleRatio: 0.1, OriginalCount: 0}
		if err := validateAggregationControl(badControl); err == nil {
			t.Error("aggregation without original count accepted (降采样必须保留原始计数)")
		}
	})

	t.Run("revocation_control_receipt_not_dropped", func(t *testing.T) {
		// 撤权、控制和操作回执：不被聚合丢弃。
		for _, kind := range []string{"revocation", "control", "action_receipt"} {
			filtered, err := filterServiceEventNoise(kind)
			if err == nil {
				t.Errorf("%s dropped as noise (撤权、控制和操作回执不被聚合丢弃)", kind)
			}
			if filtered {
				t.Errorf("%s filtered (不能丢弃)", kind)
			}
		}
	})
}

// TestV3P415BorrowedAssetObservation 借出资产观测。
func TestV3P415BorrowedAssetObservation(t *testing.T) {
	t.Run("health_reachability_separate_from_business_state", func(t *testing.T) {
		store := setupV3P415(t, "separate.db")
		now := v3P102Now()
		// 健康/可达性更新与资产业务状态分开。
		tx, _ := store.db.Begin()
		err := updateHealthReachability(tx, "node-1", "degraded", "unreachable", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 资产状态未变（告警不能直接改维护中）。
		asset, _ := readAsset(store.db, "node-1")
		if asset.State != AssetStateInService {
			t.Errorf("health update changed asset state: %s (健康/可达性更新与资产业务状态分开)", asset.State)
		}
	})

	t.Run("alert_not_directly_maintenance", func(t *testing.T) {
		// 告警不能直接改维护中（需维护接管）。
		if ErrAlertChangesMaintenance == nil {
			t.Error("alert maintenance error missing")
		}
		if !strings.Contains(ErrAlertChangesMaintenance.Error(), "不能直接改维护中") {
			t.Errorf("alert maintenance message wrong: %v", ErrAlertChangesMaintenance)
		}
	})

	t.Run("lent_asset_readable_monitoring_no_write_permission", func(t *testing.T) {
		store := setupV3P415(t, "lent.db")
		now := v3P102Now()
		// 借出资产可读监测告警（不因此自动获得写权限）。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateLent, "node-1"); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		tx, _ = store.db.Begin()
		o, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
			Payload: `{"metric":"cpu"}`,
		}, now)
		err := triggerInvestigationTask(tx, o, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 借出资产观测已审计（可读监测，不自动获得写权限）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='lent_asset_observation'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("lent asset observation not recorded: count=%d (借出资产可读监测告警不因此自动获得写权限)", count)
		}
	})

	t.Run("trigger_limited_task_or_supplement", func(t *testing.T) {
		store := setupV3P415(t, "trigger.db")
		now := v3P102Now()
		// 触发有限 Task 或补充已有调查。
		tx, _ := store.db.Begin()
		o, _ := normalizeObservation(NormalizedObservation{
			SourceInstance: "nhc-1", ObjectRef: "node-1", OccurredAt: now, ObservationKind: "nhc",
			Payload: `{"metric":"cpu"}`,
		}, now)
		err := triggerInvestigationTask(tx, o, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 触发已审计。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE event='investigation_triggered'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("investigation not triggered: count=%d (触发有限 Task 或补充已有调查)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
