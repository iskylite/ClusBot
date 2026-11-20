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
	for _, lane := range []struct{ actor, scope, current, prior string }{
		{a, "POD7", "192.0.2.70", "192.0.2.7"}, {b, "POD8", "192.0.2.80", "192.0.2.8"},
	} {
		page, err := s.ListDiscoveryCandidateObservations(ctx, lane.actor, c.ID, "", 1)
		if err != nil || len(page.Items) != 1 || page.Items[0].Address != lane.current || page.Items[0].Scope != lane.scope ||
			page.Items[0].Source != "inventory-1" || page.Items[0].AddressConflictInScan || page.NextBefore == "" ||
			page.EvidenceKind != "reported_addresses_only" {
			t.Fatalf("timeline leaked cross-scope data or called a later address a conflict: %+v %v", page, err)
		}
		old, err := s.ListDiscoveryCandidateObservations(ctx, lane.actor, c.ID, page.NextBefore, 1)
		if err != nil || len(old.Items) != 1 || old.NextBefore != "" || old.Items[0].Address != lane.prior ||
			old.Items[0].AddressConflictInScan || old.Items[0].Scope != lane.scope {
			t.Fatalf("wrong immutable timeline: %+v %v", old, err)
		}
	}
	tick, claimed, err := s.claimDiscovery(ctx, f, start+120000)
	if err != nil || !claimed || tick.plan != "scan-a" {
		t.Fatalf("claim conflict scan: %+v %t %v", tick, claimed, err)
	}
	if err := s.settleDiscovery(ctx, tick, []DiscoveryObservation{{"same-serial", "192.0.2.71"}, {"same-serial", "192.0.2.72"}}, nil, start+120000); err != nil {
		t.Fatal(err)
	}
	both, err := s.ListDiscoveryCandidateObservations(ctx, a, c.ID, "", 2)
	if err != nil || len(both.Items) != 2 || !both.Items[0].AddressConflictInScan || !both.Items[1].AddressConflictInScan ||
		both.Items[0].Address == both.Items[1].Address || both.Items[0].ScanTaskID != tick.task || both.Items[1].ScanTaskID != tick.task || both.NextBefore == "" {
		t.Fatalf("same scan's contradictory addresses hidden: %+v %v", both, err)
	}
	if _, err := s.db.Exec("UPDATE authorizations SET revoked_at=? WHERE subject=? AND action='discover'", now(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDiscoveryCandidateObservations(ctx, a, c.ID, both.NextBefore, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked scope still accessible with old cursor: %v", err)
	}
	if _, err := s.ListDiscoveryCandidateObservations(ctx, "other", c.ID, "", 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign candidate disclosed: %v", err)
	}

}

type orderedDiscoveryFixture struct{ discoveryFixture }

func (orderedDiscoveryFixture) AllowsDiscovery(source, scope string) bool {
	return (source == "inventory-1" || source == "inventory-2") && scope == "POD7"
}

func TestDiscoveryDelayedReceiptsDoNotRewindCandidateOrTriggerReverseDrift(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "scan-order.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := orderedDiscoveryFixture{}
	const actor, serial = "operator", "serial-1"
	start := now() + 2000
	if _, err := s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: "inventory-1", Scope: "POD7",
		ExpiresAt: start + 60000, AllowDriftCheck: true}, source); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []struct {
		id     string
		offset int64
	}{
		{"initial", 0}, {"late", 1}, {"a-tie", 2}, {"new", 2},
	} {
		if err := s.PublishDiscoveryPlan(ctx, actor, DiscoveryPlanRequest{ID: plan.id, Source: "inventory-1",
			Scope: "POD7", FirstAt: start + plan.offset, PeriodSeconds: 60, CheckOnAddressDrift: true}, source); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(want string, stamp int64) discoveryTick {
		t.Helper()
		tick, ok, err := s.claimDiscovery(ctx, source, stamp)
		if !ok || err != nil || tick.plan != want {
			t.Fatalf("claim %s: %+v %t %v", want, tick, ok, err)
		}
		return tick
	}
	settle := func(tick discoveryTick, ip string, stamp int64) {
		t.Helper()
		if err := s.settleDiscovery(ctx, tick, []DiscoveryObservation{{serial, ip}}, nil, stamp); err != nil {
			t.Fatal(err)
		}
	}
	read := func() DiscoveryCandidate {
		t.Helper()
		items, err := s.ListDiscoveryCandidates(ctx, actor)
		if err != nil || len(items) != 1 {
			t.Fatalf("candidate: %+v %v", items, err)
		}
		return items[0]
	}
	const initialIP, newerIP, lateIP, conflictingIP = "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"
	settle(claim("initial", start), initialIP, now())
	before := read()
	admitted, err := s.AdmitDiscoveryCandidate(ctx, actor, DiscoveryAdmission{Candidate: before.ID, ConfirmedSerial: serial,
		ExpectedSeenAt: before.SeenAt, Owner: "ops", Location: "rack", Role: "compute"})
	if err != nil {
		t.Fatal(err)
	}
	late := claim("late", start+1)
	tie := claim("a-tie", start+2)
	newer := claim("new", start+2)
	settle(newer, newerIP, start+3)
	settle(late, lateIP, start+4)
	settle(late, lateIP, start+5)
	got := read()
	if got.ID != before.ID || got.Asset != admitted.Asset || got.Address != newerIP || got.State != "admitted" || got.SeenAt != start+3 {
		t.Fatalf("old scan rewound current address or status: %+v", got)
	}
	page, err := s.ListDiscoveryObservations(ctx, actor, late.plan, "", 2)
	if err != nil || len(page.Items) != 1 || page.Items[0].Address != lateIP ||
		page.Items[0].ScheduledAt != late.at || page.Items[0].SeenAt != start+4 {
		t.Fatalf("late evidence lost or duplicated: %+v %v", page, err)
	}
	settle(tie, conflictingIP, start+3)
	got = read()
	if got.Asset != admitted.Asset || got.Address != newerIP || got.State != "conflict" {
		t.Fatalf("same-slot disagreement rewound accepted address or hid conflict: %+v", got)
	}
	var count int
	var oldAddress, newAddress, driftTask string
	if err := s.db.QueryRow(`SELECT count(*),min(previous_address),min(current_address),min(task_id) FROM discovery_drift_checks`).Scan(&count, &oldAddress, &newAddress, &driftTask); err != nil ||
		count != 1 || oldAddress != initialIP || newAddress != newerIP {
		t.Fatalf("incorrect drift count=%d, %s -> %s: %v", count, oldAddress, newAddress, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = guardDiscoveryDriftCheck(tx, driftTask, start+6)
	tx.Rollback()
	if driftTask == "" || !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("superseded drift query still allowed: %q %v", driftTask, err)
	}

	var endpoint string
	if err := s.db.QueryRow("SELECT source_address FROM discovery_admissions WHERE candidate_id=?", before.ID).Scan(&endpoint); err != nil || endpoint != initialIP {
		t.Fatalf("canonical admission endpoint overwritten: %s %v", endpoint, err)
	}
	if _, err := s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: "inventory-2", Scope: "POD7",
		ExpiresAt: start + 60000}, source); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishDiscoveryPlan(ctx, actor, DiscoveryPlanRequest{ID: "other", Source: "inventory-2",
		Scope: "POD7", FirstAt: start + 7, PeriodSeconds: 60}, source); err != nil {
		t.Fatal(err)
	}
	settle(claim("other", start+7), "192.0.2.5", start+8)
	items, err := s.ListDiscoveryCandidates(ctx, actor)
	if err != nil || len(items) != 2 || items[0].ID == before.ID || items[0].Source != "inventory-2" ||
		items[0].Asset != "" || items[1].ID != before.ID || items[1].Address != newerIP {
		t.Fatalf("a different source overwrote same-serial identity: %+v %v", items, err)
	}
}

func TestDiscoveryConflictUsesOnlyValidCanonicalAddresses(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "canonical-address.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := orderedDiscoveryFixture{}
	const actor = "operator"
	start := now() + 2000
	if _, err := s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: "inventory-1", Scope: "POD7", ExpiresAt: start + 60000}, source); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishDiscoveryPlan(ctx, actor, DiscoveryPlanRequest{ID: "canonical", Source: "inventory-1", Scope: "POD7", FirstAt: start, PeriodSeconds: 60}, source); err != nil {
		t.Fatal(err)
	}
	tick, ok, err := s.claimDiscovery(ctx, source, start)
	if !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	batch := []DiscoveryObservation{
		{"aliased", "192.0.2.1"}, {"aliased", "::ffff:192.0.2.1"},
		{"valid", "192.0.2.2"}, {"valid", "malformed"}, {"valid", "::ffff:0.0.0.0"},
		{"different", "192.0.2.3"}, {"different", "::ffff:192.0.2.4"},
		{"invalid-only", "::ffff:224.0.0.1"},
	}
	if err := s.settleDiscovery(ctx, tick, batch, nil, start); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"aliased": "observed", "valid": "observed", "different": "conflict"}
	items, err := s.ListDiscoveryCandidates(ctx, actor)
	if err != nil || len(items) != len(want) {
		t.Errorf("candidates: %+v (%v)", items, err)
	}
	for _, item := range items {
		if item.State != want[item.Serial] {
			t.Errorf("%s state = %q, want %q", item.Serial, item.State, want[item.Serial])
		}
	}
	rows, err := s.db.Query("SELECT ordinal,address FROM discovery_observations ORDER BY ordinal")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	audit := map[int]string{0: "192.0.2.1", 1: "192.0.2.1", 2: "192.0.2.2", 5: "192.0.2.3", 6: "192.0.2.4"}
	for rows.Next() {
		var ordinal int
		var ip string
		if err := rows.Scan(&ordinal, &ip); err != nil {
			t.Fatal(err)
		}
		if ip != audit[ordinal] {
			t.Errorf("audit ordinal %d = %q, want %q", ordinal, ip, audit[ordinal])
		}
		delete(audit, ordinal)
	}
	if err := rows.Err(); err != nil || len(audit) != 0 {
		t.Fatalf("missing audit entries %+v (%v)", audit, err)
	}
}
