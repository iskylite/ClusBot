package work

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkOrderLanguage(t *testing.T) {
	for _, text := range []string{"跑 POD7 全套基准", "在 POD7 上跑一下完整的基准测试", "请帮我跑一遍 pod7 的全套基准。", "请在pod7上执行完整基准！"} {
		if got := benchmarkOrderTargetFromText(text); got != "POD7" {
			t.Errorf("%q -> %q; want POD7", text, got)
		}
	}
	for _, text := range []string{
		"能否跑 POD7 全套基准", "跑 POD7 全套基准？", "不要跑 POD7 全套基准", "先别跑 POD7 全套基准",
		"“跑 POD7 全套基准”是什么意思", "跑 POD7 全套基准，SM 改 60", "跑 POD7 全套基准再停掉",
		"跑 POD7 全套基准\n先等我确认", "跑 POD7/POD8 全套基准", "跑 全套基准", "/benchmark full@1",
		"这次是跑 POD7 全套基准，不是让你真跑", "POD7 全套基准进度如何",
	} {
		if got := benchmarkOrderTargetFromText(text); got != "" {
			t.Errorf("unsafe interpretation %q -> %q", text, got)
		}
	}
}

func TestBenchmarkOrderBindingVersionsAndAuthorization(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-order.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:session"
	provider := &benchFixtureProvider{}
	p := BenchmarkProfile{Key: "full", Version: 1, Cases: []BenchmarkCase{{
		Key: "test", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}
	for version := 1; version <= 2; version++ {
		p.Version = version
		if err := s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 3}); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("unpublished version activated: %v", err)
	}
	text := "跑 POD7 全套基准"
	call := func(key, input string) (Run, error) {
		t.Helper()
		run, handled, err := s.SubmitBenchmarkCommand(ctx, key, actor, "", input, "", provider)
		if !handled {
			t.Fatalf("input was not handled: %q", input)
		}
		return run, err
	}
	missing, err := call("missing-plan", text)
	if err != nil || !strings.Contains(missing.Response, "没有创建测试任务") {
		t.Fatalf("unbound scope should clarify: %+v %v", missing, err)
	}
	// Publishing is neither selecting a default nor authorizing a launch.
	if err := s.SetBenchmarkChatTarget(ctx, actor, "pod7", &BenchmarkProfileRef{Profile: "full", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("not-authorized", text); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("unregistered inventory should be rejected: %v", err)
	}
	node := DomainAssetEntry{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1"}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{node}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("not-authorized", text); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("scope without a grant should be rejected: %v", err)
	}
	node.Benchmark = []DomainBenchmarkGrant{{Connection: "bench-test", Revision: 1, Tool: "test", ExpiresAt: now() + 120000}}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{node}); err != nil {
		t.Fatal(err)
	}
	first, err := call("first-direct", text)
	if err != nil || !strings.Contains(first.Response, "full@1") || !strings.Contains(first.Response, "不代表外部作业已启动") {
		t.Fatalf("first run: %+v %v", first, err)
	}
	receipt, err := s.ReadAdmission(ctx, "first-direct")
	if err != nil || receipt.TaskID == "" || receipt.RunID != first.ID || receipt.State != "applied" {
		t.Fatalf("no linked durable receipt: %+v %v", receipt, err)
	}
	if _, _, err := s.SubmitBenchmarkCommand(ctx, "first-direct", "another-user", "", text, "", provider); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong principal replay: %v", err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: "full", Version: 2}); err != nil {
		t.Fatal(err)
	}
	again, err := call("first-direct", text)
	if err != nil || again.ID != first.ID || !strings.Contains(again.Response, "full@1") {
		t.Fatalf("request was reclassified across activation: %+v %v", again, err)
	}
	retriedMissing, err := call("missing-plan", text)
	if err != nil || retriedMissing.ID != missing.ID {
		t.Fatalf("clarification was converted to a launch on replay: %+v %v", retriedMissing, err)
	}
	second, err := call("second-direct", "在 pod7 上跑一下完整的基准测试。")
	if err != nil || !strings.Contains(second.Response, "full@2") {
		t.Fatalf("explicit switch didn't affect the next order: %+v %v", second, err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", nil); err != nil {
		t.Fatal(err)
	}
	disabled, err := call("after-disable", text)
	if err != nil || !strings.Contains(disabled.Response, "没有创建测试任务") {
		t.Fatalf("disabled binding should not run: %+v %v", disabled, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM benchmark_runs").Scan(&count); err != nil || count != 2 || provider.started != 0 {
		t.Fatalf("unexpected effects: tasks=%d starts=%d err=%v", count, provider.started, err)
	}
}
