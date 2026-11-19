package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestBenchmarkChatConfirmationUsesOnlyRecordedProposal(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "confirmed-preview.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &benchFixtureProvider{}
	const actor = "user:session"
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: binding.Connection, Revision: 1, Tool: binding.Tool, ExpiresAt: now() + 60000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "serial-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	cases := []BenchmarkCase{}
	for _, key := range []string{"first", "second"} {
		cases = append(cases, BenchmarkCase{Key: key, Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}})
	}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "profile", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"confirmed-plan", "profile", 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	chat, _, err := s.SubmitBenchmarkCommand(ctx, "confirmed-chat", actor, "", "/benchmark-status "+task, "", p)
	if err != nil {
		t.Fatal(err)
	}
	const spoken = "第二个测试先别跑"
	preview, err := s.SubmitBenchmarkRevisionChat(ctx, actor, chat.ConversationID, BenchmarkRevisionChatInput{
		BenchmarkRevisionAIInput: BenchmarkRevisionAIInput{BenchmarkRevisionProposalInput: BenchmarkRevisionProposalInput{ExpectedRevision: 1, Text: spoken}},
		Action:                   "preview_benchmark_revision", TaskID: task, RequestID: "confirmed-preview",
	}, p, func(context.Context, BenchmarkRevisionModelInput, int) (string, error) {
		return `{"action":"remove_case","case_key":"second","quote":"第二个测试先别跑"}`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, err := s.ReadBenchmarkRevisionRequestState(ctx, actor, chat.ConversationID, "confirmed-preview"); err != nil ||
		status.State != "completed" || status.RunID != preview.RunID || status.CanRestart {
		t.Fatalf("completed request status: %+v %v", status, err)
	}
	makeRequest := func(key string) BenchmarkRevisionChatInput {
		return BenchmarkRevisionChatInput{
			BenchmarkRevisionAIInput: BenchmarkRevisionAIInput{BenchmarkRevisionProposalInput: BenchmarkRevisionProposalInput{ExpectedRevision: 1, Text: spoken}},
			Action:                   "preview_benchmark_revision", TaskID: task, RequestID: key,
		}
	}
	checkInterrupted := func(key string) {
		t.Helper()
		status, err := s.ReadBenchmarkRevisionRequestState(ctx, actor, chat.ConversationID, key)
		if err != nil || status.State != "interrupted" || !status.CanRestart || status.RunID != "" {
			t.Fatalf("interrupted request not recoverable: %+v %v", status, err)
		}
		if _, err := s.SubmitBenchmarkRevisionChat(ctx, actor, chat.ConversationID, makeRequest(key), p,
			func(context.Context, BenchmarkRevisionModelInput, int) (string, error) {
				t.Fatal("interrupted idempotency key was sent to the model again")
				return "", nil
			}); !errors.Is(err, ErrBenchmarkRevisionRequestInterrupted) {
			t.Fatalf("interrupted repeat had no explicit state: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	if _, err := s.SubmitBenchmarkRevisionChat(cancelled, actor, chat.ConversationID, makeRequest("intent-cancelled"), p,
		func(call context.Context, _ BenchmarkRevisionModelInput, _ int) (string, error) {
			cancel()
			<-call.Done()
			return "", call.Err()
		}); !errors.Is(err, ErrBenchmarkRevisionModel) {
		t.Fatalf("cancelled model attempt produced a result: %v", err)
	}
	checkInterrupted("intent-cancelled")
	if _, err := s.SubmitBenchmarkRevisionChat(ctx, actor, chat.ConversationID, makeRequest("intent-restarted"), p,
		func(_ context.Context, _ BenchmarkRevisionModelInput, _ int) (string, error) {
			// Represent a process exit after the provider may have accepted
			// the call, but before the original transaction saved a result.
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			status, err := reopened.ReadBenchmarkRevisionRequestState(ctx, actor, chat.ConversationID, "intent-restarted")
			if err != nil || status.State != "running" || status.CanRestart {
				t.Fatalf("missing durable pre-model boundary: %+v %v", status, err)
			}
			for _, count := range []int{1, 0} {
				report, err := reopened.RecoverV3(ctx, now())
				if err != nil || report.InterruptedBenchmarkPreviews != count {
					t.Fatalf("restart recovery not idempotent: %+v %v", report, err)
				}
			}
			// A synthetic late receipt must not be able to overwrite the
			// recovery decision even if the former worker remains alive.
			return `{"action":"remove_case","case_key":"second","quote":"第二个测试先别跑"}`, nil
		}); !errors.Is(err, ErrBenchmarkRevisionRequestInterrupted) {
		t.Fatalf("late result overrode crash recovery: %v", err)
	}
	checkInterrupted("intent-restarted")
	if _, err := s.ReadBenchmarkRevisionRequestState(ctx, "other", chat.ConversationID, "intent-restarted"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign actor saw interrupted idempotency state: %v", err)
	}

	confirm := BenchmarkRevisionChatConfirmInput{Action: "apply_benchmark_revision", ExpectedRevision: 1}
	call := func(subject, c, r string, in BenchmarkRevisionChatConfirmInput, provider BenchmarkProvider) (BenchmarkRevisionChatConfirmation, error) {
		return s.ConfirmBenchmarkRevisionChat(ctx, subject, c, r, in, provider)
	}
	for _, attempt := range []struct{ actor, conversation, run string }{
		{"other", chat.ConversationID, preview.RunID}, {actor, id(), preview.RunID}, {actor, chat.ConversationID, chat.ID},
	} {
		if _, err := call(attempt.actor, attempt.conversation, attempt.run, confirm, p); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("wrong history identity was accepted: %v", err)
		}
	}
	if _, err := call(actor, chat.ConversationID, preview.RunID, BenchmarkRevisionChatConfirmInput{Action: "preview_benchmark_revision", ExpectedRevision: 1}, p); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("preview was silently treated as confirmation: %v", err)
	}
	if _, err := call(actor, chat.ConversationID, preview.RunID, BenchmarkRevisionChatConfirmInput{Action: "apply_benchmark_revision", ExpectedRevision: 2}, p); !errors.Is(err, ErrBenchmarkRevisionPreviewStale) {
		t.Fatalf("wrong expected revision: %v", err)
	}
	if _, err := s.ReviseBenchmark(ctx, actor, task, BenchmarkRevisionInput{ExpectedRevision: 1, RequestKey: benchmarkChatConfirmationPrefix + preview.RunID, Cases: cases[:1]}, p, true); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("raw apply reserved confirmation key: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE benchmark_cases SET phase='submitting' WHERE task_id=? AND case_key='second'", task)
	if _, err := call(actor, chat.ConversationID, preview.RunID, confirm, p); !errors.Is(err, ErrBenchmarkRevisionPreviewStale) {
		t.Fatalf("confirmation can change dispatched work: %v", err)
	}
	exec("UPDATE benchmark_cases SET phase='queued' WHERE task_id=? AND case_key='second'", task)
	exec("UPDATE authorizations SET revoked_at=? WHERE action='benchmark.execute' AND subject=?", now(), actor)
	if _, err := call(actor, chat.ConversationID, preview.RunID, confirm, p); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked authorization can apply: %v", err)
	}
	exec("UPDATE authorizations SET revoked_at=0 WHERE action='benchmark.execute' AND subject=?", actor)
	if _, err := call(actor, chat.ConversationID, preview.RunID, confirm, nil); !errors.Is(err, ErrBenchmarkRevisionPreviewStale) {
		t.Fatalf("missing current capability bypassed: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_run_revisions WHERE task_id=?", task).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unconfirmed change was stored: %d %v", n, err)
	}

	outputs := make(chan BenchmarkRevisionChatConfirmation, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			out, err := call(actor, chat.ConversationID, preview.RunID, confirm, p)
			outputs <- out
			errs <- err
		})
	}
	wg.Wait()
	close(outputs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for out := range outputs {
		if out.RunID != preview.RunID || out.State != "applied" || out.Revision != 2 {
			t.Fatalf("wrong receipt: %+v", out)
		}
	}
	replay, err := call(actor, chat.ConversationID, preview.RunID, confirm, nil)
	if err != nil || replay.Revision != 2 {
		t.Fatalf("repeat should return minimal committed receipt: %+v %v", replay, err)
	}
	plan, err := s.ReadBenchmarkRevision(ctx, actor, task, 0)
	if err != nil || plan.Revision != 2 || len(plan.Profile.Cases) != 1 || plan.Profile.Cases[0].Key != "first" || plan.AssetImpact != nil {
		t.Fatalf("unexpected revision applied: %+v %v", plan, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM runtime_audit WHERE task_id=? AND run_id=? AND event='benchmark_task_revised'", task, preview.RunID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expected one atomic audit receipt: %d %v", n, err)
	}
	if p.started != 0 || p.looked != 0 || p.read != 0 {
		t.Fatal("confirmation must not itself contact an external provider")
	}
}
