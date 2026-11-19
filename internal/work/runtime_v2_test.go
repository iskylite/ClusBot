package work

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPendingInputSurvivesHostRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	created, err := s.Submit(t.Context(), "pending-v2", "整理这周资料", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var input int64
	if err = s.db.QueryRow("SELECT message_seq FROM input_deliveries WHERE run_id=?", first.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if err = RecordModelManifest(s.withManifestRecorder(t.Context(), first), ManifestRequest{Messages: []int64{input}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail(created.TaskID, true)
	if err != nil || d.Run.Status != "interrupted" {
		t.Fatalf("interrupted work: %+v, %v", d.Run, err)
	}
	resumed, err := s.ReassignTask(t.Context(), created.TaskID, "resume-v2", d.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(t.Context())
	if err != nil || run.ID != resumed.ID {
		t.Fatalf("resume claim: %+v, %v", run, err)
	}
	var generation int
	if err = s.db.QueryRow("SELECT assignment_generation FROM run_bindings WHERE run_id=?", run.ID).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("assignment generation=%d, %v", generation, err)
	}
	messages, err := s.modelMessagesForRun(t.Context(), run.ConversationID, run.ID)
	if err != nil || len(messages) != 1 || messages[0].ID != input || !messages[0].RequiredInput {
		t.Fatalf("original event not restored: %+v, %v", messages, err)
	}
	if err = RecordModelManifest(s.withManifestRecorder(t.Context(), run), ManifestRequest{Messages: []int64{input}}); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(run, "completed", "", runAccounting{question: "缺少哪一天的资料？"}); err != nil {
		t.Fatal(err)
	}
	var delivered int
	if err = s.db.QueryRow("SELECT count(*) FROM input_deliveries WHERE message_seq=?", input).Scan(&delivered); err != nil || delivered != 2 {
		t.Fatalf("deliveries=%d, %v", delivered, err)
	}
	var processedBy, outcome, wait string
	if err = s.db.QueryRow(`SELECT d.run_id,d.outcome,w.kind FROM input_dispositions d
JOIN task_waits w ON w.run_id=d.run_id WHERE d.message_seq=? AND w.closed_at IS NULL`, input).Scan(&processedBy, &outcome, &wait); err != nil ||
		processedBy != run.ID || outcome != "input_required" || wait != "input" {
		t.Fatalf("disposition=%s %s %s, %v", processedBy, outcome, wait, err)
	}
}

func TestRouterCommitsControlAndDeduplicates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, err := s.Submit(t.Context(), "task-v2", "整理本周报告", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	route := func(_ context.Context, _ string, _ []Message, candidates []RouteCandidate, _ int) (DelegationResult, error) {
		calls++
		if len(candidates) != 1 || candidates[0].ID != task.TaskID {
			t.Fatalf("unexpected candidates: %+v", candidates)
		}
		return DelegationResult{Route: &RouteDecision{Kind: "pause", TaskID: task.TaskID}}, nil
	}
	first, err := s.SubmitRoutedChat(t.Context(), "pause-v2", "暂停整理本周报告", "", "", Delivery{}, route)
	if err != nil || first.Status != "completed" || first.TaskID != "" {
		t.Fatalf("routing receipt: %+v, %v", first, err)
	}
	retry, err := s.SubmitRoutedChat(t.Context(), "pause-v2", "暂停整理本周报告", "", "", Delivery{}, route)
	if err != nil || retry.ID != first.ID || calls != 1 {
		t.Fatalf("duplicate control: %+v, calls=%d, %v", retry, calls, err)
	}
	d, err := s.Detail(task.TaskID, true)
	if err != nil || d.Task.StatusReason != "paused" || d.Run.Status != "interrupted" {
		t.Fatalf("pause outcome: %+v, %v", d, err)
	}
}

func TestExperienceRequiresAcceptedSameScopeWork(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	submit := func(key, scope string) Run {
		t.Helper()
		if _, err := s.Submit(t.Context(), key, "整理例行报告", scope, "", true); err != nil {
			t.Fatal(err)
		}
		run, err := s.claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	source := submit("experience-source", "项目 A")
	if err = s.delta(t.Context(), source.ID, "结论、依据与下一步"); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(source, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	unaccepted := submit("experience-unaccepted", "项目 A")
	if ref, err := s.routeTaskExperience(t.Context(), unaccepted); err != nil || ref != nil {
		t.Fatalf("unaccepted work selected: %+v, %v", ref, err)
	}
	d, err := s.Detail(source.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Accept(t.Context(), source.TaskID, d.Artifact.ID, d.Task.Revision); err != nil {
		t.Fatal(err)
	}
	unrelated := submit("experience-different-scope", "项目 B")
	if ref, err := s.routeTaskExperience(t.Context(), unrelated); err != nil || ref != nil {
		t.Fatalf("cross-scope work selected: %+v, %v", ref, err)
	}
	target := submit("experience-target", "项目 A")
	ref, err := s.routeTaskExperience(t.Context(), target)
	if err != nil || ref == nil || ref.ArtifactID != d.Artifact.ID || ref.Digest != d.Artifact.Digest {
		t.Fatalf("accepted experience not reused: %+v, %v", ref, err)
	}
}

func TestRoutedFollowupIsDeliveredAfterDecision(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, err := s.Submit(t.Context(), "followup-source", "整理本周报告", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	route := func(_ context.Context, _ string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
		return DelegationResult{Route: &RouteDecision{Kind: "follow_up", TaskID: task.TaskID}}, nil
	}
	send := func() Run {
		t.Helper()
		run, err := s.SubmitRoutedChat(t.Context(), "followup-v2", "补上周五数据", "", "", Delivery{}, route)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	if pending := send(); !pending.FollowupQueued || pending.ID != first.ID {
		t.Fatalf("followup was not queued: %+v", pending)
	}
	if pending := send(); !pending.FollowupQueued {
		t.Fatalf("duplicate did not return pending receipt: %+v", pending)
	}
	before, err := s.modelMessagesForRun(t.Context(), first.ConversationID, first.ID)
	if err != nil || len(before) != 1 || before[0].Text != "整理本周报告" {
		t.Fatalf("in-flight inputs changed: %+v, %v", before, err)
	}
	if err = s.delta(t.Context(), first.ID, "第一版"); err != nil {
		t.Fatal(err)
	}
	if err = RecordModelManifest(s.withManifestRecorder(t.Context(), first), ManifestRequest{Messages: []int64{before[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(first, "completed", "", runAccounting{}); err != nil {
		t.Fatal(err)
	}
	next, err := s.claim(t.Context())
	if err != nil || next.TaskID != task.TaskID || next.ID == first.ID {
		t.Fatalf("next run: %+v, %v", next, err)
	}
	after, err := s.modelMessagesForRun(t.Context(), next.ConversationID, next.ID)
	if err != nil || len(after) != 3 || after[1].Text != "第一版" || after[2].Text != "补上周五数据" || !after[2].RequiredInput {
		t.Fatalf("followup order or binding wrong: %+v, %v", after, err)
	}
	if dispatched := send(); dispatched.ID != next.ID || dispatched.FollowupQueued {
		t.Fatalf("retry did not find dispatched run: %+v", dispatched)
	}
}

func TestContextManifestRecordsRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created, err := s.Submit(t.Context(), "manifest-rev", "整理这周资料", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var input int64
	if err = s.db.QueryRow("SELECT message_seq FROM input_deliveries WHERE run_id=?", run.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	recorder := s.withManifestRecorder(t.Context(), run)
	for i := 0; i < 2; i++ {
		if err = RecordModelManifest(recorder, ManifestRequest{Messages: []int64{input}}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.Query(`SELECT id,previous_revision,control_revision FROM context_manifests WHERE run_id=? ORDER BY id`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	var prevs, ctrls []int64
	for rows.Next() {
		var id, prev, ctrl int64
		if err := rows.Scan(&id, &prev, &ctrl); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		prevs = append(prevs, prev)
		ctrls = append(ctrls, ctrl)
	}
	if len(ids) != 2 {
		t.Fatalf("manifests = %d, want 2", len(ids))
	}
	// First manifest chains to nothing; second chains to the first. Neither is
	// rewritten: the pair of rows is the append-only history the design requires.
	if prevs[0] != 0 {
		t.Fatalf("first manifest previous_revision = %d, want 0", prevs[0])
	}
	if prevs[1] != ids[0] {
		t.Fatalf("second manifest previous_revision = %d, want %d", prevs[1], ids[0])
	}
	// control_revision equals the task revision at assembly (>=1 once submitted).
	for i, ctrl := range ctrls {
		if ctrl < 1 {
			t.Fatalf("manifest %d control_revision = %d, want >=1", i, ctrl)
		}
	}
	_ = created
}

func TestModelReceiptMigrationAndImmutableHistory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy-model-manifest.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if _, err := s.Submit(ctx, "manifest-without-receipt", "查询", "", "", false); err != nil {
		t.Fatal(err)
	}
	run, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := ManifestRequest{Digest: "legacy-content"}
	if err = RecordModelManifest(s.withManifestRecorder(ctx, run), old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE model_receipts; PRAGMA user_version=113"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 114 {
		t.Fatalf("receipt migration version %d: %v", version, err)
	}
	base, err := s.RunRecord(ctx, run.ID, "")
	if err != nil || len(base.Contexts) != 1 || base.Contexts[0].ModelReceipt != nil {
		t.Fatalf("legacy receipt invented: %+v %v", base.Contexts, err)
	}
	next, err := RecordModelManifestWithReceipt(s.withManifestRecorder(ctx, run), ManifestRequest{RequestedModel: "alias-new"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := ModelReceipt{Protocol: "responses", State: "reported", ReportedModel: "model-returned"}
	if err := next(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := next(ctx, ModelReceipt{Protocol: "responses", State: "not_reported"}); err != nil {
		t.Fatal(err)
	}
	record, err := s.RunRecord(ctx, run.ID, "")
	if err != nil || len(record.Contexts) != 2 || record.Contexts[0].ModelReceipt == nil || *record.Contexts[0].ModelReceipt != receipt ||
		record.Contexts[1].ModelReceipt != nil || string(record.Contexts[1].Manifest) != string(base.Contexts[0].Manifest) {
		t.Fatalf("receipt replay overwrote history or touched the old request: %+v %v", record.Contexts, err)
	}
	if _, err := s.RunRecord(ctx, run.ID, "unrelated-task-scope"); err == nil {
		t.Fatal("mismatched task path can read model evidence")
	}
}
