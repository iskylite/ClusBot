package work

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func setBenchmarkTargetAtCurrent(t *testing.T, s *Store, ctx context.Context, actor, target string, ref *BenchmarkProfileRef) error {
	t.Helper()
	view, err := s.ReadBenchmarkChatTargetState(ctx, actor, target)
	if err != nil {
		return err
	}
	return s.UpdateBenchmarkChatTarget(ctx, actor, target, ref, &view.BindingRevision)
}

func TestBenchmarkChatTargetRevisionAcrossCompetingWindows(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "chat-target-cas.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	const actor = "user:session"
	const target = "POD7"
	view := func() BenchmarkChatTargetState {
		t.Helper()
		out, err := s.ReadBenchmarkChatTargetState(ctx, actor, "pod7")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if zero := view(); zero.State != "disabled" || zero.BindingRevision != 0 {
		t.Fatalf("untouched %+v", zero)
	}
	initial := BenchmarkProfileRef{Profile: "full", Version: 1}
	for version := 1; version <= 2; version++ {
		profile := BenchmarkProfile{Key: initial.Profile, Version: version,
			Cases: []BenchmarkCase{{Key: "case", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
				Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
		if err := s.PublishBenchmarkProfile(ctx, actor, profile); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, target, &initial); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, target, &initial); err != nil {
		t.Fatal(err)
	}
	// Downgrade only the new revision table to exercise the production
	// upgrade path for a v115 store with a published and active selection.
	if _, err := s.db.Exec("DROP TABLE benchmark_chat_target_revisions; PRAGMA user_version=115"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	from := view()
	if from.BindingRevision != 1 || from.State != "bound" || from.BenchmarkProfileRef != initial {
		t.Fatalf("migration %+v", from)
	}
	second := BenchmarkProfileRef{Profile: "full", Version: 2}
	if err := s.SetBenchmarkChatTarget(ctx, actor, target, &second); !errors.Is(err, ErrBenchmarkChatTargetConflict) {
		t.Fatalf("legacy overwrite passed: %v", err)
	}
	if err := s.UpdateBenchmarkChatTarget(ctx, "", target, &second, &from.BindingRevision); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("anonymous write: %v", err)
	}
	ch := make(chan error, 2)
	var wg sync.WaitGroup
	for _, selection := range []*BenchmarkProfileRef{nil, &second} {
		wg.Go(func() { ch <- s.UpdateBenchmarkChatTarget(ctx, actor, target, selection, &from.BindingRevision) })
	}
	wg.Wait()
	close(ch)
	success, conflicts := 0, 0
	for err := range ch {
		if err == nil {
			success++
		} else if errors.Is(err, ErrBenchmarkChatTargetConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	current := view()
	if success != 1 || conflicts != 1 || current.BindingRevision != 2 {
		t.Fatalf("incomplete race: %+v %d %d", current, success, conflicts)
	}
	if current.State == "bound" {
		if current.BenchmarkProfileRef != second {
			t.Fatalf("bad winning binding %+v", current)
		}
		if err := s.UpdateBenchmarkChatTarget(ctx, actor, target, nil, &current.BindingRevision); err != nil {
			t.Fatal(err)
		}
		current = view()
	}
	if current.State != "disabled" {
		t.Fatalf("missing tombstone %+v", current)
	}
	if err := s.SetBenchmarkChatTarget(ctx, actor, target, &initial); !errors.Is(err, ErrBenchmarkChatTargetConflict) {
		t.Fatalf("legacy request reused tombstone: %v", err)
	}
	if err := s.UpdateBenchmarkChatTarget(ctx, actor, target, &initial, &current.BindingRevision); err != nil {
		t.Fatal(err)
	}
	rebound := view()
	if rebound.BindingRevision != current.BindingRevision+1 || rebound.BenchmarkProfileRef != initial {
		t.Fatalf("unexpected rebind %+v", rebound)
	}
	if err := s.UpdateBenchmarkChatTarget(ctx, actor, target, nil, &from.BindingRevision); !errors.Is(err, ErrBenchmarkChatTargetConflict) {
		t.Fatalf("old window cleared a rebound target: %v", err)
	}
	for version := 1; version <= 2; version++ {
		if p, err := s.ReadBenchmarkProfileDetails(ctx, actor, initial.Profile, version, 0); err != nil || p.Profile.Version != version {
			t.Fatalf("published profile changed: %+v %v", p, err)
		}
	}
}
