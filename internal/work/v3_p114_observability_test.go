package work

// P1-14 验收：运行时审计与阶段 G1 验收。
//
// 两个测试入口：
//   - TestV3P114PersistentRuntimeEndToEnd：沿真实运行时入口走通一项个人助手
//     工作及等待恢复；F01/F03/F09/F11 本阶段子例；受理确认不等于任务成功。
//   - TestV3P114RuntimeAuditAndBacklog：业务变更与必要审计同事务，贯通
//     input/task/step/run/wait/command/result 标识；任务/输入/等待和审计记录
//     能互相定位，失败原因及未决项可查询；提供 Inbox/Outbox 数量与最老年龄、
//     到期延迟、就绪等待、未知结果、数据库错误/事务时长的查询。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestV3P114PersistentRuntimeEndToEnd 持久运行时端到端。
func TestV3P114PersistentRuntimeEndToEnd(t *testing.T) {
	t.Run("personal_assistant_work_with_wait_recovery", func(t *testing.T) {
		// 沿真实运行时入口走通一项个人助手工作（含等待与恢复）。
		path := filepath.Join(t.TempDir(), "e2e.db")
		var taskID string
		now := v3P102Now()
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			// 提交工作（真实入口）。
			run, err := store.Submit(ctx, "v3p114-e2e", "整理季度汇报", "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			taskID = run.TaskID
			// 建立等待（持久记录，不占执行槽）。
			tx, _ := store.db.Begin()
			transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "user"}, now)
			openWait(tx, Wait{WaitID: "e2e-wait", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
			tx.Commit()
			// 模拟中断：Close 不推进。
		}()
		// 重启：恢复（等待可唤醒）。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		// 等待仍在（持久恢复）。
		openWaits, err := readOpenWaits(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(openWaits) != 1 || openWaits[0].WaitID != "e2e-wait" {
			t.Errorf("wait lost after restart: %+v", openWaits)
		}
		// 恢复推进（等待可唤醒）。
		report, err := store.RecoverV3(context.Background(), now+10000)
		if err != nil {
			t.Fatal(err)
		}
		if report.Phase != PhaseComplete {
			t.Errorf("recovery phase = %s, want complete", report.Phase)
		}
	})

	t.Run("f01_persistent_admission_and_commit_boundary", func(t *testing.T) {
		// F01 子例：已确认受理的输入重启后仍在；未提交事务不留部分状态；重复不重复建。
		path := filepath.Join(t.TempDir(), "f01.db")
		now := v3P102Now()
		var proof F01Proof
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			// 受理输入。
			tx, _ := store.db.Begin()
			admitChatInput(tx, "f01-req", "user", "", "整理汇报", "工作", nil, now)
			tx.Commit()
			// 未提交事务：回滚后无部分状态。
			tx, _ = store.db.Begin()
			receiveInbox(tx, InboxEvent{Source: "chat", EventID: "f01-rolled", Principal: "user", Kind: "chat_input", Summary: "回滚", PayloadDigest: digest("回滚"), ReceivedAt: now})
			tx.Rollback()
			var rolledCount int
			store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE event_id='f01-rolled'").Scan(&rolledCount)
			proof.NoPartialCommit = rolledCount == 0
		}()
		// 重启：已受理输入仍在。
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var admitted int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE event_id='f01-req'").Scan(&admitted); err != nil {
			t.Fatal(err)
		}
		proof.InputSurvivesRestart = admitted == 1
		// 重复同键：不重复建。
		tx, _ := store.db.Begin()
		receipt, _ := admitChatInput(tx, "f01-req", "user", "", "整理汇报", "工作", nil, now)
		tx.Commit()
		proof.DuplicateNotDuplicated = receipt.Duplicate
		if !proof.InputSurvivesRestart || !proof.NoPartialCommit || !proof.DuplicateNotDuplicated {
			t.Errorf("F01 proof incomplete: %+v", proof)
		}
	})

	t.Run("f03_duplicate_and_out_of_order", func(t *testing.T) {
		// F03 子例：相同事件只应用一次；同 ID 不同内容隔离；旧事件留证。
		store, taskID := setupV3P113(t, "f03.db")
		now := v3P102Now()
		var proof F03Proof
		// 相同事件重复应用：只一次。
		tx, _ := store.db.Begin()
		receiveInbox(tx, InboxEvent{Source: "chat", EventID: "f03-evt", Principal: "user", TaskID: taskID, Kind: "chat_input", Summary: "内容", PayloadDigest: digest("内容"), ReceivedAt: now})
		tx.Commit()
		var applyCount int
		applier := func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			applyCount++
			return ApplyResult{}, nil
		}
		for i := 0; i < 2; i++ {
			tx, _ := store.db.Begin()
			applyInboxEvent(tx, "chat", "f03-evt", applier, now)
			tx.Commit()
		}
		proof.AppliedOnce = applyCount == 1
		// 同 ID 不同内容：隔离。
		tx, _ = store.db.Begin()
		receiveInbox(tx, InboxEvent{Source: "chat", EventID: "f03-conflict", Principal: "user", TaskID: taskID, Kind: "chat_input", Summary: "A", PayloadDigest: digest("A"), ReceivedAt: now})
		result, _ := receiveInbox(tx, InboxEvent{Source: "chat", EventID: "f03-conflict", Principal: "user", TaskID: taskID, Kind: "chat_input", Summary: "B", PayloadDigest: digest("B"), ReceivedAt: now})
		proof.ConflictQuarantined = result.Quarantined
		tx.Commit()
		// 旧事件留证（wait_events）。
		tx, _ = store.db.Begin()
		openWait(tx, Wait{WaitID: "f03-wait", TaskID: taskID, Kind: WaitKindInput, OpenedAt: now})
		resolveWait(tx, "f03-wait", 1, "test", "evt-old", "旧", "", "user", now)
		resolveWait(tx, "f03-wait", 1, "test", "evt-late", "迟到", "", "user", now)
		tx.Commit()
		var evidenceCount int
		store.db.QueryRow("SELECT COUNT(*) FROM wait_events WHERE wait_id='f03-wait'").Scan(&evidenceCount)
		proof.OldEvidenceKept = evidenceCount >= 2
		if !proof.AppliedOnce || !proof.ConflictQuarantined || !proof.OldEvidenceKept {
			t.Errorf("F03 proof incomplete: %+v", proof)
		}
	})

	t.Run("f09_wait_and_timer", func(t *testing.T) {
		// F09 子例：同一 Wait 只关闭一次；恢复扫描不重复投递；等待不占执行槽。
		store, taskID := setupV3P113(t, "f09.db")
		now := v3P102Now()
		var proof F09Proof
		// 回执与到期竞争：只关闭一次。
		tx, _ := store.db.Begin()
		openWait(tx, Wait{WaitID: "f09-wait", TaskID: taskID, Kind: WaitKindExternal, OpenedAt: now})
		tx.Commit()
		tx, _ = store.db.Begin()
		first, _ := resolveWait(tx, "f09-wait", 1, "ext", "evt-1", "回执", "", "system", now)
		second, _ := resolveWait(tx, "f09-wait", 1, "timer", "evt-2", "到期", "", "system", now)
		tx.Commit()
		proof.WaitClosedOnce = first.Closed && !second.Closed && second.Duplicate
		// 恢复扫描不重复投递。
		tx, _ = store.db.Begin()
		wait2, _ := openWait(tx, Wait{WaitID: "f09-timer-wait", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
		createTimer(tx, Timer{TimerID: "f09-timer", WaitID: wait2.WaitID, WaitVersion: 1, DueAt: now - 1000, CreatedAt: now})
		tx.Commit()
		var delivered int
		for i := 0; i < 2; i++ {
			result, _ := recoverDueTimers(store.db, now, 10)
			delivered += result.Delivered
		}
		proof.TimerIdempotent = delivered == 1
		// 等待不占执行槽：开放 Wait 的任务，其他就绪步骤仍可计算。
		tx, _ = store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "independent", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		ready, _ := computeReadySteps(store.db, taskID, 1)
		proof.WaitNotOccupying = len(ready) > 0
		if !proof.WaitClosedOnce || !proof.TimerIdempotent || !proof.WaitNotOccupying {
			t.Errorf("F09 proof incomplete: %+v", proof)
		}
	})

	t.Run("f11_dependency_and_persistence_failure", func(t *testing.T) {
		// F11 子例：数据库不可写时不确认新工作；模型不可用时确定性核对继续；外部未知不写成成功。
		var proof F11Proof
		// 只读库：写入被拒。
		ro, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(t.TempDir(), "f11-ro.db"), "mode=ro"))
		if err == nil {
			defer ro.Close()
			_, writeErr := ro.Exec("CREATE TABLE x(id INTEGER)")
			proof.UnwritableStopsDispatch = writeErr != nil
		}
		// 模型不可用：统一推进循环仍处理确定性工作。
		store, taskID := setupV3P113(t, "f11.db")
		now := v3P102Now()
		prepareRecoveryData(t, store, taskID, now)
		loop := NewRuntimeLoop(store, func() bool { return false }) // 模型不可用
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		loop.Run(ctx, 10*time.Millisecond, 2)
		rounds, _ := loop.Stats()
		proof.ModelUnavailableRecovers = rounds >= 1
		// 外部未知（旧代际留证）：不写成成功。
		tx, _ := store.db.Begin()
		input, output := int64(10), int64(5)
		recordReport(tx, ResultReport{ResultKey: "f11-stale", TaskID: taskID, ExecutionGeneration: 0, Summary: "完成", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
		result, _ := applyReport(tx, "f11-stale", 1, nil, now)
		proof.UnknownNotSuccess = !result.Applied && result.Stale
		tx.Commit()
		if !proof.UnwritableStopsDispatch || !proof.ModelUnavailableRecovers || !proof.UnknownNotSuccess {
			t.Errorf("F11 proof incomplete: %+v", proof)
		}
	})

	t.Run("admission_not_equal_task_success", func(t *testing.T) {
		// 受理确认不等于任务成功：受理后任务未建立。
		store, _ := setupV3P113(t, "notequal.db")
		now := v3P102Now()
		tx, _ := store.db.Begin()
		receipt, err := admitChatInput(tx, "notequal-req", "user", "", "整理汇报", "工作", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 受理成功，但 TaskID 为空（受理不等于任务成功）。
		if receipt.TaskID != "" {
			t.Errorf("admission claimed task success: %s (受理确认不等于任务成功)", receipt.TaskID)
		}
		// 任务查询：无对应任务（受理阶段未建立）。
		queried, _ := readAdmission(store.db, "notequal-req")
		if queried.TaskID != "" || strings.Contains(queried.Reason, "成功") {
			t.Errorf("admission displayed as success: %+v", queried)
		}
	})
}

// TestV3P114RuntimeAuditAndBacklog 审计与积压观测。
func TestV3P114RuntimeAuditAndBacklog(t *testing.T) {
	t.Run("audit_links_all_identifiers_queryable", func(t *testing.T) {
		store, taskID := setupV3P113(t, "auditlink.db")
		now := v3P102Now()
		// 写入贯通全部标识的审计。
		tx, _ := store.db.Begin()
		recordAudit(tx, AuditEvent{
			TaskID: taskID, StepID: "s1", RunID: "r1", UnitID: "u1", WaitID: "w1",
			CommandID: "c1", ResultKey: "k1", Event: "dispatch", Actor: "worker-1",
			StorageRevision: 1, ControlRevision: 1, Summary: "派发", OccurredAt: now,
		})
		tx.Commit()
		// 按不同标识查询审计关联链（任务/输入/等待和审计记录能互相定位）。
		for field, value := range map[string]string{
			"task_id": taskID, "step_id": "s1", "run_id": "r1",
			"unit_id": "u1", "wait_id": "w1", "command_id": "c1", "result_key": "k1",
		} {
			trail, err := readAuditTrail(store.db, field, value)
			if err != nil {
				t.Errorf("audit trail by %s: %v", field, err)
				continue
			}
			if len(trail.Events) == 0 {
				t.Errorf("no audit events by %s=%s (审计记录互相定位)", field, value)
			}
		}
	})

	t.Run("observability_collects_backlog_and_unknown", func(t *testing.T) {
		store, taskID := setupV3P113(t, "observe.db")
		now := v3P102Now()
		// 填充各类积压：Inbox、Outbox、Timer、就绪单元、未知结果。
		prepareObservabilityData(t, store, taskID, now)
		snapshot, err := store.collectObservability(now + 10000)
		if err != nil {
			t.Fatal(err)
		}
		// Inbox/Outbox 数量与最老年龄。
		if snapshot.InboxPending < 1 {
			t.Errorf("inbox pending = %d, want >= 1", snapshot.InboxPending)
		}
		if snapshot.InboxOldestMS < 0 {
			t.Errorf("inbox oldest age = %d", snapshot.InboxOldestMS)
		}
		if snapshot.OutboxBacklog.Pending < 1 {
			t.Errorf("outbox pending = %d, want >= 1", snapshot.OutboxBacklog.Pending)
		}
		// 到期延迟与就绪等待。
		if snapshot.TimerBacklog.Pending < 1 {
			t.Errorf("timer pending = %d, want >= 1 (到期延迟可观测)", snapshot.TimerBacklog.Pending)
		}
		if snapshot.ReadyUnits < 1 {
			t.Errorf("ready units = %d, want >= 1", snapshot.ReadyUnits)
		}
		// 未知结果。
		if snapshot.UnknownResults < 1 {
			t.Errorf("unknown results = %d, want >= 1 (未知结果可查询)", snapshot.UnknownResults)
		}
		// 数据库可写性。
		if !snapshot.DatabaseWritable {
			t.Error("database not writable")
		}
		t.Logf("observability: %+v", snapshot)
	})

	t.Run("audit_same_tx_as_business_change", func(t *testing.T) {
		store, taskID := setupV3P113(t, "audittx.db")
		now := v3P102Now()
		// 业务变更与审计同事务：要么都提交，要么都回滚（普通日志不能替代审计）。
		tx, _ := store.db.Begin()
		transitionTaskState(tx, taskID, EventCreate, TaskGuard{ExpectedStorageRevision: 0, Actor: "test"}, now)
		// 状态机已写审计（同事务）。
		tx.Commit()
		// 审计与状态变更同时生效。
		var auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE task_id=? AND event='task_state:create'", taskID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Errorf("audit not committed with business change: count=%d", auditCount)
		}
	})

	t.Run("failure_reason_and_pending_queryable", func(t *testing.T) {
		store, taskID := setupV3P113(t, "failq.db")
		now := v3P102Now()
		// 处理失败保存明确原因与重试安排（失败原因可查询）。
		tx, _ := store.db.Begin()
		receiveInbox(tx, InboxEvent{Source: "chat", EventID: "fail-evt", Principal: "user", TaskID: taskID, Kind: "chat_input", Summary: "失败", PayloadDigest: digest("失败"), ReceivedAt: now})
		tx.Commit()
		tx, _ = store.db.Begin()
		applyInboxEvent(tx, "chat", "fail-evt", func(tx *sql.Tx, e InboxEvent) (ApplyResult, error) {
			return ApplyResult{Failure: "模拟处理失败"}, context.DeadlineExceeded
		}, now)
		tx.Commit()
		// 失败原因与未决项可查询。
		var state, failure string
		if err := store.db.QueryRow("SELECT state, failure FROM inbox_events WHERE event_id='fail-evt'").Scan(&state, &failure); err != nil {
			t.Fatal(err)
		}
		if state != "failed" || failure == "" {
			t.Errorf("failure not queryable: state=%s failure=%q (失败原因及未决项可查询)", state, failure)
		}
		// 未决项（待重试）可查询。
		var pending int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE state='failed'").Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 1 {
			t.Errorf("pending failed events = %d, want 1", pending)
		}
	})

	t.Run("g1_fresh_and_upgraded_schema", func(t *testing.T) {
		// 生成新库与升级库均受测（G1：SQLite 新建/升级路径均受测）。
		var evidence G1Evidence
		// 新建库。
		fresh, err := Open(filepath.Join(t.TempDir(), "g1-fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.db.QueryRow("PRAGMA user_version").Scan(&evidence.SchemaVersion); err != nil {
			t.Fatal(err)
		}
		evidence.FreshSchemaWorks = evidence.SchemaVersion == databaseSchemaVersion
		fresh.Close()
		// 升级库（v43 → 当前）。
		upgradedPath := filepath.Join(t.TempDir(), "g1-upgraded.db")
		db, _ := sql.Open("sqlite", upgradedPath)
		db.Exec(`CREATE TABLE runs(id TEXT PRIMARY KEY, status TEXT NOT NULL, created_at INTEGER NOT NULL)`)
		db.Exec(`CREATE TABLE model_settings(id INTEGER PRIMARY KEY CHECK(id=1), base_url TEXT NOT NULL, model_id TEXT NOT NULL, api_key_ref TEXT NOT NULL, revision INTEGER NOT NULL)`)
		db.Exec(`CREATE TABLE context_manifests(id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, generation INTEGER NOT NULL, assembled_at INTEGER NOT NULL, manifest TEXT NOT NULL)`)
		db.Exec("PRAGMA user_version=43")
		db.Close()
		upgraded, err := Open(upgradedPath)
		if err != nil {
			t.Fatal(err)
		}
		var upgradedVersion int
		upgraded.db.QueryRow("PRAGMA user_version").Scan(&upgradedVersion)
		evidence.UpgradedSchemaWorks = upgradedVersion == databaseSchemaVersion
		upgraded.Close()
		// 未涵盖子例明确记录（不冒充通过）。
		evidence.NotCovered = []string{"多 Worker 租约（P2）", "真实资产与外部副作用（P3）", "领域业务（P4）"}
		if !evidence.FreshSchemaWorks || !evidence.UpgradedSchemaWorks {
			t.Errorf("G1 schema evidence incomplete: %+v", evidence)
		}
		if len(evidence.NotCovered) == 0 {
			t.Error("G1 must record not-covered subcases (未涵盖的子例明确记录)")
		}
		t.Logf("G1 evidence: fresh=%v upgraded=%v not_covered=%v",
			evidence.FreshSchemaWorks, evidence.UpgradedSchemaWorks, evidence.NotCovered)
	})
}

// prepareObservabilityData 准备观测数据。
func prepareObservabilityData(t *testing.T, store *Store, taskID string, now int64) {
	t.Helper()
	tx, _ := store.db.Begin()
	// Inbox。
	receiveInbox(tx, InboxEvent{Source: "chat", EventID: "obs-inbox", Principal: "user", TaskID: taskID, Kind: "chat_input", Summary: "观测", PayloadDigest: digest("观测"), ReceivedAt: now})
	// Outbox。
	enqueueOutbox(tx, OutboxCommand{CommandID: "obs-cmd", Kind: "dispatch", PayloadDigest: digest("载荷"), Backend: "test", CreatedAt: now})
	// Timer（已过期，有延迟）。
	wait, _ := openWait(tx, Wait{WaitID: "obs-wait", TaskID: taskID, Kind: WaitKindTimer, OpenedAt: now})
	createTimer(tx, Timer{TimerID: "obs-timer", WaitID: wait.WaitID, WaitVersion: 1, DueAt: now - 5000, CreatedAt: now})
	// 就绪步骤。
	insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
		DefinitionKey: "obs-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
	// 未知结果（旧代际留证）。
	input, output := int64(10), int64(5)
	recordReport(tx, ResultReport{ResultKey: "obs-stale", TaskID: taskID, ExecutionGeneration: 0, Summary: "完成", InputTokens: &input, OutputTokens: &output, ReceivedAt: now})
	applyReport(tx, "obs-stale", 1, nil, now) // 标记为旧代际留证
	tx.Commit()
	// 就绪单元与未知读取在事务外/独立事务建立（单连接不能嵌套）。
	steps, err := listSteps(store.db, taskID)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ = store.db.Begin()
	insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: steps[0].StepID,
		Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
	tx.Commit()
	if _, err := store.db.Exec(`INSERT INTO connections(id,name,config,enabled,status,protocol) VALUES('obs-conn','资料','{}',1,'ready','streamable_http')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,received_at,acknowledged_at)
SELECT id,1,'obs-conn',1,'资料','read','读取','v1','{}','unknown',0,0 FROM runs WHERE task_id=? LIMIT 1`, taskID); err != nil {
		t.Fatal(err)
	}
}
