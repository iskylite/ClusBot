package work

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

type crossScopeDiscoveryFixture struct{ discoveryFixture }

func (crossScopeDiscoveryFixture) AllowsDiscovery(source, scope string) bool {
	return source == "inventory-1" && (scope == "POD7" || scope == "POD8")
}

func TestDiscoveryCandidateScopeIsolationAndLegacyProvenance(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "candidate-scope.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	f := crossScopeDiscoveryFixture{}
	const a, b = "actor-a", "actor-b"
	start := now() + 2000
	for i, config := range []struct{ actor, scope, plan string }{{a, "POD7", "scan-a"}, {b, "POD8", "scan-b"}} {
		if _, err := s.GrantDiscovery(ctx, config.actor, DiscoveryGrant{Source: "inventory-1", Scope: config.scope, ExpiresAt: start + 180000}, f); err != nil {
			t.Fatal(err)
		}
		if err := s.PublishDiscoveryPlan(ctx, config.actor, DiscoveryPlanRequest{ID: config.plan, Source: "inventory-1", Scope: config.scope, PeriodSeconds: 60, FirstAt: start + int64(i)}, f); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(stamp int64, plan, address string) {
		t.Helper()
		tick, ok, err := s.claimDiscovery(ctx, f, stamp)
		if err != nil || !ok || tick.plan != plan {
			t.Fatalf("claim plan %s: %+v %t %v", plan, tick, ok, err)
		}
		if err := s.settleDiscovery(ctx, tick, []DiscoveryObservation{{"same-serial", address}}, nil, stamp); err != nil {
			t.Fatal(err)
		}
	}
	scan(start, "scan-a", "192.0.2.7")
	scan(start+1, "scan-b", "192.0.2.8")
	raw := func(want string) {
		t.Helper()
		var address string
		if err := s.db.QueryRow("SELECT address FROM discovery_candidates").Scan(&address); err != nil || address != want {
			t.Fatalf("canonical candidate physical address: %q, %v (want %q)", address, err, want)
		}
	}
	raw("192.0.2.7")
	// Simulate the pre-101 overwritten projection. On reopen, the scope
	// associated with the receipt, not the overwritable record, controls reads.
	if _, err := s.db.Exec("UPDATE discovery_candidates SET address='192.0.2.8',seen_at=seen_at+1,state='observed'; DROP INDEX discovery_observations_by_candidate; PRAGMA user_version=100"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var index bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='discovery_observations_by_candidate')").Scan(&index); err != nil || !index {
		t.Fatalf("missing upgraded lookup index: %t %v", index, err)
	}
	items, err := s.ListDiscoveryCandidates(ctx, a)
	if err != nil || len(items) != 1 || items[0].Address != "192.0.2.7" || items[0].SeenAt != start || items[0].State != "conflict" {
		t.Fatalf("wrong scoped candidate: %+v %v", items, err)
	}
	foreign, err := s.ListDiscoveryCandidates(ctx, b)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("other scope list revealed candidate: %+v %v", foreign, err)
	}
	c := items[0]
	req := DiscoveryAdmission{Candidate: c.ID, ConfirmedSerial: c.Serial, ExpectedSeenAt: c.SeenAt, Owner: "ops", Location: "rack", Role: "compute"}
	if out, err := s.AdmitDiscoveryCandidate(ctx, b, req); !errors.Is(err, sql.ErrNoRows) || out != (DiscoveryCandidate{}) {
		t.Fatalf("foreign admission leaked metadata: %+v %v", out, err)
	}
	if _, err := s.AdmitDiscoveryCandidate(ctx, a, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous cross-scope candidate admitted: %v", err)
	}
	history, err := s.ListDiscoveryObservations(ctx, b, "scan-b", "", 2)
	if err != nil || len(history.Items) != 1 || history.Items[0].Address != "192.0.2.8" || history.Items[0].CandidateID != c.ID {
		t.Fatalf("B receipt should stay visible only in B, preserving serial identity: %+v %v", history, err)
	}
	scan(start+60000, "scan-a", "192.0.2.70")
	scan(start+60001, "scan-b", "192.0.2.80")
	raw("192.0.2.70")
}
