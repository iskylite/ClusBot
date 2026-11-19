package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestBenchmarkDraftKeepsPublishedVersionAndDefaultStable(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "draft.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const key, actor = "full", "user:session"
	testCase := func(key string, deps ...string) BenchmarkCase {
		return BenchmarkCase{Key: key, Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
			Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}, DependsOn: deps}
	}
	initial := BenchmarkProfile{Key: key, Version: 1, Cases: []BenchmarkCase{testCase("bandwidth"), testCase("old"), testCase("deepep", "old")}}
	if err := s.PublishBenchmarkProfile(ctx, actor, initial); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: key, Version: 1}); err != nil {
		t.Fatal(err)
	}
	req := BenchmarkDraftRequest{BaseVersion: 1, Cases: []BenchmarkCase{testCase("deepep", "old"), testCase("bandwidth"), testCase("new")}}
	first, err := s.SaveBenchmarkDraft(ctx, actor, key, req)
	if err != nil || first.ValidationError == "" || !first.Changes.OrderChanged ||
		!slices.Equal(first.Changes.Added, []string{"new"}) || !slices.Equal(first.Changes.Removed, []string{"old"}) {
		t.Fatalf("bad incomplete draft: %+v err=%v", first, err)
	}
	if _, err := s.ReadBenchmarkDraft(ctx, "another", key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other actor saw draft: %v", err)
	}
	if _, err := s.PublishBenchmarkDraft(ctx, actor, key, first.Revision); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("published against removed dependency: %v", err)
	}
	if _, err := s.SaveBenchmarkDraft(ctx, actor, key, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit was not rejected: %v", err)
	}
	req.ExpectedRevision = first.Revision
	req.Cases[0] = testCase("deepep", "new")
	type result struct {
		draft BenchmarkDraft
		err   error
	}
	out := make(chan result, 2)
	for range 2 {
		go func() {
			draft, err := s.SaveBenchmarkDraft(ctx, actor, key, req)
			out <- result{draft, err}
		}()
	}
	var saved BenchmarkDraft
	success, conflict := 0, 0
	for range 2 {
		switch res := <-out; {
		case res.err == nil:
			saved = res.draft
			success++
		case errors.Is(res.err, ErrConflict):
			conflict++
		default:
			t.Fatal(res.err)
		}
	}
	if success != 1 || conflict != 1 || saved.ValidationError != "" || !slices.Equal(saved.Changes.Changed, []string{"deepep"}) {
		t.Fatalf("bad concurrent update: success=%d conflict=%d draft=%+v", success, conflict, saved)
	}
	if _, err := s.PublishBenchmarkDraft(ctx, actor, key, first.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale publication was not rejected: %v", err)
	}
	for range 2 {
		got, err := s.PublishBenchmarkDraft(ctx, actor, key, saved.Revision)
		if err != nil || !got.Published || got.Profile.Version != 2 {
			t.Fatalf("bad publication or retry: %+v err=%v", got, err)
		}
	}
	got, err := readBenchmarkProfile(s.db, key, 1)
	if err != nil || toJSON(got) != toJSON(initial) {
		t.Fatalf("base profile changed: %+v err=%v", got, err)
	}
	var selected int
	if err := s.db.QueryRow("SELECT profile_version FROM benchmark_chat_targets WHERE target='POD7'").Scan(&selected); err != nil || selected != 1 {
		t.Fatalf("publishing changed active default: version=%d err=%v", selected, err)
	}
	req.BaseVersion = 2
	req.ExpectedRevision = saved.Revision
	next, err := s.SaveBenchmarkDraft(ctx, actor, key, req)
	if err != nil || next.Profile.Version != 3 || next.Published {
		t.Fatalf("bad next draft: %+v err=%v", next, err)
	}
	external := next.Profile
	external.Version = 3
	if err := s.PublishBenchmarkProfile(ctx, actor, external); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishBenchmarkDraft(ctx, actor, key, next.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base was not rejected: %v", err)
	}
}
