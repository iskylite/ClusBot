package httpapi

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

func taskRecordsConversationFixture(t *testing.T, f *discoveryA2UIFixture, target string) string {
	t.Helper()
	ctx := t.Context()
	run, err := f.store.OpenAssistantTaskList(ctx, f.actor, "", "open-scoped-task-records", "查看我的任务")
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/conversations/" + run.ConversationID + "/task-card/" + run.ID
	path := base + "/records/" + target
	if w := f.request("GET", path, "", nil, f.cookie); w.Code != http.StatusNotFound {
		t.Fatalf("an unselected task was readable: %d %s", w.Code, w.Body)
	}
	card, err := f.store.AssistantTaskListCard(ctx, f.actor, run.ConversationID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the real durable progress-receipt transaction. The browser test
	// independently proves an actual A2UI event + controlled model admits it.
	a := work.AssistantTaskProgressAction{Actor: f.actor, Conversation: run.ConversationID, Run: run.ID, Target: target, Revision: card.Revision, Request: "seed-original-read-receipt"}
	if done, err := f.store.BeginAssistantTaskProgress(ctx, a); err != nil || done {
		t.Fatalf("unable to reserve the authorized fixture read: %t %v", done, err)
	}
	if err := f.store.FinishAssistantTaskProgress(ctx, a, "show_progress"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAssistantTaskRecordsExactBenchmarkEvidenceAndNoDispatch(t *testing.T) {
	f := makeAssistantComparisonFixture(t, nil)
	path := taskRecordsConversationFixture(t, f.discoveryA2UIFixture, f.pair[0])
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var authoritative int64
	var operation, step, originalQuery string
	if err := db.QueryRow(`SELECT c.step_id,c.operation_id,j.query_key,a.occurred_at
FROM benchmark_cases c JOIN operation_jobs j ON j.operation_id=c.operation_id AND j.provider_job_id=c.job_id
JOIN runtime_audit a ON a.task_id=c.task_id AND a.step_id=c.step_id AND a.event='benchmark_case_verified'
WHERE c.task_id=?`, f.pair[0]).Scan(&step, &operation, &originalQuery, &authoritative); err != nil || authoritative <= 0 {
		t.Fatalf("fixture did not produce an actual terminal audit: %d %v", authoritative, err)
	}
	if _, err := db.Exec("UPDATE steps SET updated_at=4070908800000 WHERE step_id=?", step); err != nil {
		t.Fatal(err)
	}
	before := modelReceiptReadSnapshot(t, db)
	restore := func(cookie *http.Cookie) string {
		t.Helper()
		w := f.request("GET", path, "", nil, cookie)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") != "" {
			t.Fatalf("record projection: %d %s", w.Code, w.Body)
		}
		var out struct {
			Task    string                 `json:"task_id"`
			Surface []assistantA2UIMessage `json:"surface"`
		}
		accountJSON(t, w, &out)
		if out.Task != f.pair[0] || len(out.Surface) != 2 || out.Surface[1].Version != "1.0" || out.Surface[1].UpdateComponents == nil {
			t.Fatalf("wrong scope or protocol: %s", w.Body)
		}
		for _, n := range out.Surface[1].UpdateComponents.Components {
			if n.Action != nil || n.Component != "Text" && n.Component != "Column" {
				t.Fatalf("task evidence was not text-only: %+v", n)
			}
		}
		for _, secret := range []string{comparisonFixtureAsset, comparisonFixtureEvidence, "2099-01-01", "fixture-job-1", "fixture-original", operation, step} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("record included false/stale/raw data %q: %s", secret, w.Body)
			}
		}
		return w.Body.String()
	}
	for _, cookie := range []*http.Cookie{f.cookie, f.account.login("admin", "correct horse battery staple")} {
		body := restore(cookie)
		if !strings.Contains(body, "终态登记 "+time.UnixMilli(authoritative).UTC().Format("2006-01-02 15:04:05 UTC")) ||
			!strings.Contains(body, "基准报告引用的原始证据 · bandwidth") {
			t.Fatalf("missing from verified report: %s", body)
		}
	}
	parts := strings.Split(path, "/")
	for _, other := range []string{
		strings.TrimSuffix(path, f.pair[0]) + f.pair[1],
		strings.Replace(path, parts[4], strings.Repeat("1", 32), 1),
		strings.Replace(path, parts[6], strings.Repeat("2", 32), 1),
	} {
		if w := f.request("GET", other, "", nil, f.cookie); w.Code != http.StatusNotFound {
			t.Fatalf("alternate target or origin became readable: %d %s", w.Code, w.Body)
		}
	}
	if _, err := db.Exec("UPDATE operation_jobs SET query_key='unrelated-job-query' WHERE operation_id=?", operation); err != nil {
		t.Fatal(err)
	}
	if body := restore(f.cookie); strings.Contains(body, "基准报告引用的原始证据") || !strings.Contains(body, "当前无法核对原作业证据") {
		t.Fatalf("wrong job was called verified: %s", body)
	}
	if _, err := db.Exec("UPDATE operation_jobs SET query_key=? WHERE operation_id=?", originalQuery, operation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM benchmark_evidence WHERE step_id=?", step); err != nil {
		t.Fatal(err)
	}
	if body := restore(f.cookie); strings.Contains(body, "基准报告引用的原始证据") || !strings.Contains(body, "当前无法核对原作业证据") {
		t.Fatalf("a report with missing required original was presented as complete evidence: %s", body)
	}
	if _, err := db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='read' AND object_ref=?", time.Now().UnixMilli(), f.actor, comparisonFixtureAsset); err != nil {
		t.Fatal(err)
	}
	if body := restore(f.cookie); strings.Contains(body, "bandwidth") || !strings.Contains(body, "有些来源已不可查看") {
		t.Fatalf("reopened after revocation leaked detail: %s", body)
	}
	if f.worker.starts.Load() != 2 || f.worker.reads.Load() != 2 || f.calls.Load() != 0 || modelReceiptReadSnapshot(t, db) != before {
		t.Fatal("history read re-executed original work, model, or wrote to chat")
	}
}

func TestAssistantTaskRecordsUsesExactLocalResultAndReadGrant(t *testing.T) {
	f := newDiscoveryA2UIFixture(t, nil)
	ctx := t.Context()
	const ref = "private-task-record-asset"
	if err := f.store.ImportDomainAssetsForAccount(ctx, f.actor, []work.DomainAssetEntry{{ObjectRef: ref, Kind: "node", Identity: "owned-fixture", Read: true}}); err != nil {
		t.Fatal(err)
	}
	task, err := f.store.SubmitAssetAcceptanceQuery(ctx, "local-record-owned-query", ref, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := f.store.EnableAssetAcceptanceQuery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := executor.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("fixture did not finish the real read unit: %t %v", worked, err)
	}
	path := taskRecordsConversationFixture(t, f, task.TaskID)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM authorization_decisions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", path, "", nil, f.cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "资产验收状态查询 · 已完成 · 结果入账 ") || strings.Contains(w.Body.String(), ref) {
		t.Fatalf("local receipt was not read exactly, or leaked contents: %d %s", w.Code, w.Body)
	}
	var after int
	if err := db.QueryRow("SELECT count(*) FROM authorization_decisions").Scan(&after); err != nil || after != n {
		t.Fatalf("read replay recorded a new decision: %d/%d %v", n, after, err)
	}
	if _, err := db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='read' AND object_ref=?", time.Now().UnixMilli(), f.actor, ref); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", path, "", nil, f.cookie); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "资产验收状态查询") {
		t.Fatalf("stale scoped local result was disclosed: %d %s", w.Code, w.Body)
	}
}

func prepareTaskRecordsBrowserFixture(t *testing.T, f *discoveryA2UIFixture, title string) (work.Run, work.Artifact) {
	t.Helper()
	const body = "case-report-body-private <https://example.invalid/task-proof>"
	target, artifact := produceAssistantArtifact(t, f.store, title, body, "accepted-task-evidence-fixture")
	d, err := f.store.Detail(target.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Accept(t.Context(), target.TaskID, artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Clean(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Most ordinary Report runs have no internal steps; create one
	// history fixture with no terminal clock to verify it is not guessed.
	if _, err := db.Exec(`INSERT INTO steps(step_id,task_id,contract_revision,definition_key,definition_version,state,position,created_at,updated_at)
VALUES('browser-record-without-completion-time',?,1,'test-record',1,'completed',1,1,4070908800000)`, target.TaskID); err != nil {
		t.Fatal(err)
	}
	return target, artifact
}
