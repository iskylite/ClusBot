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

func TestStartBenchmarkDraftFromExactPublishedSnapshot(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "clone-published.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, key = "user:session", "published-workflow"
	binding := BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1}
	base := BenchmarkProfile{Key: key, Version: 1, Cases: []BenchmarkCase{
		{Key: "first", Binding: binding, Targets: []string{"node-1"}, Parameters: map[string]string{"gpu_count": "40"}, ActualConfig: map[string]string{"gpu_count": "40"}, RequiredMetrics: map[string]string{"rate": "GB/s"}},
		{Key: "last", Binding: binding, Targets: []string{"node-1"}, DependsOn: []string{"first"},
			Parameters: map[string]string{"gpu_count": "40"}, ActualConfig: map[string]string{"gpu_count": "40"},
			RequiredMetrics: map[string]string{"rate": "GB/s"}, ComparisonKeys: []string{"gpu_count"},
			NeedsRawEvidence: true, Threshold: &BenchmarkMetricThreshold{"rate", 128}},
	}}
	if _, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unpublished clone: %v", err)
	}
	if err := s.PublishBenchmarkProfile(ctx, actor, base); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, "POD7", &BenchmarkProfileRef{Profile: key, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture",
		Benchmark: []DomainBenchmarkGrant{{Connection: binding.Connection, Revision: binding.Revision, Tool: binding.Tool,
			Allowed: map[string][]string{"gpu_count": {"40"}}, ExpiresAt: now() + 120000}}}}); err != nil {
		t.Fatal(err)
	}
	p := &benchFixtureProvider{}
	old, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"original-frozen", key, 1}, p)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		draft BenchmarkDraft
		err   error
	}
	windows := make(chan result, 2)
	for range 2 {
		go func() {
			draft, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, 0)
			windows <- result{draft, err}
		}()
	}
	var start BenchmarkDraft
	var successes, conflicts int
	for range 2 {
		result := <-windows
		switch {
		case result.err == nil:
			successes++
			start = result.draft
		case errors.Is(result.err, ErrConflict):
			conflicts++
		default:
			t.Fatal(result.err)
		}
	}
	if successes != 1 || conflicts != 1 || start.Revision != 1 || start.BaseVersion != 1 || start.Profile.Version != 2 ||
		start.Published || start.ValidationError != "" || toJSON(start.Profile.Cases) != toJSON(base.Cases) ||
		len(start.Changes.Added)+len(start.Changes.Removed)+len(start.Changes.Changed) != 0 || start.Changes.OrderChanged {
		t.Fatalf("clone not exact or concurrent overwrite: %+v successes=%d conflicts=%d", start, successes, conflicts)
	}
	recovered, err := s.ReadBenchmarkDraft(ctx, actor, key)
	if err != nil || toJSON(recovered) != toJSON(start) {
		t.Fatalf("lost receipt draft cannot be recovered: %+v %v", recovered, err)
	}
	if _, err := s.ReadBenchmarkDraft(ctx, "another-actor", key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("actor cannot read other draft, got %v", err)
	}
	if _, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, start.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("unpublished draft replaced: %v", err)
	}
	if _, err := s.ReadBenchmarkProfileDetails(ctx, actor, key, 2, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("starting a draft published a new version: %v", err)
	}
	start.Profile.Cases[1].Threshold.Minimum = 130
	edited, err := s.SaveBenchmarkDraft(ctx, actor, key, BenchmarkDraftRequest{ExpectedRevision: start.Revision, BaseVersion: 1, Cases: start.Profile.Cases})
	if err != nil || edited.Published || !slices.Equal(edited.Changes.Changed, []string{"last"}) {
		t.Fatalf("edit did not use existing CAS/diff: %+v %v", edited, err)
	}
	if _, err := s.PublishBenchmarkDraft(ctx, actor, key, start.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale publish accepted: %v", err)
	}
	published, err := s.PublishBenchmarkDraft(ctx, actor, key, edited.Revision)
	if err != nil || !published.Published || published.Profile.Version != 2 {
		t.Fatalf("separate publish failed: %+v %v", published, err)
	}
	if _, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, published.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale published base accepted: %v", err)
	}
	again, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 2, published.Revision)
	if err != nil || again.Published || again.Profile.Version != 3 || again.Profile.Cases[1].Threshold.Minimum != 130 {
		t.Fatalf("starting the next exact version failed: %+v %v", again, err)
	}
	selected, err := s.ReadBenchmarkChatTarget(ctx, actor, "POD7")
	if err != nil || selected.Version != 1 {
		t.Fatalf("draft/publish activated a default: %+v %v", selected, err)
	}
	frozen, err := s.ReadBenchmarkRevision(ctx, actor, old, 0)
	if err != nil || frozen.Revision != 1 || toJSON(frozen.Profile) != toJSON(base) || p.started != 0 {
		t.Fatalf("authoring changed live task snapshot or dispatched: %+v %v", frozen, err)
	}
}
