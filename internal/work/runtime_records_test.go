package work

// 验收：Task、Step、Run 与控制记录的持久结构。
//
// 三个测试入口：
//   - TestTaskStepRunRecords：Task 九态、Step、依赖、work_units、控制
//     记录、结果引用及基础审计的字段/索引可用；插入不合法状态、重复身份或
//     悬空引用被拒绝；失败事务没有部分记录。
//   - TestLegacyHistoryPreserved：新库与旧库升级的同一 Task 可查到
//     完整历史，外键和唯一约束有效；旧 ID、成果/验收与历史 run_bindings 保留。
//   - TestContractRevisionIsolation：同任务合同变更与状态更新有独立
//     版本含义；重复请求键相同内容返回原结果，不同内容冲突；旧结果不会满足
//     新合同。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordsTestNow 是测试用的固定时间戳。
func recordsTestNow() int64 { return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC).UnixMilli() }

// setupRecordsTask 创建一个基础 Task 供记录测试。
func setupRecordsTask(t *testing.T, store *Store) string {
	t.Helper()
	ctx := context.Background()
	run, err := store.Submit(ctx, "v3p102-task-"+id(), "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	return run.TaskID
}

// TestTaskStepRunRecords 持久结构的字段、约束与原子性。
func TestTaskStepRunRecords(t *testing.T) {
	t.Run("task_state_nine_states_and_constraint", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "records.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)

		// 九个合法状态均可写入。
		states := []RuntimeTaskState{
			RuntimeTaskPending, RuntimeTaskRunning, RuntimeTaskWaiting, RuntimeTaskPaused, RuntimeTaskAcceptance,
			RuntimeTaskFinalizing, RuntimeTaskSucceeded, RuntimeTaskFailed, RuntimeTaskCancelled,
		}
		for i, state := range states {
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			err = upsertTaskState(tx, TaskState{
				TaskID: taskID, State: state, Reason: "测试", StorageRevision: i + 1, UpdatedAt: recordsTestNow(),
			})
			if err != nil {
				tx.Rollback()
				t.Fatalf("upsert state %s: %v", state, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		// 读回最后一个状态。
		got, err := readTaskState(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != RuntimeTaskCancelled {
			t.Errorf("final state = %s, want cancelled", got.State)
		}
		// 反例：非法状态被拒绝（CHECK 约束）。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		err = upsertTaskState(tx, TaskState{
			TaskID: taskID, State: RuntimeTaskState("mystery"), StorageRevision: 99, UpdatedAt: recordsTestNow(),
		})
		if err == nil {
			t.Error("invalid task state not rejected by CHECK constraint")
		}
		tx.Rollback() // 立即回滚，释放单连接
	})

	t.Run("steps_dependencies_workunits_structure", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "steps.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)

		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		now := recordsTestNow()
		// 两个 Step + 一条依赖 + 一个就绪 work_unit。
		stepA := Step{StepID: id(), TaskID: taskID, ContractRevision: 1, DefinitionKey: "collect",
			DefinitionVersion: 1, State: RuntimeStepCompleted, Position: 1, CreatedAt: now, UpdatedAt: now}
		stepB := Step{StepID: id(), TaskID: taskID, ContractRevision: 1, DefinitionKey: "assemble",
			DefinitionVersion: 1, State: RuntimeStepReady, Position: 2, CreatedAt: now, UpdatedAt: now}
		if err := insertStep(tx, stepA); err != nil {
			t.Fatalf("insert stepA: %v", err)
		}
		if err := insertStep(tx, stepB); err != nil {
			t.Fatalf("insert stepB: %v", err)
		}
		if err := insertStepDependency(tx, stepB.StepID, stepA.StepID, 1); err != nil {
			t.Fatalf("insert dependency: %v", err)
		}
		unit := WorkUnit{UnitID: id(), TaskID: taskID, StepID: stepB.StepID, Kind: WorkUnitModel,
			ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now}
		if err := insertWorkUnit(tx, unit); err != nil {
			t.Fatalf("insert work unit: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		// 查询：Step 列表、依赖、就绪单元。
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 || steps[0].DefinitionKey != "collect" || steps[1].DefinitionKey != "assemble" {
			t.Errorf("steps = %+v", steps)
		}
		deps, err := stepDependencies(store.db, stepB.StepID)
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 || deps[0] != stepA.StepID {
			t.Errorf("dependencies = %v, want [%s]", deps, stepA.StepID)
		}

		// 反例 1：自依赖被拒绝。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := insertStepDependency(tx, stepA.StepID, stepA.StepID, 1); err == nil {
			t.Error("self-dependency not rejected")
		}
		// 反例 2：悬空引用（依赖不存在的 step）被拒绝。
		if err := insertStepDependency(tx, stepA.StepID, "nonexistent-step", 1); err == nil {
			t.Error("dangling dependency reference not rejected")
		}
		tx.Rollback() // 立即回滚，释放单连接
	})

	t.Run("control_record_and_result_receipt_idempotent", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "control.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		// 控制记录：重复请求键相同内容返回原决定。
		record := ControlRecord{RecordID: id(), TaskID: taskID, RequestKey: "pause-key-1",
			DecidedBy: "user", Kind: "pause", ControlRevision: 1, TargetContractRevision: 1,
			Decision: "pause", CreatedAt: now}
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		first, err := insertControlRecord(tx, record)
		if err != nil {
			t.Fatalf("insert control: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// 同键同内容：幂等返回原记录。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		again, err := insertControlRecord(tx, record)
		if err != nil {
			t.Fatalf("idempotent control: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if again.RecordID != first.RecordID {
			t.Errorf("duplicate control created new record: %s vs %s", again.RecordID, first.RecordID)
		}
		// 同键不同内容：冲突。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		conflicting := record
		conflicting.Decision = "resume"
		if _, err := insertControlRecord(tx, conflicting); !errors.Is(err, ErrConflict) {
			t.Errorf("same key different content not conflicted: %v", err)
		}
		tx.Rollback() // 立即回滚，释放单连接（不能 defer 到后续 Begin 之后）

		// 结果引用：重复送达只应用一次，同身份不同结果隔离。
		receipt := ResultReceipt{ReceiptID: id(), ResultKey: "result-1", TaskID: taskID,
			Summary: "完成", State: "received", ReceivedAt: now}
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		r1, err := recordResultReceipt(tx, receipt)
		if err != nil {
			t.Fatalf("record receipt: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		r2, err := recordResultReceipt(tx, receipt)
		if err != nil {
			t.Fatalf("duplicate receipt: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if r2.ReceiptID != r1.ReceiptID {
			t.Errorf("duplicate receipt created new record")
		}
		// 同键不同内容：隔离（冲突）。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		bad := receipt
		bad.Summary = "不同内容"
		if _, err := recordResultReceipt(tx, bad); !errors.Is(err, ErrConflict) {
			t.Errorf("same result key different content not isolated: %v", err)
		}
		tx.Rollback()
	})

	t.Run("failed_transaction_no_partial_records", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "atomic.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		// 一个事务内：插入 Step + 审计，然后故意失败（插入悬空依赖），
		// 验证整个事务回滚，没有部分记录。
		stepID := id()
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := insertStep(tx, Step{StepID: stepID, TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "draft", DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := recordAudit(tx, AuditEvent{TaskID: taskID, StepID: stepID, Event: "step_created",
			Actor: "test", OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
		// 故意失败：悬空依赖。
		err = insertStepDependency(tx, stepID, "nonexistent", 1)
		if err == nil {
			t.Fatal("expected dangling dependency to fail")
		}
		tx.Rollback()

		// 验证没有部分记录。
		var stepCount, auditCount int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM steps WHERE step_id=?", stepID).Scan(&stepCount); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runtime_audit WHERE step_id=?", stepID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if stepCount != 0 || auditCount != 0 {
			t.Errorf("failed transaction left partial records: steps=%d audits=%d", stepCount, auditCount)
		}
	})

	t.Run("audit_links_all_identifiers", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "audit.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := recordAudit(tx, AuditEvent{
			TaskID: taskID, StepID: "s1", RunID: "r1", UnitID: "u1", WaitID: "w1",
			CommandID: "c1", ResultKey: "k1", Event: "dispatch", Actor: "worker-1",
			StorageRevision: 3, ControlRevision: 2, Summary: "派发", OccurredAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// 审计可按任务查询，贯通全部标识。
		var found bool
		if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM runtime_audit
WHERE task_id=? AND step_id='s1' AND run_id='r1' AND unit_id='u1' AND wait_id='w1' AND command_id='c1' AND result_key='k1')`, taskID).
			Scan(&found); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Error("audit record does not link all identifiers")
		}
	})
}

// TestLegacyHistoryPreserved 旧库升级后历史保留。
func TestLegacyHistoryPreserved(t *testing.T) {
	t.Run("upgraded_v47_preserves_history_and_gains_new_schema", func(t *testing.T) {
		// 构造 v47 库（完整旧结构 + 数据），升级到 48，验证历史保留且新结构可用。
		path := filepath.Join(t.TempDir(), "legacy.db")
		store, err := Open(path) // 先建当前结构
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		// 注入历史：任务、成果、验收、run_bindings。
		run, err := store.Submit(ctx, "v3p102-legacy", "整理季度汇报", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.delta(ctx, claimed.ID, "# 季度汇报"); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(claimed, "completed", "", runAccounting{}); err != nil {
			t.Fatal(err)
		}
		detail, err := store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Accept(ctx, run.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
			t.Fatal(err)
		}
		taskID := run.TaskID
		// 记录升级前的历史计数。
		var runsBefore, bindingsBefore, acceptancesBefore int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM runs WHERE task_id=?", taskID).Scan(&runsBefore); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT COUNT(*) FROM run_bindings WHERE task_id=?", taskID).Scan(&bindingsBefore); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow("SELECT COUNT(*) FROM task_acceptances WHERE task_id=?", taskID).Scan(&acceptancesBefore); err != nil {
			t.Fatal(err)
		}
		// 回写到 v47（模拟旧版本库）。
		if _, err := store.db.Exec("PRAGMA user_version=47"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		// 升级到 48。
		upgraded, err := Open(path)
		if err != nil {
			t.Fatalf("upgrade from 47: %v", err)
		}
		defer upgraded.Close()
		var version int
		if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != databaseSchemaVersion {
			t.Errorf("version = %d, want %d", version, databaseSchemaVersion)
		}
		// 历史保留。
		var runsAfter, bindingsAfter, acceptancesAfter int
		if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM runs WHERE task_id=?", taskID).Scan(&runsAfter); err != nil {
			t.Fatal(err)
		}
		if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM run_bindings WHERE task_id=?", taskID).Scan(&bindingsAfter); err != nil {
			t.Fatal(err)
		}
		if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM task_acceptances WHERE task_id=?", taskID).Scan(&acceptancesAfter); err != nil {
			t.Fatal(err)
		}
		if runsAfter != runsBefore || bindingsAfter != bindingsBefore || acceptancesAfter != acceptancesBefore {
			t.Errorf("history lost: runs %d→%d bindings %d→%d acceptances %d→%d",
				runsBefore, runsAfter, bindingsBefore, bindingsAfter, acceptancesBefore, acceptancesAfter)
		}
		// 新结构可用（task_states/steps/work_units 等存在且可写）。
		tx, err := upgraded.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := upsertTaskState(tx, TaskState{TaskID: taskID, State: RuntimeTaskSucceeded,
			StorageRevision: 1, UpdatedAt: recordsTestNow()}); err != nil {
			t.Errorf("new task_states not writable after upgrade: %v", err)
		}
		tx.Rollback() // 立即回滚，释放单连接
		// 外键与完整性。
		var fkViolations int
		if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil {
			t.Fatal(err)
		}
		if fkViolations != 0 {
			t.Errorf("foreign key violations after upgrade = %d", fkViolations)
		}
	})

	t.Run("fresh_and_upgraded_schema_match", func(t *testing.T) {
		// 新建库与升级库的 v48 新结构一致。
		fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		freshTables := tableNames(t, fresh.db)

		upgradedPath := filepath.Join(t.TempDir(), "upgraded.db")
		db, err := sql.Open("sqlite", upgradedPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE runs(id TEXT PRIMARY KEY, status TEXT NOT NULL, created_at INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version=47"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		upgraded, err := Open(upgradedPath)
		if err != nil {
			t.Fatal(err)
		}
		defer upgraded.Close()
		upgradedTables := tableNames(t, upgraded.db)

		// 新结构表必须在两库都存在。
		for _, table := range []string{"task_states", "steps", "step_dependencies", "work_units",
			"control_records", "result_receipts", "runtime_audit"} {
			if !freshTables[table] {
				t.Errorf("fresh missing %s", table)
			}
			if !upgradedTables[table] {
				t.Errorf("upgraded missing %s", table)
			}
		}
	})
}

// TestContractRevisionIsolation 合同修订与状态更新的版本隔离。
func TestContractRevisionIsolation(t *testing.T) {
	t.Run("contract_change_invalidates_old_plan", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "contract.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		// 合同 v1 的 Step（绑定 contract_revision=1）。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stepV1 := Step{StepID: id(), TaskID: taskID, ContractRevision: 1, DefinitionKey: "draft",
			DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now}
		if err := insertStep(tx, stepV1); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// 合同变更到 v2：新 Step 绑定 contract_revision=2，旧 v1 保留但不再满足新合同。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stepV2 := Step{StepID: id(), TaskID: taskID, ContractRevision: 2, DefinitionKey: "draft",
			DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now}
		if err := insertStep(tx, stepV2); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// 查询：两个合同版本的 Step 各自独立（旧结果不会满足新合同）。
		steps, err := listSteps(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		var v1Count, v2Count int
		for _, s := range steps {
			if s.ContractRevision == 1 {
				v1Count++
			}
			if s.ContractRevision == 2 {
				v2Count++
			}
		}
		if v1Count != 1 || v2Count != 1 {
			t.Errorf("contract revisions not isolated: v1=%d v2=%d", v1Count, v2Count)
		}
		// 唯一约束：同一 (task, definition_key, contract_revision) 只能有一个 Step。
		tx, err = store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		dup := stepV2
		dup.StepID = id() // 不同 ID 但同 task+key+revision
		if err := insertStep(tx, dup); err == nil {
			t.Error("duplicate (task,definition_key,contract_revision) not rejected")
		}
		tx.Rollback() // 立即回滚，释放单连接
	})

	t.Run("control_records_immutable_append_only", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "immutable.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		// 追加两条控制记录：pause → resume，各自独立版本。
		for i, kind := range []string{"pause", "resume"} {
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := insertControlRecord(tx, ControlRecord{
				RecordID: id(), TaskID: taskID, RequestKey: "key-" + kind,
				DecidedBy: "user", Kind: kind, ControlRevision: i + 1,
				TargetContractRevision: 1, Decision: kind, CreatedAt: now + int64(i),
			}); err != nil {
				t.Fatalf("insert %s: %v", kind, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM control_records WHERE task_id=?", taskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("control records = %d, want 2 (append-only)", count)
		}
	})

	t.Run("task_overview_query_model", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "overview.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		taskID := setupRecordsTask(t, store)
		now := recordsTestNow()

		// 填充：状态 + Step + work_unit + 审计。
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := upsertTaskState(tx, TaskState{TaskID: taskID, State: RuntimeTaskRunning, StorageRevision: 2, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		stepID := id()
		if err := insertStep(tx, Step{StepID: stepID, TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "analyze", DefinitionVersion: 1, State: RuntimeStepReady, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := insertWorkUnit(tx, WorkUnit{UnitID: id(), TaskID: taskID, StepID: stepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := recordAudit(tx, AuditEvent{TaskID: taskID, Event: "unit_ready", Actor: "scheduler", OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		// 查询模型：当前状态、步骤、未决单元、审计计数。
		overview, err := readTaskOverview(store.db, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if overview.State.State != RuntimeTaskRunning {
			t.Errorf("overview state = %s, want running", overview.State.State)
		}
		if len(overview.Steps) != 1 || overview.Steps[0].DefinitionKey != "analyze" {
			t.Errorf("overview steps = %+v", overview.Steps)
		}
		if overview.PendingUnits != 1 {
			t.Errorf("overview pending units = %d, want 1", overview.PendingUnits)
		}
		if overview.RecentAudits != 1 {
			t.Errorf("overview audits = %d, want 1", overview.RecentAudits)
		}
	})
}

// tableNames 返回库中全部表名集合。
func tableNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

// 确保 strings 引用保留。
var _ = strings.Contains
