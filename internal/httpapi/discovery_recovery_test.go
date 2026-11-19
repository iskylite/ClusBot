package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

type blockingDiscoveryFixture struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingDiscoveryFixture) AllowsDiscovery(source, scope string) bool {
	return source == "fixture-discovery" && scope == "POD7"
}
func (f *blockingDiscoveryFixture) ScanDiscovery(ctx context.Context, _, _ string) ([]work.DiscoveryObservation, error) {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return []work.DiscoveryObservation{{Serial: "retired-read", Address: "192.0.2.5"}}, nil
	}
}

func TestDiscoveryRecoveryGuardPrecedesUnavailableModel(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "discovery-http-recovery.sqlite")
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	f := &blockingDiscoveryFixture{started: make(chan struct{}, 1), release: make(chan struct{})}
	const actor = "user:session"
	if _, err := store.GrantDiscovery(ctx, actor, work.DiscoveryGrant{Source: "fixture-discovery", Scope: "POD7", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, f); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishDiscoveryPlan(ctx, actor, work.DiscoveryPlanRequest{ID: "fixture", Source: "fixture-discovery", Scope: "POD7", PeriodSeconds: 60,
		FirstAt: time.Now().Add(150 * time.Millisecond).UnixMilli()}, f); err != nil {
		t.Fatal(err)
	}
	scanCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { store.RunDiscovery(scanCtx, f); close(done) }()
	defer func() { stop(); <-done }()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("finite discovery scan did not begin")
	}
	if err := store.DisableDiscoveryPlan(ctx, actor, "fixture"); err != nil {
		t.Fatal(err)
	}
	close(f.release)
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	var entry work.Task
	for entry.ID == "" {
		items, err := store.ListTasks(ctx, work.TaskFilter{Status: "interrupted"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 1 {
			entry = items[0]
			break
		}
		select {
		case <-timer.C:
			t.Fatal("retired scan did not settle")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	<-done
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverV3(ctx, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	s := Server{Store: store, Settings: &model.Settings{Store: store}, Token: "a1-token", Hosts: []string{"127.0.0.1"}}
	a := &a1Server{store: store, ts: httptest.NewServer(s.Handler(fstest.MapFS{"index.html": {Data: []byte("fixture")}}))}
	defer a.ts.Close()
	a.cookie = a.login()
	for _, body := range []string{`{"revision":2,"client_request_id":"scan-resume"}`, `{"revision":2,"client_request_id":"scan-retry","retry":true}`,
		`{"revision":2,"client_request_id":"scan-reassign","reassign":true}`} {
		code, out := a.post("/api/v1/tasks/"+entry.ID+"/resume", body)
		if code != http.StatusConflict || out["code"] != "discovery_recovery_unsupported" {
			t.Fatalf("wrong no-model recovery rejection: %d %+v", code, out)
		}
	}
	runs, err := store.TaskRuns(ctx, entry.ID, "")
	if err != nil || len(runs.Items) != 0 {
		t.Fatalf("generic HTTP attempt created a Run: %+v %v", runs, err)
	}
	candidates, err := store.ListDiscoveryCandidates(ctx, actor)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("late observation from disabled plan became evidence: %+v %v", candidates, err)
	}
}
