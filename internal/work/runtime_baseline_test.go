package work

// 建立可复现的代码与数据基线。
//
// 本文件提供两类测试入口：
//   - TestBaselineFixture：在临时目录构造覆盖普通对话、已验收成果、
//     未确认写入、暂停、取消、未知读取和父子工作流的基线样本；同一构造
//     函数重复执行必须得到同样的逻辑记录（ID 随机，结构与关联稳定）。
//   - TestBaselineEvidenceLinks：核对样本的文件引用、摘要与外键
//     关联完整，并验证受支持旧版本（schema 45/46）的迁移分支与不支持
//     版本的明确拒绝。
//
// 样本不复制真实凭据；连接配置使用 loopback 占位地址，且测试不发起任何
// 真实网络调用（未知读取经连接不可用错误注入产生，不离开本机）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clusbot/internal/mcpclient"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// baselineCounts 是可重复断言的逻辑记录计数集合。
type baselineCounts struct {
	Conversations    int
	Tasks            int
	Runs             int
	Messages         int
	Artifacts        int
	Acceptances      int
	Publications     int
	UnknownReads     int
	WorkflowChildren int
	WorkflowDeps     int
	Connections      int
	Submissions      int
}

// baselineFixture 是一次基线样本构造的返回句柄。
type baselineFixture struct {
	Counts          baselineCounts
	ChatRun         Run
	AcceptedTask    Run
	PendingPub      Run
	PausedTask      Run
	CancelledTask   Run
	UnknownReadTask Run
	Workflow        Workflow
	WorkflowParent  Run
}

// buildBaselineFixture 在 path 指向的新库上构造全部基线样本。
// 除 runTaskReadAt 的未知结果注入外不发起任何外部调用。
func buildBaselineFixture(t *testing.T, path string) baselineFixture {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open baseline store: %v", err)
	}
	defer store.Close()
	if err := store.OpenWorkspace(filepath.Join(t.TempDir(), "workspace")); err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	ctx := context.Background()

	// --- 1. 普通对话（非任务 Run） ---
	chatRun, err := store.Submit(ctx, "v3p001-chat", "今天天气怎么样", "", "", false)
	if err != nil {
		t.Fatalf("submit chat: %v", err)
	}
	claimedChat, err := store.claim(ctx)
	if err != nil || claimedChat.ID != chatRun.ID {
		t.Fatalf("claim chat run: %+v %v", claimedChat, err)
	}
	executeBaselineEcho(store, ctx, claimedChat, "普通对话回答")

	// --- 2. 已验收成果任务 ---
	accepted, err := store.Submit(ctx, "v3p001-accepted", "整理季度汇报", "", "", true)
	if err != nil {
		t.Fatalf("submit accepted task: %v", err)
	}
	run, err := store.claim(ctx)
	if err != nil || run.ID != accepted.ID {
		t.Fatalf("claim accepted run: %+v %v", run, err)
	}
	executeBaselineEcho(store, ctx, run, "# 季度汇报\n\n已完成整理。")
	detail, err := store.Detail(accepted.TaskID, true)
	if err != nil {
		t.Fatalf("detail accepted task: %v", err)
	}
	if detail.Task == nil || detail.Task.Status != "waiting_user" || detail.Artifact == nil {
		t.Fatalf("accepted task not waiting for acceptance: %+v", detail)
	}
	if err := store.Accept(ctx, accepted.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatalf("accept artifact: %v", err)
	}

	// --- 3. 未确认写入（publication 停留在 pending） ---
	pendingPub, err := store.Submit(ctx, "v3p001-pending-pub", "生成待保存的发布说明", "", "", true)
	if err != nil {
		t.Fatalf("submit pending publication task: %v", err)
	}
	run, err = store.claim(ctx)
	if err != nil || run.ID != pendingPub.ID {
		t.Fatalf("claim pending publication run: %+v %v", run, err)
	}
	executeBaselineEcho(store, ctx, run, "# 发布说明\n\n待用户确认保存。")
	detail, err = store.Detail(pendingPub.TaskID, true)
	if err != nil {
		t.Fatalf("detail pending publication task: %v", err)
	}
	if detail.Artifact == nil {
		t.Fatalf("pending publication task has no artifact: %+v", detail)
	}
	publication, err := store.RequestPublication(ctx, pendingPub.TaskID, detail.Artifact.ID, "release-notes.md", "v3p001-pub", detail.Task.Revision)
	if err != nil {
		t.Fatalf("request publication: %v", err)
	}
	if publication.Status != "pending" {
		t.Fatalf("publication not pending: %+v", publication)
	}

	// --- 4. 暂停任务（active + status_reason=paused） ---
	// 在 Run 已被领取、尚未完成时持久化暂停意图；竞争中的完成不会越过暂停。
	paused, err := store.Submit(ctx, "v3p001-paused", "整理会议纪要", "", "", true)
	if err != nil {
		t.Fatalf("submit paused task: %v", err)
	}
	pauseClaimed, err := store.claim(ctx)
	if err != nil || pauseClaimed.ID != paused.ID {
		t.Fatalf("claim paused run: %+v %v", pauseClaimed, err)
	}
	pauseDetail, err := store.Detail(paused.TaskID, true)
	if err != nil {
		t.Fatalf("detail paused task: %v", err)
	}
	if err := store.Control(ctx, paused.TaskID, "pause", pauseDetail.Task.Revision); err != nil {
		t.Fatalf("pause task: %v", err)
	}
	if err := store.finish(pauseClaimed, "completed", "", runAccounting{}); err != nil {
		t.Fatalf("racing completion after pause: %v", err)
	}
	if pausedCheck, err := store.Detail(paused.TaskID, true); err != nil ||
		pausedCheck.Task.Status != "interrupted" || pausedCheck.Task.StatusReason != "paused" {
		t.Fatalf("pause did not persist: %+v %v", pausedCheck, err)
	}

	// --- 5. 取消任务（取消仍 queued 的 Run） ---
	cancelled, err := store.Submit(ctx, "v3p001-cancelled", "临时的资料搜集", "", "", true)
	if err != nil {
		t.Fatalf("submit cancelled task: %v", err)
	}
	cancelDetail, err := store.Detail(cancelled.TaskID, true)
	if err != nil {
		t.Fatalf("detail cancelled task: %v", err)
	}
	if err := store.Control(ctx, cancelled.TaskID, "stop", cancelDetail.Task.Revision); err != nil {
		t.Fatalf("stop task: %v", err)
	}

	// --- 6. 未知读取（dispatch 已受理、外部调用结果未能取回） ---
	// 起一个真实 MCP 服务：第一次 Discover 提供工具，此后会话断开、工具消失，
	// 使 callConnectionTool 在 connection_calls 留下派发记录后无法取得结果。
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "v3p001-lookup", Version: "1"}, nil)
	mcpInput := json.RawMessage(`{"type":"object","properties":{"doc":{"type":"string"}},"required":["doc"]}`)
	mcpOutput := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	mcpServer.AddTool(&mcp.Tool{Name: "read_summary", Title: "读取摘要", InputSchema: mcpInput, OutputSchema: mcpOutput},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "合同金额待核对"}},
				StructuredContent: map[string]any{"summary": "合同金额待核对"},
			}, nil
		})
	// 服务对 initialize/tools-list 正常响应，对 tools/call 一律失联：
	// connection_calls 留下派发记录，但实际结果无法取回。
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mcpHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err == nil && strings.Contains(string(body), `"tools/call"`) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		mcpHandler.ServeHTTP(w, r)
	}))
	defer mcpHTTP.Close()

	conn, err := store.SaveConnection(ctx, "", Connection{Name: "基线占位连接", Enabled: true, Config: mcpclient.Config{
		Transport: "http",
		Endpoint:  mcpHTTP.URL,
	}})
	if err != nil {
		t.Fatalf("save fixture connection: %v", err)
	}
	conn, err = store.TestConnection(ctx, conn.ID, conn.Revision)
	if err != nil || conn.Status != "ready" || len(conn.Tools) != 1 {
		t.Fatalf("test fixture connection: %+v %v", conn, err)
	}
	if err := store.SetToolPermission(ctx, conn.ID, conn.Tools[0].Name, conn.Tools[0].Version, conn.Revision, true); err != nil {
		t.Fatalf("allow fixture tool: %v", err)
	}
	conn, err = store.Connection(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	unknownRead, err := store.SubmitWithDelivery(ctx, "v3p001-unknown-read", "读取合同摘要", "", "", true, Delivery{
		Read: &TaskReadRequest{ConnectionID: conn.ID, Revision: conn.Revision, ToolName: conn.Tools[0].Name,
			ToolVersion: conn.Tools[0].Version, Arguments: map[string]any{"doc": "合同A"}},
	})
	if err != nil {
		t.Fatalf("submit unknown read task: %v", err)
	}
	run, err = store.claim(ctx)
	if err != nil || run.ID != unknownRead.ID {
		t.Fatalf("claim unknown read run: %+v %v", run, err)
	}
	// 注入"运行中"前提，使读取完成路径校验通过；调用在服务失联后进行，
	// 派发记录已写入但结果无法取回，状态保持 unknown。
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatalf("mark read run running: %v", err)
	}
	readResult, readErr := store.runTaskReadAt(context.WithValue(ctx, executionGenerationKey{}, run.Generation), run.ID, 1)
	if !errors.Is(readErr, errReadFailed) {
		t.Fatalf("unknown read injection: readErr=%v, want errReadFailed", readErr)
	}
	if readResult == nil || readResult.Status != "unknown" {
		t.Fatalf("unknown read status: %+v, want unknown", readResult)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET status='queued' WHERE id=?`, run.ID); err != nil {
		t.Fatalf("restore read run queued: %v", err)
	}

	// --- 7. 父子工作流（父 + 两个子任务 + 一条依赖边） ---
	workflow, err := store.CreateWorkflow(ctx, "v3p001-workflow", WorkflowRequest{
		Goal:     "发布月度报告",
		Scope:    "工作资料",
		Criteria: "两个子任务成果均验收",
		Steps: []WorkflowStep{
			{Key: "collect", Goal: "收集素材", Criteria: "素材列表完整"},
			{Key: "assemble", Goal: "汇编成稿", Criteria: "成稿通过评审", Depends: []string{"collect"}},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if len(workflow.Children) != 2 {
		t.Fatalf("workflow children = %d, want 2", len(workflow.Children))
	}
	parentRun, err := scanRun(store.db.QueryRow("SELECT "+runColumns+" FROM runs WHERE task_id=?", workflow.ParentID))
	if err != nil {
		t.Fatalf("scan workflow parent run: %v", err)
	}

	// --- 汇总逻辑计数 ---
	fixture := baselineFixture{
		ChatRun:         chatRun,
		AcceptedTask:    accepted,
		PendingPub:      pendingPub,
		PausedTask:      paused,
		CancelledTask:   cancelled,
		UnknownReadTask: unknownRead,
		Workflow:        workflow,
		WorkflowParent:  parentRun,
	}
	countQueries := map[string]*int{
		"SELECT COUNT(*) FROM conversations":          &fixture.Counts.Conversations,
		"SELECT COUNT(*) FROM tasks":                  &fixture.Counts.Tasks,
		"SELECT COUNT(*) FROM runs":                   &fixture.Counts.Runs,
		"SELECT COUNT(*) FROM messages":               &fixture.Counts.Messages,
		"SELECT COUNT(*) FROM artifacts":              &fixture.Counts.Artifacts,
		"SELECT COUNT(*) FROM task_acceptances":       &fixture.Counts.Acceptances,
		"SELECT COUNT(*) FROM publications":           &fixture.Counts.Publications,
		"SELECT COUNT(*) FROM run_reads WHERE status='unknown'": &fixture.Counts.UnknownReads,
		"SELECT COUNT(*) FROM workflow_children":      &fixture.Counts.WorkflowChildren,
		"SELECT COUNT(*) FROM workflow_dependencies":  &fixture.Counts.WorkflowDeps,
		"SELECT COUNT(*) FROM connections":            &fixture.Counts.Connections,
		"SELECT COUNT(*) FROM submissions":            &fixture.Counts.Submissions,
	}
	for query, dst := range countQueries {
		if err := store.db.QueryRow(query).Scan(dst); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
	}
	return fixture
}

// executeBaselineEcho 用固定文本的受控 provider 完成一次领取到的 Run。
func executeBaselineEcho(s *Store, ctx context.Context, run Run, text string) {
	s.execute(ctx, testProvider(func(_ context.Context, _ []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
		if err := emit(text); err != nil {
			return nil, err
		}
		input, output := int64(10), int64(5)
		return &TokenUsage{InputTokens: &input, OutputTokens: &output}, nil
	}), run, RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 512})
	s.reconcileFinalizations(ctx)
}

// TestBaselineFixture 记录代码与数据基线，并验证样本可复现。
func TestBaselineFixture(t *testing.T) {
	// 代码、schema 与运行时版本基线。
	t.Logf("baseline: go=%s modernc.org/sqlite=v1.62.0 schema=%d", runtime.Version(), databaseSchemaVersion)
	var sqliteVersion string
	func() {
		store, err := Open(filepath.Join(t.TempDir(), "version.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.db.QueryRow("SELECT sqlite_version()").Scan(&sqliteVersion); err != nil {
			t.Fatalf("query sqlite_version: %v", err)
		}
	}()
	t.Logf("baseline: sqlite runtime version=%s", sqliteVersion)

	path := filepath.Join(t.TempDir(), "baseline.db")
	first := buildBaselineFixture(t, path)

	// 核对：当前 Task 五态约束与单连接配置。
	var taskCheck string
	func() {
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='tasks'`).Scan(&taskCheck); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(taskCheck, "'active','waiting_user','interrupted','completed','cancelled'") {
			t.Fatalf("task status constraint drifted: %s", taskCheck)
		}
	}()

	// 各类样本状态核对。
	type taskState struct{ status, reason string }
	states := map[string]taskState{}
	func() {
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		for id, run := range map[string]Run{
			"accepted": first.AcceptedTask, "pending_pub": first.PendingPub,
			"paused": first.PausedTask, "cancelled": first.CancelledTask, "unknown_read": first.UnknownReadTask,
		} {
			var st taskState
			if err := store.db.QueryRow("SELECT status,status_reason FROM tasks WHERE id=?", run.TaskID).Scan(&st.status, &st.reason); err != nil {
				t.Fatalf("scan %s task state: %v", id, err)
			}
			states[id] = st
		}
		var pubStatus string
		if err := store.db.QueryRow("SELECT status FROM publications WHERE task_id=?", first.PendingPub.TaskID).Scan(&pubStatus); err != nil {
			t.Fatalf("scan publication status: %v", err)
		}
		states["publication"] = taskState{status: pubStatus}
	}()
	wantStates := map[string]taskState{
		"accepted":     {"completed", "accepted"},
		"pending_pub":  {"waiting_user", "approval_required"},
		"paused":       {"interrupted", "paused"},
		"cancelled":    {"cancelled", "stopped"},
		"unknown_read": {"active", "running"},
		"publication":  {"pending", ""},
	}
	for id, want := range wantStates {
		if states[id] != want {
			t.Errorf("sample %s state = %+v, want %+v", id, states[id], want)
		}
	}

	// 相同夹具在新库上重复构造，逻辑记录计数一致。
	second := buildBaselineFixture(t, filepath.Join(t.TempDir(), "baseline-copy.db"))
	if first.Counts != second.Counts {
		t.Errorf("fixture not reproducible:\nfirst=%+v\nsecond=%+v", first.Counts, second.Counts)
	}
	wantCounts := baselineCounts{
		Conversations: 9, Tasks: 8, Runs: 9, Messages: 12, Artifacts: 2, Acceptances: 1,
		Publications: 1, UnknownReads: 1, WorkflowChildren: 2, WorkflowDeps: 1,
		Connections: 1, Submissions: 7,
	}
	if first.Counts != wantCounts {
		t.Errorf("baseline counts drifted: got %+v, want %+v", first.Counts, wantCounts)
	}
	t.Logf("baseline counts: %+v", first.Counts)
}

// TestBaselineEvidenceLinks 核对文件引用、摘要、外键关联与迁移分支。
func TestBaselineEvidenceLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	fixture := buildBaselineFixture(t, path)

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 引用与摘要：成果内容摘要一致、发布绑定目标文件名与成果摘要。
	var artifactDigest, artifactContent string
	if err := store.db.QueryRow("SELECT sha256,content FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 1", fixture.AcceptedTask.TaskID).
		Scan(&artifactDigest, &artifactContent); err != nil {
		t.Fatalf("scan accepted artifact: %v", err)
	}
	if digest(artifactContent) != artifactDigest {
		t.Errorf("artifact digest mismatch: content digest %s, recorded %s", digest(artifactContent), artifactDigest)
	}
	var pubFilename, pubDigest string
	if err := store.db.QueryRow("SELECT filename,sha256 FROM publications WHERE task_id=?", fixture.PendingPub.TaskID).
		Scan(&pubFilename, &pubDigest); err != nil {
		t.Fatalf("scan publication: %v", err)
	}
	var pubArtifactDigest string
	if err := store.db.QueryRow("SELECT sha256 FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 1", fixture.PendingPub.TaskID).
		Scan(&pubArtifactDigest); err != nil {
		t.Fatalf("scan publication artifact: %v", err)
	}
	if pubFilename != "release-notes.md" || pubDigest != pubArtifactDigest {
		t.Errorf("publication link drifted: filename=%s digest=%s artifact=%s", pubFilename, pubDigest, pubArtifactDigest)
	}

	// 外键与完整性检查。
	var fkViolations int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if fkViolations != 0 {
		t.Errorf("foreign key violations = %d, want 0", fkViolations)
	}
	var integrity string
	if err := store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %s, want ok", integrity)
	}

	// 工作流关联：父任务、子任务与依赖边互相可达。
	var depChild, depParent string
	if err := store.db.QueryRow(`SELECT c.task_id,d.prerequisite_id FROM workflow_children c
JOIN workflow_dependencies d ON d.task_id=c.task_id WHERE c.parent_id=? AND c.step_key='assemble'`, fixture.Workflow.ParentID).
		Scan(&depChild, &depParent); err != nil {
		t.Fatalf("scan workflow dependency: %v", err)
	}
	var collectID string
	if err := store.db.QueryRow("SELECT task_id FROM workflow_children WHERE parent_id=? AND step_key='collect'", fixture.Workflow.ParentID).
		Scan(&collectID); err != nil {
		t.Fatalf("scan collect child: %v", err)
	}
	if depParent != collectID {
		t.Errorf("workflow dependency links wrong parent: got %s, want collect %s", depParent, collectID)
	}

	// 未知读取保持未确认：查询 Detail 返回 unknown_read_run_id。
	d, err := store.Detail(fixture.UnknownReadTask.TaskID, true)
	if err != nil {
		t.Fatalf("detail unknown read task: %v", err)
	}
	if d.UnknownReadRunID == "" {
		t.Errorf("unknown read run not surfaced in Detail: %+v", d)
	}

	// 迁移分支核对：schema 45/46 受支持，未支持版本明确拒绝。
	for _, version := range []int{45, 46} {
		t.Run(fmt.Sprintf("supported_v%d", version), func(t *testing.T) {
			legacy := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", legacy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE runs(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, task_id TEXT,
status TEXT NOT NULL, response TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
created_at INTEGER NOT NULL, control TEXT NOT NULL DEFAULT '', usage_tracked INTEGER NOT NULL DEFAULT 0,
model_requests INTEGER NOT NULL DEFAULT 0, active_ms INTEGER NOT NULL DEFAULT 0, time_partial INTEGER NOT NULL DEFAULT 0,
input_tokens INTEGER, output_tokens INTEGER, error_code TEXT NOT NULL DEFAULT '', reported_requests INTEGER NOT NULL DEFAULT 0,
execution_generation INTEGER NOT NULL DEFAULT 0, read_failure_run_id TEXT, memory_command INTEGER NOT NULL DEFAULT 0,
admission_requests INTEGER NOT NULL DEFAULT 0, admission_output INTEGER NOT NULL DEFAULT 0, admission_output_used INTEGER NOT NULL DEFAULT 0)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE model_settings(id INTEGER PRIMARY KEY CHECK(id=1), base_url TEXT NOT NULL,
model_id TEXT NOT NULL, api_key_ref TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '',
protocol TEXT NOT NULL DEFAULT 'responses', revision INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE context_manifests(id INTEGER PRIMARY KEY, run_id TEXT NOT NULL,
generation INTEGER NOT NULL, assembled_at INTEGER NOT NULL, manifest TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			migrated, err := Open(legacy)
			if err != nil {
				t.Fatalf("supported version %d rejected: %v", version, err)
			}
			defer migrated.Close()
			var got int
			if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != databaseSchemaVersion {
				t.Errorf("after migration from %d: version = %d, want %d", version, got, databaseSchemaVersion)
			}
		})
	}
	t.Run("unsupported_version_rejected", func(t *testing.T) {
		legacy := filepath.Join(t.TempDir(), "unsupported.db")
		db, err := sql.Open("sqlite", legacy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE marker(id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version=42"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(legacy); err == nil {
			t.Error("unsupported schema version 42 was not rejected")
		}
	})

	// 受支持版本迁移不依赖真实模型密钥与现网数据；样本连接不含真实凭据。
	var config string
	if err := store.db.QueryRow(`SELECT config FROM connections WHERE name='基线占位连接'`).Scan(&config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, "127.0.0.1") {
		t.Errorf("fixture connection config contains non-loopback material: %s", config)
	}
	// 工作区引用：发布产物未写出到数据目录之外（仍在 pending，无文件副作用）。
	var delivered int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM publications WHERE status='succeeded'").Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Errorf("baseline unexpectedly delivered %d publications", delivered)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("baseline database missing: %v", err)
	}
}
