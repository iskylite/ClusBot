package work

import (
	"context"
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
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
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
		if err := s.SetBenchmarkChatTarget(call, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 2}); err != nil {
			t.Fatal(err)
		}
		return DelegationResult{Route: &RouteDecision{Kind: "benchmark_preview", BenchmarkTarget: "POD7"}}, nil
	})
	if err != nil || run.Status != "completed" || run.BenchmarkSuggestion != nil || run.TaskID != "" {
		t.Fatalf("stale published candidate was replaced silently: %+v %v", run, err)
	}
	var domainTasks int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&domainTasks); err != nil || domainTasks != 0 {
		t.Fatalf("ordinary chat created a domain task: %d %v", domainTasks, err)
	}
}
