package work

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkChatAtomicAndExplicit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "chat-bench.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:session"
	p := BenchmarkProfile{Key: "chat", Version: 1, Cases: []BenchmarkCase{{Key: "test", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	grant := DomainBenchmarkGrant{Connection: "bench-test", Revision: 1, Tool: "test", ExpiresAt: now() + 120000}
	if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	provider := &benchFixtureProvider{}
	if _, handled, err := s.SubmitBenchmarkCommand(ctx, "natural-key", actor, "", "能不能说说基准测试", "", provider); err != nil || handled {
		t.Fatalf("free form chat dispatched a benchmark: %v %v", handled, err)
	}
	run, handled, err := s.SubmitBenchmarkCommand(ctx, "benchmark-key", actor, "", "/benchmark chat@1", "", provider)
	if err != nil || !handled || run.Status != "completed" || !strings.Contains(run.Response, "不代表外部作业已启动") {
		t.Fatalf("bad admission %+v %v", run, err)
	}
	again, _, err := s.SubmitBenchmarkCommand(ctx, "benchmark-key", actor, "", "/benchmark chat@1", "", provider)
	if err != nil || again.ID != run.ID {
		t.Fatalf("duplicate chat created second run %+v %v", again, err)
	}
	if _, _, err = s.SubmitBenchmarkCommand(ctx, "benchmark-key", actor, "", "/benchmark chat@2", "", provider); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency conflict ignored: %v", err)
	}
	var task string
	if err = s.db.QueryRow("SELECT task_id FROM benchmark_runs").Scan(&task); err != nil {
		t.Fatal(err)
	}
	status, handled, err := s.SubmitBenchmarkCommand(ctx, "status-key", actor, run.ConversationID, "/benchmark-status "+task, "", provider)
	if err != nil || !handled || !strings.Contains(status.Response, "0/1") {
		t.Fatalf("bad status %+v %v", status, err)
	}
	if provider.started != 0 {
		t.Fatal("chat handler bypassed queue")
	}
}
