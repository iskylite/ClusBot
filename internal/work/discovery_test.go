package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

type discoveryFixture struct{}

func (discoveryFixture) AllowsDiscovery(source, scope string) bool {
	return source == "inventory-1" && scope == "POD7"
}
func (discoveryFixture) ScanDiscovery(context.Context, string, string) ([]DiscoveryObservation, error) {
	panic("drive the boundaries explicitly")
}

func TestV31DiscoveryDueRestartAdmission(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "discovery.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	source := discoveryFixture{}
	actor := "user:session"
	auth, err := s.GrantDiscovery(ctx, actor, DiscoveryGrant{Source: "inventory-1", Scope: "POD7", ExpiresAt: now() + 600000, AllowAdmissionCheck: true, AllowDriftCheck: true}, source)
	if err != nil {
		t.Fatal(err)
	}
	due := now() + 2000
	if err = s.PublishDiscoveryPlan(ctx, actor, DiscoveryPlanRequest{ID: "daily", Source: "inventory-1", Scope: "POD7", PeriodSeconds: 60, FirstAt: due, CheckAfterAdmit: true, CheckOnAddressDrift: true}, source); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.claimDiscovery(ctx, source, due-1); ok || err != nil {
		t.Fatalf("ran before due: %v %v", ok, err)
	}
	first, ok, err := s.claimDiscovery(ctx, source, due)
	if err != nil || !ok || first.task == "" {
		t.Fatalf("did not create finite discovery task %+v %v", first, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	replay, ok, err := s.claimDiscovery(ctx, source, due+30001)
	if err != nil || !ok || replay.task != first.task || replay.generation <= first.generation {
		t.Fatalf("recovery lost or recreated task %+v %v", replay, err)
	}
	observations := []DiscoveryObservation{{"SN-1", "192.0.2.1"}, {"SN-2", "192.0.2.1"}, {"", "192.0.2.2"}}
	if err = s.settleDiscovery(ctx, first, observations, nil, now()); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.ListDiscoveryCandidates(ctx, actor)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("old generation wrote observations: %+v %v", candidates, err)
	}
	if err = s.settleDiscovery(ctx, replay, observations, nil, now()); err != nil {
		t.Fatal(err)
	}
	candidates, err = s.ListDiscoveryCandidates(ctx, actor)
	if err != nil || len(candidates) != 3 {
		t.Fatalf("IP-based false merge %+v %v", candidates, err)
	}
	var node, ipOnly DiscoveryCandidate
	for _, c := range candidates {
		if c.Serial == "SN-1" {
			node = c
		}
		if c.Serial == "" {
			ipOnly = c
		}
	}
	req := DiscoveryAdmission{Candidate: ipOnly.ID, ConfirmedSerial: "invented", ExpectedSeenAt: ipOnly.SeenAt, Owner: "ops", Location: "rack7", Role: "compute"}
	if _, err = s.AdmitDiscoveryCandidate(ctx, actor, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("IP alone admitted: %v", err)
	}
	req.Candidate, req.ConfirmedSerial, req.ExpectedSeenAt = node.ID, node.Serial, node.SeenAt
	admitted, err := s.AdmitDiscoveryCandidate(ctx, actor, req)
	if err != nil || admitted.Asset == "" || admitted.CheckTask == "" {
		t.Fatalf("admission did not enqueue check: %+v %v", admitted, err)
	}
	again, err := s.AdmitDiscoveryCandidate(ctx, actor, req)
	if err != nil || again.Asset != admitted.Asset || again.CheckTask != admitted.CheckTask {
		t.Fatalf("repeat admission created new records: %+v %v", again, err)
	}
	var assets int
	if err = s.db.QueryRow("SELECT count(*) FROM ops_assets").Scan(&assets); err != nil || assets != 1 {
		t.Fatalf("wrong asset count %d %v", assets, err)
	}
	var deployed DiscoveryCandidate
	for _, c := range candidates {
		if c.Serial == "SN-2" {
			deployed = c
		}
	}
	deployed, err = s.AdmitDiscoveryCandidate(ctx, actor, DiscoveryAdmission{Candidate: deployed.ID, ConfirmedSerial: deployed.Serial,
		ExpectedSeenAt: deployed.SeenAt, Owner: "ops", Location: "rack7", Role: "compute"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE ops_assets SET state=? WHERE object_ref=?", AssetStateInService, deployed.Asset); err != nil {
		t.Fatal(err)
	}

	next, ok, err := s.claimDiscovery(ctx, source, due+60000)
	if err != nil || !ok || next.task == first.task {
		t.Fatalf("next tick %+v %v", next, err)
	}
	if err = s.settleDiscovery(ctx, next, []DiscoveryObservation{{"SN-1", "192.0.2.3"}, {"SN-2", "192.0.2.4"}}, nil, now()); err != nil {
		t.Fatal(err)
	}
	candidates, err = s.ListDiscoveryCandidates(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.Serial == "SN-1" && (c.ID != node.ID || c.Asset != admitted.Asset || c.Address != "192.0.2.3") {
			t.Fatalf("address move changed identity %+v", c)
		}
	}
	page, err := s.ListDiscoveryObservations(ctx, actor, "daily", "", 2)
	if err != nil || page.PlanID != "daily" || page.Source != "inventory-1" || page.Scope != "POD7" || page.NextBefore == "" || len(page.Items) != 2 {
		t.Fatalf("observation provenance page: %+v %v", page, err)
	}
	if item := page.Items[1]; item.Address != "192.0.2.3" || item.CandidateID != node.ID || item.ScanTaskID != next.task || item.PlanRevision != next.revision {
		t.Fatalf("latest immutable observation: %+v", item)
	}
	historical, err := s.ListDiscoveryObservations(ctx, actor, "daily", page.NextBefore, 3)
	if err != nil || len(historical.Items) != 3 || historical.NextBefore != "" ||
		historical.Items[2].CandidateID != node.ID || historical.Items[2].Address != "192.0.2.1" || historical.Items[2].ScanTaskID != first.task {
		t.Fatalf("history should not reflect current candidate address: %+v %v", historical, err)
	}
	if _, err := s.GrantDiscovery(ctx, "other", DiscoveryGrant{Source: "inventory-1", Scope: "POD7", ExpiresAt: now() + 600000}, source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDiscoveryObservations(ctx, "other", "daily", page.NextBefore, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a grant alone must not authorize another actor's plan: %v", err)
	}
	if _, err := s.ListDiscoveryObservations(ctx, actor, "daily", "bad:cursor", 3); !errors.Is(err, ErrDiscoveryInput) {
		t.Fatalf("invalid cursor: %v", err)
	}

	var driftChecks, goals int
	if err = s.db.QueryRow("SELECT count(*) FROM discovery_drift_checks WHERE task_id IS NOT NULL").Scan(&driftChecks); err != nil || driftChecks != 2 {
		t.Fatalf("known undelivered and delivered assets both require read-only checks: %d %v", driftChecks, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM goals").Scan(&goals); err != nil || goals != 0 {
		t.Fatalf("discovery should not create Goal: %d %v", goals, err)
	}
	repeat, ok, err := s.claimDiscovery(ctx, source, due+120000)
	if err != nil || !ok {
		t.Fatalf("repeat tick: %+v %v", repeat, err)
	}
	if err = s.settleDiscovery(ctx, repeat, []DiscoveryObservation{{"SN-1", "192.0.2.3"}, {"SN-2", "192.0.2.4"}}, nil, now()); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM discovery_drift_checks").Scan(&driftChecks); err != nil || driftChecks != 2 {
		t.Fatalf("same observation caused extra follow-up: %d %v", driftChecks, err)
	}
	var driftTask string
	if err = s.db.QueryRow("SELECT task_id FROM discovery_drift_checks WHERE candidate_id=?", deployed.ID).Scan(&driftTask); err != nil {
		t.Fatal(err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = revokeAuthorization(tx, auth, actor, now()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = guardDiscoveryAdmissionCheck(tx, admitted.CheckTask, "policy:discovery-check:"+node.ID, now()); !errors.Is(err, ErrAuthorizationDenied) {
		tx.Rollback()
		t.Fatalf("revoked policy still able to read: %v", err)
	}
	if err = guardDiscoveryDriftCheck(tx, driftTask, now()); !errors.Is(err, ErrAuthorizationDenied) {
		tx.Rollback()
		t.Fatalf("revoked drift grant still allows read: %v", err)
	}

	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDiscoveryObservations(ctx, actor, "daily", page.NextBefore, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a prior cursor must not retain access after revocation: %v", err)
	}

	if _, ok, err = s.claimDiscovery(ctx, source, due+180000); err != nil || ok {
		t.Fatalf("revoked grant made a new task: %v %v", ok, err)
	}
}
