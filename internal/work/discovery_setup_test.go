package work

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestDiscoverySetupStatusUsesAuthorizedPlanAndDurableOccurrence(t *testing.T) {
	ctx := t.Context()
	name := filepath.Join(t.TempDir(), "discovery-status.db")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	source := discoveryFixture{}
	const actor, key, scope = "user:session", "inventory-1", "POD7"
	read := func(as string, adapter DiscoveryScanSource, filter, want string) DiscoverySetupStatus {
		t.Helper()
		status, err := s.ReadDiscoverySetupStatus(ctx, as, adapter, key, filter)
		if err != nil || status.State != want {
			t.Fatalf("status = %+v, %v; want %s", status, err, want)
		}
		return status
	}
	read(actor, nil, scope, "source_unavailable")
	read(actor, source, "not-configured", "source_unavailable")
	read(actor, source, scope, "no_active_plan")
	authorization, err := s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: key, Scope: scope, ExpiresAt: now() + 600_000}, source)
	if err != nil {
		t.Fatal(err)
	}
	due := now() + 10_000
	plan := DiscoveryPlanRequest{ID: "init", Source: key, Scope: scope, PeriodSeconds: 60, FirstAt: due}
	if err := s.PublishDiscoveryPlan(ctx, actor, plan, source); err != nil {
		t.Fatal(err)
	}
	if status := read(actor, source, scope, "waiting_for_result"); status.Plan == nil || status.Plan.ID != plan.ID || status.Plan.NextAt != due ||
		status.Plan.PeriodSeconds != 60 || status.Plan.Revision != 1 || status.LastRun != nil {
		t.Fatalf("unexpected initial state: %+v", status)
	}
	if status := read("user:someone", source, scope, "no_active_plan"); status.Plan != nil || status.LastRun != nil {
		t.Fatal("another actor received plan details")
	}
	claim := func(stamp int64) discoveryTick {
		t.Helper()
		occurrence, ok, err := s.claimDiscovery(ctx, source, stamp)
		if err != nil || !ok || occurrence.task == "" {
			t.Fatalf("claim = %+v %v %v", occurrence, ok, err)
		}
		return occurrence
	}
	first := claim(due)
	if status := read(actor, source, scope, "waiting_for_result"); status.LastRun == nil || status.LastRun.Status != "running" || status.LastRun.ObservationCount != nil {
		t.Fatalf("unexpected running state: %+v", status)
	}
	if err := s.settleDiscovery(ctx, first, nil, nil, due+1); err != nil {
		t.Fatal(err)
	}
	if status := read(actor, source, scope, "no_observations"); status.LastRun.ObservationCount == nil || *status.LastRun.ObservationCount != 0 {
		t.Fatalf("missing explicit zero observation count: %+v", status)
	}
	second := claim(due + 60_000)
	if err := s.settleDiscovery(ctx, second, []DiscoveryObservation{{Serial: "fixture", Address: "192.0.2.4"}}, nil, due+60_001); err != nil {
		t.Fatal(err)
	}
	if status := read(actor, source, scope, "observations_available"); status.LastRun.ObservationCount == nil || *status.LastRun.ObservationCount != 1 {
		t.Fatalf("incorrect observation count: %+v", status)
	}
	third := claim(due + 120_000)
	if err := s.settleDiscovery(ctx, third, nil, errors.New("synthetic source failure"), due+120_001); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if status := read(actor, source, scope, "run_failed"); status.LastRun.ObservationCount != nil {
		t.Fatalf("failed scan reported a successful observation count: %+v", status)
	}
	if _, _, err := s.claimDiscovery(ctx, source, due+240_000); err != nil {
		t.Fatal(err)
	}
	read(actor, source, scope, "run_skipped")
	if _, err := s.db.ExecContext(ctx, "UPDATE authorizations SET revoked_at=? WHERE auth_id=?", now(), authorization); err != nil {
		t.Fatal(err)
	}
	read(actor, source, scope, "no_active_plan")
}
