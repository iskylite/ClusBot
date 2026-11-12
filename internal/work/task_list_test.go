package work

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestTaskHistoryFiltersBeforePagingAndKeepsWaitingOverview(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<204)
INSERT INTO conversations(id,title,created_at) SELECT printf('%032x',i),'历史记录',100 FROM n;
INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,reason,updated_at)
SELECT id,id,'已停止的工作','Rerun report','Ops','cancelled','已停止',100 FROM conversations;
INSERT INTO conversations(id,title,created_at) VALUES('awaiting','旧的待确认工作',1);
INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,reason,updated_at)
VALUES('awaiting','awaiting','旧的待确认工作','审核报告','Ops','waiting_user','等待验收',1);`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	older, err := s.ListTasks(ctx, TaskFilter{Scope: "Ops", Status: "waiting_user", Limit: 1})
	if err != nil || len(older) != 1 || older[0].ID != "awaiting" {
		t.Fatalf("newer stopped tasks hid the waiting task: %+v, %v", older, err)
	}
	filter := TaskFilter{Scope: "Ops", Status: "cancelled", Query: "rerun", Limit: 2}
	first, err := s.ListTasks(ctx, filter)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	filter.AfterUpdatedAt, filter.AfterID = first[1].UpdatedAt, first[1].ID
	second, err := s.ListTasks(ctx, filter)
	if err != nil || len(second) != 2 || second[0].UpdatedAt != filter.AfterUpdatedAt || second[0].ID <= filter.AfterID || second[0].ID >= second[1].ID {
		t.Fatalf("cursor did not handle equal timestamps: %+v, %v", second, err)
	}
	snapshot, err := s.Overview(ctx, OverviewFilter{Scope: "Ops"})
	if err != nil || len(snapshot.Groups["waiting"]) != 1 || snapshot.Groups["waiting"][0].ID != "awaiting" || len(snapshot.Groups["active"])+len(snapshot.Groups["completed"]) != 0 || len(snapshot.Scopes) != 1 || snapshot.Scopes[0] != "Ops" {
		t.Fatalf("overview contains wrong groups or scopes: %+v, %v, %v", snapshot.Groups, snapshot.Scopes, err)
	}
}

func TestOverviewCursorsPageEachGroupWithinTheFilter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "overview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<212)
INSERT INTO conversations(id,title,created_at) SELECT printf('%032x',i),'Report',100 FROM n;
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<212)
INSERT INTO tasks(id,conversation_id,title,goal,scope_label,status,status_reason,reason,updated_at,state_changed_at,completed_at)
SELECT printf('%032x',i),printf('%032x',i),'Report','整理',CASE WHEN i=212 THEN 'Other' ELSE 'Ops' END,
CASE WHEN i<=205 OR i=212 THEN 'waiting_user' WHEN i<=208 THEN 'active' ELSE 'completed' END,
'','',CASE WHEN i BETWEEN 206 AND 208 THEN 300+(i-206)/2 ELSE 1000+i END,
100+(i-1)/2,CASE WHEN i=209 OR i=210 THEN 400 WHEN i=211 THEN 401 END FROM n;`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := func(index int) string { return fmt.Sprintf("%032x", index) }
	indexes := func(snapshot OverviewSnapshot, group string) []string {
		var ids []string
		for _, task := range snapshot.Groups[group] {
			ids = append(ids, task.ID)
		}
		return ids
	}
	filter := OverviewFilter{Scope: "Ops", Query: "report"}
	first, err := s.Overview(ctx, filter)
	if err != nil || len(first.Groups["waiting"]) != TaskPageLimit || first.Counts["waiting"] != 205 ||
		first.NextCursors["waiting"] != "199:"+id(200) || first.SnapshotAt == 0 {
		t.Fatalf("default waiting page: count=%d length=%d cursors=%v err=%v", first.Counts["waiting"], len(first.Groups["waiting"]), first.NextCursors, err)
	}
	filter.After = map[string]TaskCursor{"waiting": {Timestamp: 199, ID: id(200)}}
	tail, err := s.Overview(ctx, filter)
	if err != nil || !slices.Equal(indexes(tail, "waiting"), []string{id(201), id(202), id(203), id(204), id(205)}) ||
		tail.NextCursors["waiting"] != "" || !slices.Equal(indexes(tail, "active"), indexes(first, "active")) {
		t.Fatalf("waiting tail: %+v, %v", tail, err)
	}
	filter.Limit, filter.After = 2, nil
	page, err := s.Overview(ctx, filter)
	if err != nil || !slices.Equal(indexes(page, "active"), []string{id(208), id(206)}) ||
		!slices.Equal(indexes(page, "completed"), []string{id(211), id(209)}) ||
		page.NextCursors["active"] != "300:"+id(206) || page.NextCursors["completed"] != "400:"+id(209) {
		t.Fatalf("descending group order and ties: %+v, %v", page, err)
	}
	filter.After = map[string]TaskCursor{
		"waiting": {Timestamp: 100, ID: id(2)}, "active": {Timestamp: 300, ID: id(206)}, "completed": {Timestamp: 400, ID: id(209)},
	}
	more, err := s.Overview(ctx, filter)
	if err != nil || !slices.Equal(indexes(more, "waiting"), []string{id(3), id(4)}) ||
		!slices.Equal(indexes(more, "active"), []string{id(207)}) || !slices.Equal(indexes(more, "completed"), []string{id(210)}) {
		t.Fatalf("independent cursors: %+v, %v", more, err)
	}
}

func TestWaitingOrderFollowsStateInsteadOfTaskEdits(t *testing.T) {
	s, _, _, original := publicationFixture(t)
	ctx := context.Background()
	older := original.Task.ID
	if _, err := s.db.Exec("UPDATE tasks SET state_changed_at=1 WHERE id=?", older); err != nil {
		t.Fatal(err)
	}
	newer, err := s.Submit(ctx, "waiting-input", "准备交接清单", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(newer, "completed", "", runAccounting{question: "请确认接收人。"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE tasks SET state_changed_at=2 WHERE id=?", newer.TaskID); err != nil {
		t.Fatal(err)
	}
	name := "更新标题后的旧事项"
	edited, err := s.EditTask(ctx, older, TaskEdit{Revision: original.Task.Revision, Title: &name})
	if err != nil || edited.StateChangedAt != 1 || edited.StatusReason != "acceptance_required" {
		t.Fatalf("editing a waiting task moved its state: %+v, %v", edited, err)
	}
	snapshot, err := s.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 2 || snapshot.Groups["waiting"][0].ID != older || snapshot.Groups["waiting"][1].StatusReason != "input_required" {
		t.Fatalf("wait order: %+v, %v", snapshot.Groups, err)
	}
	original.Task = &edited
	p := requestWrite(t, s, original, "交接.md", "approval-transition")
	pending, err := s.Detail(older, true)
	if err != nil || pending.Task.StatusReason != "approval_required" || pending.Task.StateChangedAt <= edited.StateChangedAt {
		t.Fatalf("same status, new decision: %+v, %v", pending.Task, err)
	}
	if _, _, err = s.beginPublication(ctx, p.ID, "approve", p.taskRevision); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 1 || snapshot.Groups["waiting"][0].ID != newer.TaskID || len(snapshot.Groups["active"]) != 1 || snapshot.Groups["active"][0].StatusReason != "file_writing" {
		t.Fatalf("in-flight write still looks like a decision: %+v, %v", snapshot.Groups, err)
	}
	if _, err = s.finishPublication(p, "unknown", "写入回执未取得"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.Overview(ctx, OverviewFilter{})
	if err != nil || len(snapshot.Groups["waiting"]) != 2 || snapshot.Groups["waiting"][0].ID != newer.TaskID || snapshot.Groups["waiting"][1].StatusReason != "reconciliation_required" || len(snapshot.Groups["active"]) != 0 {
		t.Fatalf("unknown write not ordered as a new decision: %+v, %v", snapshot.Groups, err)
	}
}
