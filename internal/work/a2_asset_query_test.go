package work

// A2+A3 验收：只读领域任务闭环——查询某资产的验收状态。
//
// 覆盖验收点：
//   - 提交明确意图 → V3 Task/Step → work_unit 领取 → 执行 → 结果落盘 → 查询回显；
//   - 幂等：同请求键返回同一任务；结果回执重复送达只应用一次；
//   - 重启恢复：claimed/running 单元置回 ready 可重新领取；
//   - 迟到回报（旧代际）留证但不重复完成；
//   - 只读：查询不修改领域状态。

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setupA2 创建带一个资产的存储，并注册领域查询处理器。
func setupA2(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	nowV := now()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "192.168.1.1", State: "待交付", Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, nowV)
	recordAcceptanceCoverage(tx, AcceptanceCoverage{RequirementKey: "delivery-ready", ObjectRef: "node-1", CoverageState: "covered"}, nowV)
	// 授予 user:session 对 node-1 与 node-unknown 的读取权限（A2 只读查询对象级鉴权）。
	grantAuthorization(tx, Authorization{Subject: "user:session", Action: "read", ObjectRef: "node-1", ValidFrom: nowV, Owner: "ops"}, nowV)
	grantAuthorization(tx, Authorization{Subject: "user:session", Action: "read", ObjectRef: "node-unknown", ValidFrom: nowV, Owner: "ops"}, nowV)
	tx.Commit()
	return store
}

func TestA2AssetAcceptanceQueryClosedLoop(t *testing.T) {
	s := setupA2(t, "a2.db")
	ctx := context.Background()
	executor, err := s.EnableAssetAcceptanceQuery(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("submit_execute_query_result", func(t *testing.T) {
		// 提交明确只读查询。
		task, err := s.SubmitAssetAcceptanceQuery(ctx, "a2-req-1", "node-1", "user:session")
		if err != nil {
			t.Fatal(err)
		}
		if task.TaskID == "" || task.Duplicate {
			t.Fatalf("submit: %+v", task)
		}
		// 执行一个单元（领取 → 执行 → 落盘 → 完成）。
		worked, err := executor.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			t.Fatal("no ready unit claimed")
		}
		// 查询进度与结果。
		progress, err := s.ReadAssetAcceptanceQuery(ctx, task.TaskID, "")
		if err != nil {
			t.Fatal(err)
		}
		if !progress.Done {
			t.Errorf("task not done: %+v", progress)
		}
		if progress.Result == nil {
			t.Fatal("no persisted result (任务完成必须有持久结果)")
		}
		if progress.Result.ObjectRef != "node-1" || !progress.Result.Found {
			t.Errorf("result: %+v", progress.Result)
		}
		if progress.Result.State != "待交付" {
			t.Errorf("asset state=%s, want 待交付", progress.Result.State)
		}
		if progress.Result.Coverage["delivery-ready"] != "covered" {
			t.Errorf("coverage=%v, want delivery-ready covered", progress.Result.Coverage)
		}
	})

	t.Run("same_request_key_idempotent", func(t *testing.T) {
		// 同请求键：返回同一任务（幂等）。
		first, _ := s.SubmitAssetAcceptanceQuery(ctx, "a2-req-2", "node-1", "user:session")
		second, err := s.SubmitAssetAcceptanceQuery(ctx, "a2-req-2", "node-1", "user:session")
		if err != nil {
			t.Fatal(err)
		}
		if second.TaskID != first.TaskID || !second.Duplicate {
			t.Errorf("same key not idempotent: %s vs %s", first.TaskID, second.TaskID)
		}
	})

	t.Run("query_is_read_only", func(t *testing.T) {
		// 只读：查询不修改领域状态（revision 不变）。
		before, _ := readAsset(s.db, "node-1")
		task, _ := s.SubmitAssetAcceptanceQuery(ctx, "a2-req-3", "node-1", "user:session")
		executor.RunOnce(ctx)
		after, _ := readAsset(s.db, "node-1")
		if after.Revision != before.Revision || after.State != before.State {
			t.Errorf("read query mutated asset: before=%+v after=%+v (只读查询不应改领域状态)", before, after)
		}
		_ = task
	})

	t.Run("unknown_asset_reports_not_found", func(t *testing.T) {
		task, _ := s.SubmitAssetAcceptanceQuery(ctx, "a2-req-4", "node-unknown", "user:session")
		// 循环消费直到目标任务完成（前面子测试可能残留其他 ready 单元）。
		for i := 0; i < 10; i++ {
			executor.RunOnce(ctx)
			progress, _ := s.ReadAssetAcceptanceQuery(ctx, task.TaskID, "")
			if progress.Done {
				if progress.Result == nil || progress.Result.Found {
					t.Errorf("unknown asset should report not found: %+v", progress.Result)
				}
				return
			}
		}
		t.Error("unknown asset task not completed after consuming backlog")
	})
}

// TestA2RestartRecovery 重启后 claimed 单元置回 ready 可重新领取。
func TestA2RestartRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "a2-restart.db")
	var taskID string
	// 第一次启动：提交并领取（但不完成，模拟中断）。
	func() {
		s, err := Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		nowV := now()
		tx, _ := s.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: "在管", Revision: 1, OwnershipEpoch: 1}, nowV)
		grantAuthorization(tx, Authorization{Subject: "user:session", Action: "read", ObjectRef: "node-1", ValidFrom: nowV, Owner: "ops"}, nowV)
		tx.Commit()
		registerAssetAcceptanceHandler(s)
		task, err := s.SubmitAssetAcceptanceQuery(context.Background(), "a2-restart", "node-1", "user:session")
		if err != nil {
			t.Fatal(err)
		}
		taskID = task.TaskID
		// 领取但不完成（模拟执行中断）。
		executor, _ := s.StartEmbeddedExecutor(context.Background())
		tx2, _ := s.db.Begin()
		_, err = claimWorkUnit(tx2, ClaimRequest{WorkerID: executor.workerID, Credential: executor.credential, Kind: WorkUnitDeterministic, LeaseTTL: 60000, QuotaAvailable: true}, now())
		if err != nil {
			t.Fatal(err)
		}
		tx2.Exec("UPDATE work_units SET state='claimed' WHERE task_id=?", taskID)
		tx2.Commit()
		// Close 模拟中断（claimed 单元未完成）。
	}()
	// 重启：RecoverV3 把 claimed 置回 ready。
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.RecoverV3(context.Background(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// 单元已置回 ready，可重新领取并完成。
	var unitState string
	if err := s2.db.QueryRow("SELECT state FROM work_units WHERE task_id=?", taskID).Scan(&unitState); err != nil {
		t.Fatal(err)
	}
	if unitState != "ready" {
		t.Errorf("after restart unit state=%s, want ready (重启后待分类/执行单元恢复)", unitState)
	}
	executor2, err := s2.EnableAssetAcceptanceQuery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := executor2.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("restart execute: worked=%v err=%v (重启后应能重新领取执行)", worked, err)
	}
	progress, _ := s2.ReadAssetAcceptanceQuery(context.Background(), taskID, "")
	if !progress.Done || progress.Result == nil || !progress.Result.Found {
		t.Errorf("restart result: done=%v result=%+v (重启后任务应完成且有持久结果)", progress.Done, progress.Result)
	}
}

// TestA2LateReport 迟到回报（重复送达）只应用一次；同身份不同结果隔离。
func TestA2LateReport(t *testing.T) {
	s := setupA2(t, "a2-late.db")
	ctx := context.Background()
	executor, _ := s.EnableAssetAcceptanceQuery(ctx)
	task, _ := s.SubmitAssetAcceptanceQuery(ctx, "a2-late", "node-1", "user:session")
	executor.RunOnce(ctx)
	nowV := now()
	var unitID, appliedSummary string
	var leaseGen int
	tx0, _ := s.db.Begin()
	tx0.QueryRow("SELECT unit_id, lease_generation FROM work_units WHERE task_id=?", task.TaskID).Scan(&unitID, &leaseGen)
	tx0.QueryRow("SELECT summary FROM result_receipts WHERE task_id=? AND state='applied' ORDER BY applied_at DESC LIMIT 1", task.TaskID).Scan(&appliedSummary)
	tx0.Commit()
	resultKey := unitID + ":" + strconv.Itoa(leaseGen)

	// 重复送达相同内容（迟到回报）：幂等返回既有回执，不新建。
	tx, _ := s.db.Begin()
	existing, isNew, err := recordReport(tx, ResultReport{
		ResultKey: resultKey, UnitID: unitID, TaskID: task.TaskID,
		ExecutionGeneration: leaseGen, LeaseGeneration: leaseGen, Summary: appliedSummary, ReceivedAt: nowV,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Error("重复送达相同内容创建了新回执（应只应用一次）")
	}
	if existing.State != "applied" {
		t.Errorf("重复回执状态=%s，应已应用", existing.State)
	}
	// 重复应用：applyReport 对已 applied 结果返回 Duplicate。
	applyRes, err := applyReport(tx, resultKey, leaseGen, nil, nowV)
	if err != nil {
		t.Fatal(err)
	}
	if !applyRes.Duplicate {
		t.Error("重复应用未识别为 Duplicate（重复送达只应用一次）")
	}
	// 同身份不同结果：隔离（不覆盖原结果）。
	_, _, err = recordReport(tx, ResultReport{
		ResultKey: resultKey, UnitID: unitID, TaskID: task.TaskID,
		ExecutionGeneration: leaseGen, Summary: "不同内容", ReceivedAt: nowV,
	})
	if err == nil {
		t.Error("同身份不同结果未隔离（不应覆盖原结果）")
	}
	tx.Commit()
	// 任务结果保持原样（不被迟到回报覆盖）。
	progress, _ := s.ReadAssetAcceptanceQuery(ctx, task.TaskID, "")
	if !progress.Done {
		t.Error("迟到回报破坏完成状态")
	}
	if progress.Summary != appliedSummary {
		t.Errorf("结果被覆盖：%q ≠ %q", progress.Summary, appliedSummary)
	}
}

// TestA2UnauthorizedQueryDenied 未授权对象拒绝且不泄露存在性（P0-1）。
func TestA2UnauthorizedQueryDenied(t *testing.T) {
	s := setupA2(t, "a2-unauth.db")
	ctx := context.Background()
	s.EnableAssetAcceptanceQuery(ctx)
	// user:other 无 node-1 读权限：提交即拒绝，且不区分对象是否存在（不泄露存在性）。
	_, err := s.SubmitAssetAcceptanceQuery(ctx, "a2-unauth-1", "node-1", "user:other")
	if err == nil {
		t.Fatal("unauthorized query not denied")
	}
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Errorf("err=%v, want ErrAuthorizationDenied", err)
	}
	// 错误信息不区分"对象不存在"与"无权"（不泄露存在性）。
	if !strings.Contains(err.Error(), "不存在或无权访问") {
		t.Errorf("error leaks existence: %v", err)
	}
}

func TestA2NewLeaseResultSurvivesOldReport(t *testing.T) {
	s := setupA2(t, "lease-generation.db")
	ctx := t.Context()
	executor, err := s.EnableAssetAcceptanceQuery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.SubmitAssetAcceptanceQuery(ctx, "a2-generation", "node-1", "user:session")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	old, err := claimWorkUnit(tx, ClaimRequest{WorkerID: executor.workerID, Credential: executor.credential, Kind: WorkUnitDeterministic, LeaseTTL: 30000, QuotaAvailable: true}, now())
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverV3(ctx, now()); err != nil {
		t.Fatal(err)
	}
	if worked, err := executor.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("new lease: %v %v", worked, err)
	}
	if err := executor.completeClaimed(ctx, old, DeterministicResult{Summary: "old", OutputRef: "old"}, nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.ReadAssetAcceptanceQuery(ctx, task.TaskID, "user:session")
	if err != nil || p.Result == nil || p.Summary == "old" || p.State != "succeeded" {
		t.Fatalf("current result: %+v, %v", p, err)
	}
}

func TestA2HistoricalHandoverIsNotCurrentService(t *testing.T) {
	s := setupA2(t, "historical-service.db")
	if _, err := s.db.Exec(`INSERT INTO delivery_records(delivery_id,object_ref,stage,occurred_at,created_at)
VALUES('old','node-1','in_service',1,1)`); err != nil {
		t.Fatal(err)
	}
	p, err := queryAssetAcceptance(s.db, "node-1")
	if err != nil || p.InService || p.State != "待交付" || len(p.DeliveryStages) != 1 {
		t.Fatalf("current and historical status: %+v %v", p, err)
	}
}
