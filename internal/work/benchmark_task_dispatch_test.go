package work

import (
	"database/sql"
	"errors"
	"testing"
)

func taskDispatchInput(action, key string, version int) BenchmarkTaskDispatchInput {
	return BenchmarkTaskDispatchInput{Action: action, ExpectedPlanRevision: 1, ExpectedControlVersion: &version, RequestID: key}
}

func TestBenchmarkTaskDispatchPausePersistenceReconciliationAndResume(t *testing.T) {
	ctx := t.Context()
	s, path, task, _, provider := newDispatchGateFixture(t, "first", "dependent", "sibling")
	read := func() BenchmarkRunProgress {
		t.Helper()
		p, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil {
		t.Fatalf("initial dispatch: %t %v", ok, err)
	}
	inFlight := read().Cases[0]
	if inFlight.Operation == nil || inFlight.Wait == nil || inFlight.TargetHolds != 1 || provider.started != 1 {
		t.Fatalf("missing in-flight work: %+v", inFlight)
	}
	pause := taskDispatchInput("pause", "pause-task-request", 0)
	if _, err := s.ControlBenchmarkTaskDispatch(ctx, "another-user", task, pause, provider); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign control: %v", err)
	}
	receipt, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, pause, provider)
	if err != nil || receipt.State != "paused" || receipt.ControlVersion != 1 {
		t.Fatalf("pause: %+v %v", receipt, err)
	}
	if replay, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, pause, provider); err != nil || replay != receipt {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	changed := pause
	changed.Action = "resume"
	if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, changed, provider); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := read(); got.State != "paused" || !got.DispatchControl.Paused || got.DispatchControl.ControlVersion != 1 || got.Cases[0].TargetHolds != 1 {
		t.Fatalf("pause not durable: %+v", got)
	}
	if stored, err := s.ReadBenchmarkTaskDispatchReceipt(ctx, dispatchGateFixtureActor, task, pause.RequestID); err != nil || stored != receipt {
		t.Fatalf("stored receipt: %+v %v", stored, err)
	}
	if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil {
		t.Fatalf("reconcile in flight: %t %v", ok, err)
	}
	after := read()
	if provider.read != 1 || provider.started != 1 || after.Cases[0].Phase != "verified" || after.Cases[0].TargetHolds != 0 || after.Cases[1].Phase != "queued" || after.Cases[2].Phase != "queued" {
		t.Fatalf("paused reconciliation: provider=%+v progress=%+v", provider, after)
	}
	if ok, err := s.runBenchmarkOnce(ctx, provider); ok || err != nil || provider.started != 1 {
		t.Fatalf("paused task dispatched new work: %t %v %+v", ok, err, provider)
	}
	resume := taskDispatchInput("resume", "resume-task-request", 1)
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='benchmark.execute'", now(), dispatchGateFixtureActor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, resume, provider); !errors.Is(err, ErrAuthorizationDenied) || !read().DispatchControl.Paused {
		t.Fatalf("resume ignored revoked grant: %v", err)
	}
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=0 WHERE subject=? AND action='benchmark.execute'", dispatchGateFixtureActor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE ops_assets SET frozen_at=? WHERE object_ref='node-1'", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, resume, provider); !errors.Is(err, ErrState) || !read().DispatchControl.Paused {
		t.Fatalf("resume ignored frozen target: %v", err)
	}
	if _, err := s.db.Exec("UPDATE ops_assets SET frozen_at=0 WHERE object_ref='node-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, resume, nil); !errors.Is(err, ErrBenchmarkBinding) || !read().DispatchControl.Paused {
		t.Fatalf("resume ignored unavailable provider: %v", err)
	}
	if got, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, resume, provider); err != nil || got.State != "resumed" || got.ControlVersion != 2 {
		t.Fatalf("resume: %+v %v", got, err)
	}
	if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil || provider.started != 2 {
		t.Fatalf("resumed work not dispatched: %t %v %+v", ok, err, provider)
	}
}

func TestBenchmarkTaskDispatchPauseRaceAndStop(t *testing.T) {
	ctx := t.Context()
	t.Run("pause commits first", func(t *testing.T) {
		s, _, task, _, provider := newDispatchGateFixture(t, "first")
		if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, taskDispatchInput("pause", "pause-before-claim", 0), provider); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.runBenchmarkOnce(ctx, provider); ok || err != nil || provider.started != 0 {
			t.Fatalf("claim passed pause: %t %v", ok, err)
		}
		var revision int
		if err := s.db.QueryRow("SELECT revision FROM tasks WHERE id=?", task).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if err := s.Control(ctx, task, "stop", revision); err != nil {
			t.Fatalf("stop while paused: %v", err)
		}
		if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, taskDispatchInput("resume", "resume-after-stop", 1), provider); !errors.Is(err, ErrConflict) {
			t.Fatalf("resume after stop: %v", err)
		}
		if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil || provider.started != 0 {
			t.Fatalf("stop failed to drain: %t %v", ok, err)
		}
		if got, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task); err != nil || got.State != "cancelled" || got.Cases[0].Phase != "skipped" {
			t.Fatalf("stop result: %+v %v", got, err)
		}
	})
	t.Run("claim commits first", func(t *testing.T) {
		s, _, task, _, provider := newDispatchGateFixture(t, "first")
		d, ok, err := s.claimBenchmark(ctx, provider, now())
		if err != nil || !ok || d.action != "start" {
			t.Fatalf("claim: %+v %t %v", d, ok, err)
		}
		if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, taskDispatchInput("pause", "pause-after-claim", 0), provider); err != nil {
			t.Fatal(err)
		}
		got, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
		if err != nil || got.Cases[0].Operation == nil || got.Cases[0].TargetHolds != 1 {
			t.Fatalf("claimed operation lost: %+v %v", got, err)
		}
		job, err := provider.Start(ctx, d.request)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.settleBenchmark(ctx, d, job, BenchmarkJobResult{}, nil, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.runBenchmarkOnce(ctx, provider); !ok || err != nil || provider.started != 1 || provider.read != 1 {
			t.Fatalf("claimed job not reconciled: %t %v %+v", ok, err, provider)
		}
		if got, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task); err != nil || got.State != "paused" || got.Cases[0].Phase != "verified" {
			t.Fatalf("settled while paused: %+v %v", got, err)
		}
		if _, err := s.ControlBenchmarkTaskDispatch(ctx, dispatchGateFixtureActor, task, taskDispatchInput("resume", "resume-settled-task", 1), provider); err != nil {
			t.Fatalf("finalize after resume: %v", err)
		}
		if got, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task); err != nil || got.State != "succeeded" {
			t.Fatalf("resume did not finalize: %+v %v", got, err)
		}
	})
}
