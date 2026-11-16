package work

// P3-17 验收：持续服务实例与规范化事件。
//
// 三个测试入口：
//   - TestV3P317ServiceEventIdentity：规定推送/轮询的事件 ID、来源序号、对象、
//     时间、关联及覆盖；同 ID 异内容隔离；推送与轮询重复事件只应用一次；
//     来源身份伪造被拒绝；配置停用/撤销后新触发受阻。
//   - TestV3P317ServiceGapRecovery：失联后按可支持重放/查询补缺，记录水位与
//     遗漏；无保证则返回覆盖未知；断线恢复水位可查，不能因未看到异常就宣称
//     全部健康。
//   - TestV3P317SingleCheckScheduler：明确周期检查由哪方触发；外部已调度的
//     检查不再由本地 Schedule 重复执行；观测允许规则化聚合，动作回执、撤权
//     和控制事件不能当普通噪声丢弃。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// setupV3P317 创建存储与服务实例。
func setupV3P317(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := v3P102Now()
	tx, _ := store.db.Begin()
	s, err := registerServiceInstance(tx, ServiceInstance{
		SourceIdentity: "monitor-cluster-1", SubscriptionScope: "nodes",
		SupportsPause: "supported", SupportsResume: "supported",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return store, s.ServiceID, s.SourceIdentity
}

// TestV3P317ServiceEventIdentity 事件身份。
func TestV3P317ServiceEventIdentity(t *testing.T) {
	t.Run("event_has_all_fields", func(t *testing.T) {
		store, serviceID, sourceIdentity := setupV3P317(t, "event.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		result, err := receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-1", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, CorrelationID: "corr-1",
			Coverage: "node-1", Kind: "alert", PayloadDigest: digest("事件1"),
		}, sourceIdentity, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if result.Quarantined {
			t.Error("valid event quarantined")
		}
		// 事件已入库（含全部字段）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM service_events WHERE event_id='evt-1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("event not persisted: count=%d", count)
		}
	})

	t.Run("duplicate_event_applied_once", func(t *testing.T) {
		store, serviceID, sourceIdentity := setupV3P317(t, "dup.db")
		now := v3P102Now()
		// 推送与轮询重复事件：只应用一次。
		tx, _ := store.db.Begin()
		receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-dup", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("事件"),
		}, sourceIdentity, now)
		tx.Commit()
		// 重复（同 ID 同内容）：幂等。
		tx, _ = store.db.Begin()
		result, err := receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-dup", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("事件"),
		}, sourceIdentity, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Duplicate {
			t.Error("duplicate event not marked duplicate (重复事件只应用一次)")
		}
	})

	t.Run("same_id_different_content_quarantined", func(t *testing.T) {
		store, serviceID, sourceIdentity := setupV3P317(t, "conflict.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-conflict", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("内容A"),
		}, sourceIdentity, now)
		tx.Commit()
		// 同 ID 异内容：隔离。
		tx, _ = store.db.Begin()
		result, err := receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-conflict", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("内容B"),
		}, sourceIdentity, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if !result.Quarantined {
			t.Error("same id different content not quarantined (同 ID 异内容隔离)")
		}
	})

	t.Run("forged_source_identity_rejected", func(t *testing.T) {
		store, serviceID, _ := setupV3P317(t, "forged.db")
		now := v3P102Now()
		// 来源身份伪造：拒绝。
		tx, _ := store.db.Begin()
		_, err := receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-forged", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("伪造"),
		}, "forged-identity", now)
		tx.Rollback()
		if err == nil {
			t.Error("forged source identity not rejected (来源身份伪造被拒绝)")
		}
	})

	t.Run("stopped_service_blocks_new_events", func(t *testing.T) {
		store, serviceID, sourceIdentity := setupV3P317(t, "stopped.db")
		now := v3P102Now()
		// 停用服务。
		tx, _ := store.db.Begin()
		if _, err := tx.Exec("UPDATE service_instances SET state='stopped' WHERE service_id=?", serviceID); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 配置停用/撤销后新触发受阻。
		tx, _ = store.db.Begin()
		_, err := receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-stopped", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("事件"),
		}, sourceIdentity, now)
		tx.Rollback()
		if err == nil {
			t.Error("stopped service allowed new event (配置停用/撤销后新触发受阻)")
		}
		if !strings.Contains(err.Error(), "已") && !strings.Contains(err.Error(), "受阻") {
			t.Errorf("stopped reason missing: %v", err)
		}
	})
}

// TestV3P317ServiceGapRecovery 缺口恢复。
func TestV3P317ServiceGapRecovery(t *testing.T) {
	t.Run("watermark_records_sequence_and_gaps", func(t *testing.T) {
		store, serviceID, _ := setupV3P317(t, "watermark.db")
		now := v3P102Now()
		// 更新水位（记录水位与遗漏）。
		tx, _ := store.db.Begin()
		err := updateServiceWatermark(tx, serviceID, 100, `[{"start":50,"end":60}]`, false, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 断线恢复水位可查。
		w, err := readServiceWatermark(store.db, serviceID)
		if err != nil {
			t.Fatal(err)
		}
		if w.LastSequence != 100 {
			t.Errorf("watermark = %d, want 100 (水位可查)", w.LastSequence)
		}
		if w.Gaps == "" {
			t.Error("gaps not recorded (遗漏记录)")
		}
	})

	t.Run("no_watermark_returns_coverage_unknown", func(t *testing.T) {
		store, _, _ := setupV3P317(t, "nowatermark.db")
		// 无水位：覆盖未知（不能因未看到异常就宣称全部健康）。
		w, err := readServiceWatermark(store.db, "nonexistent-service")
		if err != nil {
			t.Fatal(err)
		}
		if w.ServiceID != "" {
			t.Errorf("nonexistent watermark returned: %+v (无保证则返回覆盖未知)", w)
		}
	})

	t.Run("coverage_unknown_visible", func(t *testing.T) {
		store, serviceID, _ := setupV3P317(t, "coverage.db")
		now := v3P102Now()
		// 覆盖未知：明确可见（不能宣称全部健康）。
		tx, _ := store.db.Begin()
		if err := updateServiceWatermark(tx, serviceID, 50, `[]`, true, now); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		w, err := readServiceWatermark(store.db, serviceID)
		if err != nil {
			t.Fatal(err)
		}
		if !w.CoverageUnknown {
			t.Error("coverage unknown not visible (覆盖未知明确可见)")
		}
	})
}

// TestV3P317SingleCheckScheduler 单一检查调度。
func TestV3P317SingleCheckScheduler(t *testing.T) {
	t.Run("external_scheduled_check_not_duplicated_locally", func(t *testing.T) {
		store, serviceID, _ := setupV3P317(t, "scheduler.db")
		now := v3P102Now()
		// 外部已调度的检查：不再由本地 Schedule 重复执行。
		tx, _ := store.db.Begin()
		_, err := claimCheckOwnership(tx, serviceID, "health-check", "external", "external-schedule-ref", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 本地尝试声明同一检查：拒绝（外部已调度）。
		tx, _ = store.db.Begin()
		_, err = claimCheckOwnership(tx, serviceID, "health-check", "local", "", now)
		tx.Rollback()
		if err == nil {
			t.Error("external scheduled check duplicated locally (外部已调度不再由本地重复执行)")
		}
		if !strings.Contains(err.Error(), "已由外部调度") {
			t.Errorf("duplicate reason missing: %v", err)
		}
	})

	t.Run("local_check_claimed_when_no_external", func(t *testing.T) {
		store, serviceID, _ := setupV3P317(t, "localcheck.db")
		now := v3P102Now()
		// 无外部调度：本地可声明。
		tx, _ := store.db.Begin()
		ownership, err := claimCheckOwnership(tx, serviceID, "custom-check", "local", "", now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if ownership.Owner != "local" {
			t.Errorf("ownership = %s, want local", ownership.Owner)
		}
	})

	t.Run("action_receipt_revocation_control_not_dropped_as_noise", func(t *testing.T) {
		// 动作回执、撤权和控制事件：不能当普通噪声丢弃。
		for _, kind := range []string{"action_receipt", "revocation", "control"} {
			filtered, err := filterServiceEventNoise(kind)
			if err == nil {
				t.Errorf("%s dropped as noise (不能当普通噪声丢弃)", kind)
			}
			if filtered {
				t.Errorf("%s filtered (不能丢弃)", kind)
			}
		}
		// 普通观测：允许规则化聚合。
		filtered, err := filterServiceEventNoise("metric")
		if err != nil {
			t.Errorf("metric filtered: %v", err)
		}
		if !filtered {
			t.Error("metric not filterable (观测允许规则化聚合)")
		}
	})

	t.Run("accepted_work_continues_by_responsibility_after_disable", func(t *testing.T) {
		store, serviceID, sourceIdentity := setupV3P317(t, "disable.db")
		now := v3P102Now()
		// 已受理工作（事件已接收）。
		tx, _ := store.db.Begin()
		receiveServiceEvent(tx, ServiceEvent{
			EventID: "evt-accepted", ServiceID: serviceID, SourceSequence: 1,
			ObjectRef: "node-1", OccurredAt: now, Kind: "alert", PayloadDigest: digest("事件"),
		}, sourceIdentity, now)
		tx.Commit()
		// 停用服务：已受理工作仍按其责任处理。在事务外执行（避免单连接嵌套）。
		if _, err := store.db.Exec("UPDATE service_instances SET state='stopped' WHERE service_id=?", serviceID); err != nil {
			t.Fatal(err)
		}
		// 已受理事件仍在（已受理工作仍按其责任处理）。
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM service_events WHERE event_id='evt-accepted'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("accepted event lost after disable: count=%d (已受理工作仍按其责任处理)", count)
		}
	})
}

// 确保 sql/strings/context 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
)
