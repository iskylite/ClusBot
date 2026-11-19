package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestRouterBenchmarkHintRejectsUnsafeTextAndTaskContext(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "router-benchmark.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	b := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	g := DomainBenchmarkGrant{Connection: b.Connection, Revision: 1, Tool: b.Tool, ExpiresAt: now() + 60000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []DomainBenchmarkGrant{g}}}); err != nil {
		t.Fatal(err)
	}
	cases := []BenchmarkCase{{Key: "one", Binding: b, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}
	for _, version := range []int{1, 2} {
		if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "full", Version: version, Cases: cases}); err != nil {
			t.Fatal(err)
		}
	}
	if err := setBenchmarkTargetAtCurrent(t, s, ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
		t.Fatal(err)
	}
	taskRun, err := s.Submit(ctx, "existing-task", "已有任务", "bench", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE runs SET status='completed' WHERE id=?", taskRun.ID); err != nil {
		t.Fatal(err)
	}
	const natural = "POD7那一整套基准就交给你安排"
	for _, tc := range []struct{ key, text, conversation string }{
		{"negative-message", "不要跑 POD7 全套基准", ""},
		{"query-message", "查进度：POD7 的全套基准", ""},
		{"log-message", "[INFO] POD7 全套基准任务", ""},
		{"quote-message", "\x60跑 POD7 全套基准\x60", ""},
		{"task-continuation", natural, taskRun.ConversationID},
	} {
		if _, err := s.AdmitChat(ctx, tc.key, actor, tc.conversation, tc.text, "", false, Delivery{}, nil, true); err != nil {
			t.Fatal(err)
		}
		run, err := s.SubmitRoutedChat(ctx, tc.key, tc.text, "", tc.conversation, Delivery{}, func(call context.Context, _ string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
			if RouterBenchmarkSuggestion(call) != nil {
				t.Fatalf("unsafe text received domain proposal: %s", tc.key)
			}
			return DelegationResult{Route: &RouteDecision{Kind: "benchmark_preview", BenchmarkTarget: "POD7"}}, nil
		})
		if tc.conversation != "" {
			if err != nil || run.TaskID != taskRun.TaskID || run.BenchmarkSuggestion != nil {
				t.Fatalf("normal existing-task follow-up was not preserved: %+v %v", run, err)
			}
			continue
		}
		if !errors.Is(err, ErrClassification) || run.ID != "" {
			t.Fatalf("unsupported model proposal mutated chat: %+v %v", run, err)
		}
	}
	const stale = "stale-published-candidate"
	if _, err := s.AdmitChat(ctx, stale, actor, "", natural, "", false, Delivery{}, nil, true); err != nil {
		t.Fatal(err)
	}
	run, err := s.SubmitRoutedChat(ctx, stale, natural, "", "", Delivery{}, func(call context.Context, _ string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
		if c := RouterBenchmarkSuggestion(call); c == nil || c.Profile.Version != 1 {
			t.Fatalf("expected known initial candidate: %+v", c)
		}
		if err := setBenchmarkTargetAtCurrent(t, s, call, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 2}); err != nil {
			t.Fatal(err)
		}
		return DelegationResult{Route: &RouteDecision{Kind: "benchmark_preview", BenchmarkTarget: "POD7"}}, nil
	})
	if err != nil || run.Status != "completed" || run.BenchmarkSuggestion != nil || run.TaskID != "" {
		t.Fatalf("stale published candidate was replaced silently: %+v %v", run, err)
	}
	valid, handled, err := s.SubmitBenchmarkSuggestionShortcut(ctx, "real-shortcut-receipt", actor, "", "在POD7上跑一下完整的基准测试", "")
	if err != nil || !handled || valid.BenchmarkSuggestion == nil {
		t.Fatalf("could not persist valid suggestion: %+v %t %v", valid, handled, err)
	}
	open := func(who, conversation, run string) (BenchmarkAdmissionPreview, error) {
		return s.PreviewBenchmarkSuggestion(ctx, who, conversation, run,
			BenchmarkSuggestionPreviewInput{Action: "preview_benchmark_suggestion"}, &benchFixtureProvider{})
	}
	for _, pair := range [][2]string{{"user:other", valid.ConversationID}, {actor, taskRun.ConversationID}} {
		if _, err := open(pair[0], pair[1], valid.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("cross-actor/conversation suggested preview disclosed: %v", err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := open(actor, valid.ConversationID, valid.ID); err != nil {
		t.Fatalf("valid receipt denied: %v", err)
	}
	exec("UPDATE authorizations SET revoked_at=? WHERE action='benchmark.execute' AND subject=?", now(), actor)
	if _, err := open(actor, valid.ConversationID, valid.ID); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("cached preview survived grant revocation: %v", err)
	}
	exec("UPDATE authorizations SET revoked_at=0 WHERE action='benchmark.execute' AND subject=?", actor)
	if err := setBenchmarkTargetAtCurrent(t, s, ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := open(actor, valid.ConversationID, valid.ID); !errors.Is(err, ErrBenchmarkAdmissionPreviewStale) {
		t.Fatalf("suggestion silently accepted replaced profile: %v", err)
	}
	if err := setBenchmarkTargetAtCurrent(t, s, ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 2}); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE runs SET created_at=? WHERE id=?", now()-benchmarkRouterSuggestionLifetime.Milliseconds(), valid.ID)
	if _, err := open(actor, valid.ConversationID, valid.ID); !errors.Is(err, ErrBenchmarkAdmissionPreviewStale) {
		t.Fatalf("expired suggestion reopened: %v", err)
	}

	var domainTasks int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&domainTasks); err != nil || domainTasks != 0 {
		t.Fatalf("ordinary chat created a domain task: %d %v", domainTasks, err)
	}
	// Reopening an old, unconfirmed preview may bind its source, but only
	// after this exact owner/conversation/suggestion is verified again.
	exec("UPDATE runs SET created_at=? WHERE id=?", now(), valid.ID)
	exec("UPDATE benchmark_admission_previews SET source_conversation='',source_run=''")
	draft, err := open(actor, valid.ConversationID, valid.ID)
	if err != nil {
		t.Fatalf("could not verify and retain the exact old source: %v", err)
	}
	f := &benchFixtureProvider{}
	accept := BenchmarkAdmissionConfirmationInput{Action: "accept_benchmark_admission"}
	confirm := func() (BenchmarkAdmissionConfirmation, error) {
		return s.ConfirmBenchmarkAdmission(ctx, actor, draft.PreviewID, accept, f)
	}
	exec(`CREATE TRIGGER reject_queue_receipt BEFORE INSERT ON run_events
WHEN NEW.kind='benchmark.admission_queued' BEGIN SELECT RAISE(ABORT,'fixture receipt write failure'); END`)
	if _, err := confirm(); err == nil {
		t.Fatal("failed receipt write nevertheless accepted a task")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&domainTasks); err != nil || domainTasks != 0 {
		t.Fatalf("task enqueue did not roll back: %d %v", domainTasks, err)
	}
	exec("DROP TRIGGER reject_queue_receipt")
	accepted, err := confirm()
	if err != nil || accepted.TaskID == "" {
		t.Fatalf("safe confirmation failed: %+v %v", accepted, err)
	}
	replay, err := confirm()
	if err != nil || replay != accepted {
		t.Fatalf("confirmation replay changed the original receipt: %+v %v", replay, err)
	}
	feed, err := s.ReadConversationEvents(ctx, actor, valid.ConversationID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, item := range feed.Events {
		if item.Kind == benchmarkAdmissionQueuedEvent {
			found++
			if item.TaskID != accepted.TaskID || item.RunID != valid.ID || item.Status != "queued" {
				t.Fatalf("wrong admission projection: %+v", item)
			}
		}
	}
	if found != 1 || f.started != 0 || f.looked != 0 || f.read != 0 {
		t.Fatalf("duplicate or externally dispatched acceptance: %d %+v", found, f)
	}

}
