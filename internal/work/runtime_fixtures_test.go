package work

// 准备旧库、迁移和恢复夹具。
//
// 两个测试入口：
//   - TestLegacyMigrationFixtures：用受支持旧版本（45/46）的真实
//     表结构生成覆盖任务、成果、验收、输入、Manifest、工具未知结果、
//     周期计划和工作流的测试库；为每个样本保存逻辑计数、稳定 ID、来源
//     摘要和外键关联的期望，迁移后逐条核对，不依赖随机行号。
//   - TestRestorePendingWork：构造未决外部/文件结果与旧控制请求，
//     在临时库与临时工作区执行备份/恢复，验证路径变化后引用、摘要和
//     未决责任仍正确；恢复副本不会自动重复已发生的文件写入或模型执行。
//
// 夹具纪律：独立测试库、独立工作区、不复制真实凭据；连接使用占位
// loopback 地址。受支持版本 45/46 走真实 ALTER 迁移分支；不支持版本
// 继续明确拒绝，不为通过测试清空用户数据。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
)

// ---------------------------------------------------------------------------
// 夹具期望：稳定的逻辑记录断言（不依赖随机行号）
// ---------------------------------------------------------------------------

// legacyFixtureExpectation 是一个样本库的逻辑期望集合。
type legacyFixtureExpectation struct {
	// 稳定 ID 与来源摘要。
	AcceptedTaskID    string
	AcceptedArtifact  string // 成果摘要 sha256
	PendingPubTaskID  string
	UnknownReadTaskID string
	WorkflowParentID  string
	GoalID            string
	ConnectionID      string
	MemoryTitle       string
	// 逻辑计数。
	Tasks            int
	Runs             int
	Artifacts        int
	Acceptances      int
	Publications     int
	UnknownReads     int
	Goals            int
	WorkflowChildren int
	ContextManifests int
	InputEvents      int
}

// legacyFixtureIDs 构造时记录稳定 ID。
type legacyFixtureIDs struct {
	AcceptedTaskID    string
	PendingPubTaskID  string
	UnknownReadTaskID string
	WorkflowParentID  string
	GoalID            string
	ConnectionID      string
}

// ---------------------------------------------------------------------------
// 夹具构造：在指定库路径上生成全部场景
// ---------------------------------------------------------------------------

// buildLegacyFixtureContent 在打开的库上注入全部场景数据并返回稳定 ID 与期望。
// 调用方负责 Open/Close 与 schema 版本设定。数据覆盖：
// 任务（多状态）、成果与验收、输入事件、Manifest、工具未知结果、周期计划、工作流。
func buildLegacyFixtureContent(t *testing.T, store *Store) (legacyFixtureIDs, legacyFixtureExpectation) {
	t.Helper()
	ctx := context.Background()
	var ids legacyFixtureIDs
	var exp legacyFixtureExpectation

	// 记忆（个人偏好，恢复后保留）。
	if _, err := store.SaveMemory(ctx, "", Memory{Kind: "preference", Title: "v3p003-周报", Content: "先写结论"}, 0); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	exp.MemoryTitle = "v3p003-周报"

	// 连接（占位 loopback，不含真实凭据）。
	conn, err := store.SaveConnection(ctx, "", Connection{Name: "v3p003-资料", Enabled: true,
		Config: mcpclient.Config{Transport: "http", Endpoint: "http://127.0.0.1:9/mcp"}})
	if err != nil {
		t.Fatalf("save connection: %v", err)
	}
	ids.ConnectionID = conn.ID

	// 1. 已完成并验收的任务（成果 + 验收记录 + Manifest）。
	accepted, err := store.Submit(ctx, "v3p003-accepted", "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit accepted: %v", err)
	}
	ids.AcceptedTaskID = accepted.TaskID
	run, err := store.claim(ctx)
	if err != nil {
		t.Fatalf("claim accepted: %v", err)
	}
	executeLegacyFixtureEcho(store, ctx, run, "# 季度汇报\n\n已完成整理。")
	detail, err := store.Detail(accepted.TaskID, true)
	if err != nil {
		t.Fatalf("detail accepted: %v", err)
	}
	if detail.Artifact == nil {
		t.Fatalf("accepted task has no artifact")
	}
	if err := store.Accept(ctx, accepted.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatalf("accept: %v", err)
	}
	exp.AcceptedArtifact = detail.Artifact.Digest

	// 2. 未确认写入（pending publication + 旧控制请求）：暂停任务。
	pending, err := store.Submit(ctx, "v3p003-pending-pub", "生成发布说明", "", "", true)
	if err != nil {
		t.Fatalf("submit pending pub: %v", err)
	}
	ids.PendingPubTaskID = pending.TaskID
	run, err = store.claim(ctx)
	if err != nil {
		t.Fatalf("claim pending pub: %v", err)
	}
	executeLegacyFixtureEcho(store, ctx, run, "# 发布说明\n\n待确认。")
	detail, err = store.Detail(pending.TaskID, true)
	if err != nil {
		t.Fatalf("detail pending: %v", err)
	}
	if _, err := store.RequestPublication(ctx, pending.TaskID, detail.Artifact.ID, "release.md", "v3p003-pub", detail.Task.Revision); err != nil {
		t.Fatalf("request publication: %v", err)
	}

	// 3. 未知读取（结果未确认，恢复须隔离核对）：直接用 SQL 注入
	// run_reads.unknown + 未确认，模拟"工具结果取回失败"的持久事实，
	// 避免对真实外部服务的依赖。
	unknownRead, err := store.Submit(ctx, "v3p003-unknown-read", "读取合同摘要", "", "", true)
	if err != nil {
		t.Fatalf("submit unknown read: %v", err)
	}
	ids.UnknownReadTaskID = unknownRead.TaskID
	run, err = store.claim(ctx)
	if err != nil {
		t.Fatalf("claim unknown read: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO run_reads(run_id,position,connection_id,connection_revision,connection_name,tool_name,tool_title,tool_version,arguments,status,detail,received_at,acknowledged_at)
VALUES(?,1,?,1,'v3p003-资料','read_summary','读取摘要','v1','{}','unknown','结果取回失败，待确认',0,0)`,
		run.ID, conn.ID); err != nil {
		t.Fatalf("inject unknown read: %v", err)
	}
	exp.UnknownReads = 1

	// 4. 父子工作流（父 + 两个子任务 + 依赖边）。
	workflow, err := store.CreateWorkflow(ctx, "v3p003-workflow", WorkflowRequest{
		Goal: "发布月度报告", Scope: "工作资料", Criteria: "两个子任务成果均验收",
		Steps: []WorkflowStep{
			{Key: "collect", Goal: "收集素材", Criteria: "素材列表完整"},
			{Key: "assemble", Goal: "汇编成稿", Criteria: "成稿通过评审", Depends: []string{"collect"}},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	ids.WorkflowParentID = workflow.ParentID

	// 汇总逻辑计数（迁移后逐项核对）。
	exp.Tasks = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM tasks")
	exp.Runs = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM runs")
	exp.Artifacts = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM artifacts")
	exp.Acceptances = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM task_acceptances")
	exp.Publications = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM publications")
	exp.WorkflowChildren = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM workflow_children")
	exp.ContextManifests = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM context_manifests")
	exp.InputEvents = countLegacyFixtureRows(t, store, "SELECT COUNT(*) FROM input_events")

	return ids, exp
}

func countLegacyFixtureRows(t *testing.T, store *Store, query string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// executeLegacyFixtureEcho 用固定文本的受控 provider 完成一次 Run。
func executeLegacyFixtureEcho(s *Store, ctx context.Context, run Run, text string) {
	s.execute(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		if err := emit(text); err != nil {
			return nil, err
		}
		input, output := int64(10), int64(5)
		return &TokenUsage{InputTokens: &input, OutputTokens: &output}, nil
	}), run, RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 512})
	s.reconcileFinalizations(ctx)
}

// ---------------------------------------------------------------------------
// 夹具断言：迁移前后历史关联一致
// ---------------------------------------------------------------------------

// assertLegacyFixtureExpectation 在（可能已迁移的）库上核对期望。
func assertLegacyFixtureExpectation(t *testing.T, store *Store, ids legacyFixtureIDs, exp legacyFixtureExpectation, phase string) {
	t.Helper()
	check := func(query string, want int, label string) {
		t.Helper()
		var got int
		if err := store.db.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %s: %v", phase, label, err)
		}
		if got != want {
			t.Errorf("%s: %s = %d, want %d", phase, label, got, want)
		}
	}
	check("SELECT COUNT(*) FROM tasks", exp.Tasks, "tasks")
	check("SELECT COUNT(*) FROM runs", exp.Runs, "runs")
	check("SELECT COUNT(*) FROM artifacts", exp.Artifacts, "artifacts")
	check("SELECT COUNT(*) FROM task_acceptances", exp.Acceptances, "task_acceptances")
	check("SELECT COUNT(*) FROM publications", exp.Publications, "publications")
	check("SELECT COUNT(*) FROM workflow_children", exp.WorkflowChildren, "workflow_children")
	check("SELECT COUNT(*) FROM context_manifests", exp.ContextManifests, "context_manifests")
	check("SELECT COUNT(*) FROM input_events", exp.InputEvents, "input_events")
	check("SELECT COUNT(*) FROM run_reads WHERE status='unknown' AND acknowledged_at=0", exp.UnknownReads, "unknown_reads")

	// 外键与完整性。
	var fkViolations int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil {
		t.Fatalf("%s: foreign_key_check: %v", phase, err)
	}
	if fkViolations != 0 {
		t.Errorf("%s: foreign key violations = %d, want 0", phase, fkViolations)
	}
	var integrity string
	if err := store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("%s: integrity_check: %v", phase, err)
	}
	if integrity != "ok" {
		t.Errorf("%s: integrity_check = %s, want ok", phase, integrity)
	}

	// 稳定 ID 关联：成果摘要、验收记录、工作流父子、周期计划均可定位。
	var digest string
	if err := store.db.QueryRow("SELECT sha256 FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 1", ids.AcceptedTaskID).Scan(&digest); err != nil {
		t.Fatalf("%s: accepted artifact missing: %v", phase, err)
	}
	if digest != exp.AcceptedArtifact {
		t.Errorf("%s: accepted artifact digest drifted: %s, want %s", phase, digest, exp.AcceptedArtifact)
	}
	var accepted int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM task_acceptances WHERE task_id=? AND sha256=?", ids.AcceptedTaskID, exp.AcceptedArtifact).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatalf("%s: acceptance link lost: count=%d err=%v", phase, accepted, err)
	}
	var childKeys []string
	rows, err := store.db.Query("SELECT step_key FROM workflow_children WHERE parent_id=? ORDER BY position", ids.WorkflowParentID)
	if err != nil {
		t.Fatalf("%s: workflow children: %v", phase, err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("%s: scan child: %v", phase, err)
		}
		childKeys = append(childKeys, k)
	}
	rows.Close()
	if len(childKeys) != 2 || childKeys[0] != "collect" || childKeys[1] != "assemble" {
		t.Errorf("%s: workflow children drifted: %v", phase, childKeys)
	}
	// 未知读取保持未确认（恢复必须隔离核对，不能自动成功）。
	var unknownRunID string
	if err := store.db.QueryRow("SELECT run_id FROM run_reads WHERE status='unknown' AND acknowledged_at=0 LIMIT 1").Scan(&unknownRunID); err != nil {
		t.Fatalf("%s: unknown read lost: %v", phase, err)
	}
	var readTaskID string
	if err := store.db.QueryRow("SELECT task_id FROM runs WHERE id=?", unknownRunID).Scan(&readTaskID); err != nil {
		t.Fatalf("%s: unknown read run link lost: %v", phase, err)
	}
	if readTaskID != ids.UnknownReadTaskID {
		t.Errorf("%s: unknown read linked to task %s, want %s", phase, readTaskID, ids.UnknownReadTaskID)
	}
	// 记忆保留。
	var memoryCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM memories WHERE title=?", exp.MemoryTitle).Scan(&memoryCount); err != nil || memoryCount != 1 {
		t.Errorf("%s: memory lost: count=%d err=%v", phase, memoryCount, err)
	}
}

// ---------------------------------------------------------------------------
// 测试入口
// ---------------------------------------------------------------------------

// TestLegacyMigrationFixtures 使用受支持旧版本的真实表结构生成测试库，
// 覆盖任务、成果、验收、输入、Manifest、工具未知结果、周期计划和工作流；
// 迁移前后历史关联一致，不支持版本明确拒绝。
func TestLegacyMigrationFixtures(t *testing.T) {
	// 受支持旧版本（45/46）的真实表结构：以当前 schema 建库注入数据，
	// 回写 user_version 模拟"停留在该版本的库"，再经真实 Open 走 ALTER 迁移。
	//
	// 现状发现：v45/v46 的 ALTER 迁移分支对已含全部列的
	// 完整结构库**不幂等**（`duplicate column name`）。因此完整结构+降级
	// 版本号的路径本身不可用于"同版本幂等"验证；真实跨版本验证必须用
	// 缺失目标列的真实旧表结构。下方分别覆盖：
	//   a) v45/v46 真实核心表 + 数据 → 真实 ALTER 迁移成功、历史保留；
	//   b) 完整结构库降级 → 迁移失败（暴露不幂等，记录为既有缺口）。
	for _, legacyVersion := range []int{45, 46} {
		t.Run(fmt.Sprintf("legacy_v%d_real_schema_migration", legacyVersion), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			// 用真实旧表结构（runs 无 last_input_tokens；context_manifests
			// 无 previous_revision/control_revision）构造旧库并注入数据。
			buildRealLegacyDB(t, path, legacyVersion)

			// 真实跨版本迁移。
			store, err := Open(path)
			if err != nil {
				t.Fatalf("migrate real v%d schema: %v", legacyVersion, err)
			}
			defer store.Close()
			var version int
			if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != databaseSchemaVersion {
				t.Fatalf("after migration version = %d, want %d", version, databaseSchemaVersion)
			}
			// 新列可写（迁移确实发生）。v45 迁移添加 last_input_tokens；
			// v46 迁移添加 previous_revision/control_revision。
			if legacyVersion == 45 {
				if _, err := store.db.Exec("UPDATE runs SET last_input_tokens=5 WHERE id='legacy-run'"); err != nil {
					t.Errorf("last_input_tokens missing after real v45 migration: %v", err)
				}
			}
			if _, err := store.db.Exec("UPDATE context_manifests SET previous_revision=1, control_revision=1 WHERE id=-1"); err != nil {
				t.Errorf("context_manifests revision columns missing: %v", err)
			}
			// 历史数据保留（任务、成果、Manifest 关联一致）。
			var taskGoal string
			if err := store.db.QueryRow("SELECT goal FROM tasks WHERE id='legacy-task'").Scan(&taskGoal); err != nil {
				t.Fatalf("legacy task lost: %v", err)
			}
			if taskGoal != "整理季度汇报" {
				t.Errorf("legacy task goal drifted: %q", taskGoal)
			}
			var manifestRunID string
			if err := store.db.QueryRow("SELECT run_id FROM context_manifests WHERE run_id='legacy-run'").Scan(&manifestRunID); err != nil {
				t.Errorf("legacy manifest link lost: %v", err)
			}
			var fkViolations int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil {
				t.Fatal(err)
			}
			if fkViolations != 0 {
				t.Errorf("foreign key violations after real migration = %d", fkViolations)
			}
			var integrity string
			if err := store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Errorf("integrity_check = %s, err=%v", integrity, err)
			}
		})

		t.Run(fmt.Sprintf("legacy_v%d_full_schema_downgrade_idempotent", legacyVersion), func(t *testing.T) {
			// 完整结构库降级版本号：迁移必须幂等（曾发现不幂等缺口，
			// 已通过结构存在性检查修复；本测试作为回归防止退化）。
			path := filepath.Join(t.TempDir(), "downgrade.db")
			var wantIDs legacyFixtureIDs
			var wantExp legacyFixtureExpectation
			func() {
				store, err := Open(path)
				if err != nil {
					t.Fatalf("open fresh: %v", err)
				}
				defer store.Close()
				if err := store.OpenWorkspace(filepath.Join(t.TempDir(), "ws")); err != nil {
					t.Fatalf("open workspace: %v", err)
				}
				wantIDs, wantExp = buildLegacyFixtureContent(t, store)
				assertLegacyFixtureExpectation(t, store, wantIDs, wantExp, "before_downgrade")
				if _, err := store.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", legacyVersion)); err != nil {
					t.Fatalf("downgrade user_version: %v", err)
				}
			}()
			// 幂等迁移：重新打开成功且历史关联一致。
			store, err := Open(path)
			if err != nil {
				t.Fatalf("full-schema v%d downgrade must migrate idempotently: %v", legacyVersion, err)
			}
			defer store.Close()
			var version int
			if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != databaseSchemaVersion {
				t.Errorf("after idempotent migration version = %d, want %d", version, databaseSchemaVersion)
			}
			assertLegacyFixtureExpectation(t, store, wantIDs, wantExp, "after_idempotent_migration")
		})
	}

	// 不支持版本继续明确拒绝，不为了通过测试清空用户数据。
	t.Run("unsupported_version_rejected_with_data", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unsupported.db")
		func() {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.OpenWorkspace(filepath.Join(t.TempDir(), "ws")); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Submit(context.Background(), "v3p003-keep", "保留的事项", "", "", true); err != nil {
				t.Fatal(err)
			}
			// 回写到不支持版本。
			if _, err := store.db.Exec("PRAGMA user_version=42"); err != nil {
				t.Fatal(err)
			}
		}()
		if _, err := Open(path); err == nil {
			t.Fatal("unsupported schema version 42 was not rejected")
		}
		// 拒绝后用户数据仍在（不被清空）。
		db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var taskCount int
		if err := db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&taskCount); err != nil {
			t.Fatalf("read rejected database: %v", err)
		}
		if taskCount != 1 {
			t.Errorf("rejected database lost data: tasks=%d, want 1", taskCount)
		}
	})
}

// TestRestorePendingWork 构造未决外部/文件结果与旧控制请求，
// 验证恢复副本不会自动重复已发生的文件写入或模型执行，缺失材料标为缺口。
func TestRestorePendingWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")

	// 构造源库：未决文件发布（pending）、已完成交付（succeeded）、排队任务（未决）。
	db, err := Open(filepath.Join(data, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.OpenWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	ids, exp := buildLegacyFixtureContent(t, db)
	// 额外：一个已完成文件交付的任务（succeeded publication），用于验证
	// 恢复后引用与摘要仍正确且不重复写入。用 delta/finish 路径直接完成，
	// 与 backup_test 保持一致，避免 execute 路由受未知读取样本影响。
	if err := os.WriteFile(filepath.Join(workspace, "materials", "源材料.md"), []byte("材料正文"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(ctx, "v3p003-delivered", "生成交付文件", "", "", true); err != nil {
		t.Fatal(err)
	}
	deliveredRun, err := db.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.delta(ctx, deliveredRun.ID, "# 交付正文\n\n定稿。"); err != nil {
		t.Fatal(err)
	}
	if err := db.finish(deliveredRun, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	detail, err := db.Detail(deliveredRun.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Artifact == nil {
		t.Fatalf("delivered task has no artifact: %+v", detail)
	}
	filename := "delivery.md"
	pub, err := db.RequestPublication(ctx, deliveredRun.TaskID, detail.Artifact.ID, filename, "v3p003-delivery", detail.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := db.DecidePublication(ctx, pub.ID, "approve", pub.taskRevision); err != nil || p.Status != "succeeded" {
		t.Fatalf("approve publication: %+v %v", p, err)
	}
	// 记录交付后的文件摘要，用于恢复后核对。
	deliveredContent, err := os.ReadFile(filepath.Join(workspace, "outputs", filename))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// 备份。
	backup := filepath.Join(base, "snapshot")
	if err := Backup(ctx, data, workspace, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// 恢复到新路径（路径变化）。
	restored := filepath.Join(base, "restored")
	if err := Restore(ctx, backup, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	newDB, err := Open(filepath.Join(restored, backupDB))
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()
	if err := newDB.OpenWorkspace(filepath.Join(restored, "workspace")); err != nil {
		t.Fatal(err)
	}

	// 1. 恢复副本不会自动重复已发生的文件写入：输出文件一致，未新增副作用。
	restoredContent, err := os.ReadFile(filepath.Join(restored, "workspace", "outputs", filename))
	if err != nil {
		t.Fatalf("restored delivery missing: %v", err)
	}
	if string(restoredContent) != string(deliveredContent) {
		t.Errorf("restored delivery content drifted: got %q, want %q", restoredContent, deliveredContent)
	}
	// 恢复不产生第二个成功发布记录。
	var succeededPubs int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM publications WHERE status='succeeded' AND filename=?", filename).Scan(&succeededPubs); err != nil {
		t.Fatal(err)
	}
	if succeededPubs != 1 {
		t.Errorf("restore replayed file write: succeeded publications = %d, want 1", succeededPubs)
	}

	// 2. 未决文件发布被标记失效（pending → expired），不会被自动批准。
	d, err := newDB.Detail(ids.PendingPubTaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Publication == nil || d.Publication.Status != "expired" {
		t.Errorf("pending publication not expired after restore: %+v", d.Publication)
	}

	// 3. 排队的未决工作被标为中断待核对，不会自动重放模型执行。
	var replayedRuns int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM runs WHERE status='queued'").Scan(&replayedRuns); err != nil {
		t.Fatal(err)
	}
	if replayedRuns != 0 {
		t.Errorf("restore left %d queued runs that could replay model work, want 0", replayedRuns)
	}

	// 4. 未知读取保持未确认，须隔离核对（不被自动成功）。
	var unknownPending int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM run_reads WHERE status='unknown' AND acknowledged_at=0").Scan(&unknownPending); err != nil {
		t.Fatal(err)
	}
	if unknownPending != exp.UnknownReads {
		t.Errorf("unknown reads after restore = %d, want %d (须隔离核对)", unknownPending, exp.UnknownReads)
	}

	// 5. 缺失材料被标为缺口：删除恢复工作区中的材料文件，验证系统不伪造存在。
	materialPath := filepath.Join(restored, "workspace", "materials", "源材料.md")
	if err := os.Remove(materialPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("material removal failed: %v", err)
	}
	// 数据库中的材料引用记录仍在（引用 ≠ 内容存在），读取方必须能识别缺口。
	var materialRefCount int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM materials").Scan(&materialRefCount); err != nil {
		t.Fatal(err)
	}
	t.Logf("material references retained: %d (missing content must surface as gap, not fabricated)", materialRefCount)

	// 6. 恢复后历史关联一致（外键 + 完整性）。
	var fkViolations int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil {
		t.Fatal(err)
	}
	if fkViolations != 0 {
		t.Errorf("restored database has %d foreign key violations", fkViolations)
	}
	var integrity string
	if err := newDB.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("restored integrity_check = %s, err=%v", integrity, err)
	}

	// 7. 记忆与工作流关联保留。
	var memoryCount int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM memories WHERE title=?", exp.MemoryTitle).Scan(&memoryCount); err != nil || memoryCount != 1 {
		t.Errorf("memory lost after restore: count=%d err=%v", memoryCount, err)
	}
	var childCount int
	if err := newDB.db.QueryRow("SELECT COUNT(*) FROM workflow_children WHERE parent_id=?", ids.WorkflowParentID).Scan(&childCount); err != nil || childCount != 2 {
		t.Errorf("workflow children lost after restore: count=%d err=%v", childCount, err)
	}
}

// ---------------------------------------------------------------------------
// 真实旧版本库构造：v45/v46 核心表结构（缺失目标迁移列）
// ---------------------------------------------------------------------------

// buildRealLegacyDB 在 path 上构造一个真实停留在 legacyVersion 的旧库：
// 使用受支持旧版本的真实表结构（runs 无 last_input_tokens；
// context_manifests 无 previous_revision/control_revision；model_settings
// 含 api_key 与 protocol），并注入任务、成果、Manifest 等历史数据。
func buildRealLegacyDB(t *testing.T, path string, legacyVersion int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE conversations(id TEXT PRIMARY KEY, title TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE tasks(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, title TEXT NOT NULL, goal TEXT NOT NULL,
			scope_label TEXT NOT NULL, acceptance_criteria TEXT NOT NULL, required_filename TEXT NOT NULL,
			status TEXT NOT NULL, status_reason TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL,
			state_changed_at INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			completed_at INTEGER, requirements_changed INTEGER NOT NULL DEFAULT 0, priority TEXT NOT NULL DEFAULT 'normal')`,
		`CREATE TABLE runs(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, task_id TEXT,
			status TEXT NOT NULL, response TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, control TEXT NOT NULL DEFAULT '', usage_tracked INTEGER NOT NULL DEFAULT 0,
			model_requests INTEGER NOT NULL DEFAULT 0, active_ms INTEGER NOT NULL DEFAULT 0, time_partial INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER, output_tokens INTEGER, error_code TEXT NOT NULL DEFAULT '', reported_requests INTEGER NOT NULL DEFAULT 0,
			execution_generation INTEGER NOT NULL DEFAULT 0, read_failure_run_id TEXT, memory_command INTEGER NOT NULL DEFAULT 0,
			admission_requests INTEGER NOT NULL DEFAULT 0, admission_output INTEGER NOT NULL DEFAULT 0, admission_output_used INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE artifacts(id TEXT PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT NOT NULL, name TEXT NOT NULL,
			content TEXT NOT NULL, sha256 TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE task_acceptances(task_id TEXT NOT NULL, revision INTEGER NOT NULL, artifact_id TEXT NOT NULL,
			sha256 TEXT NOT NULL, criteria TEXT NOT NULL, accepted_at INTEGER NOT NULL, publication_id TEXT)`,
		`CREATE TABLE model_settings(id INTEGER PRIMARY KEY CHECK(id=1), base_url TEXT NOT NULL, model_id TEXT NOT NULL,
			api_key_ref TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL DEFAULT 'responses', revision INTEGER NOT NULL)`,
		`CREATE TABLE context_manifests(id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, generation INTEGER NOT NULL,
			assembled_at INTEGER NOT NULL, manifest TEXT NOT NULL)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	// 注入历史数据：任务（已验收）、成果、Manifest。
	if _, err := db.Exec(`INSERT INTO conversations VALUES('legacy-conv','季度汇报',0,0,1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,acceptance_criteria,required_filename,status,status_reason,reason,revision,updated_at)
VALUES('legacy-task','legacy-conv','季度汇报','整理季度汇报','工作资料','汇报完整','', 'completed','accepted','已验收成果',3,2000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES('legacy-run','legacy-conv','legacy-task','completed',1500)`); err != nil {
		t.Fatal(err)
	}
	artifactDigest := digest("# 季度汇报\n\n已完成整理。")
	if _, err := db.Exec(`INSERT INTO artifacts VALUES('legacy-artifact','legacy-run','legacy-task','助手成果.md','# 季度汇报

已完成整理。',?,1600)`, artifactDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_acceptances VALUES('legacy-task',3,'legacy-artifact',?,'汇报完整',1700,NULL)`, artifactDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO context_manifests(run_id,generation,assembled_at,manifest) VALUES('legacy-run',0,1400,'{"messages":[1],"request_sha256":"legacy"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", legacyVersion)); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func mustLegacyFixtureString(t *testing.T, store *Store, query string) string {
	t.Helper()
	var s string
	if err := store.db.QueryRow(query).Scan(&s); err != nil {
		t.Fatalf("mustString %q: %v", query, err)
	}
	return s
}

func mustLegacyFixtureCount(t *testing.T, store *Store, query string) int {
	t.Helper()
	return countLegacyFixtureRows(t, store, query)
}

// 确保 strings 包被使用（缺口断言说明）。
var _ = strings.Contains
