package work

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func incidentFixture(t *testing.T) (*Store, *fakeTicketSystem, *TicketProvider, Incident) {
	t.Helper()
	s, f, p := setupB(t, "incident-actions.db")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "read", ObjectRef: "node-1", ValidFrom: now(), Owner: "ops"}, now()); err != nil {
		t.Fatal(err)
	}
	inc, err := createIncident(tx, Incident{Symptoms: "节点异常"}, now())
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s, f, p, inc
}

func TestIncidentTicketConfirmationAndDispatch(t *testing.T) {
	s, f, p, inc := incidentFixture(t)
	ctx := context.Background()
	preview, err := s.PreviewIncidentTicket(ctx, "user:ops", inc.IncidentID, "node-1", "人工报告：网卡异常")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ConfirmIncidentTicket(ctx, "user:ops", inc.IncidentID, preview.PreviewID, p)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ConfirmIncidentTicket(ctx, "user:ops", inc.IncidentID, preview.PreviewID, p)
	if err != nil || !replay.Duplicate || replay.OperationID != first.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = s.ConfirmIncidentTicket(ctx, "user:other", inc.IncidentID, preview.PreviewID, p); err == nil {
		t.Fatal("another principal used the confirmation")
	}
	if _, err = s.PreviewIncidentTicket(ctx, "user:ops", inc.IncidentID, "node-1", "再次请求"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second request not blocked: %v", err)
	}
	dispatched, err := s.DispatchTicketCreate(ctx, first.OperationID, "user:ops", "", p)
	if err != nil {
		t.Fatal(err)
	}
	if f.createCount() != 1 {
		t.Fatalf("provider calls=%d", f.createCount())
	}
	ticket, err := readTicket(s.db, dispatched.TicketID)
	if err != nil || ticket.IncidentID != inc.IncidentID || ticket.ObjectRef != "node-1" || ticket.FaultEvidence != "人工报告：网卡异常" {
		t.Fatalf("ticket=%+v err=%v", ticket, err)
	}
	if _, err = s.ConfirmIncidentTicket(ctx, "user:ops", inc.IncidentID, preview.PreviewID, p); err != nil {
		t.Fatalf("lost-response retry: %v", err)
	}
	if f.createCount() != 1 {
		t.Fatal("retry created another external ticket")
	}
}

func TestIncidentTicketOptionsAndStalePreview(t *testing.T) {
	s, _, p, inc := incidentFixture(t)
	ctx := context.Background()
	stamp := now()
	tx, _ := s.db.Begin()
	for _, a := range []OpsAsset{
		{ObjectRef: "read-only", Kind: "node", Identity: "r", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1},
		{ObjectRef: "part-1", Kind: "component", Identity: "p", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1},
		{ObjectRef: "frozen", Kind: "node", Identity: "f", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1, FrozenAt: stamp},
	} {
		if _, err := upsertAsset(tx, a, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []Authorization{
		{Subject: "user:ops", Action: "read", ObjectRef: "read-only", ValidFrom: stamp, Owner: "ops"},
		{Subject: "user:ops", Action: "read", ObjectRef: "part-1", ValidFrom: stamp, Owner: "ops"},
		{Subject: "user:ops", Action: "repair", ObjectRef: "part-1", ValidFrom: stamp, Owner: "ops"},
		{Subject: "user:ops", Action: "read", ObjectRef: "frozen", ValidFrom: stamp, Owner: "ops"},
		{Subject: "user:ops", Action: "repair", ObjectRef: "frozen", ValidFrom: stamp, Owner: "ops"},
	} {
		if _, err := grantAuthorization(tx, g, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE ops_assets SET frozen_at=? WHERE object_ref='frozen'`, stamp); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	options, err := s.IncidentTicketOptions(ctx, "user:ops", inc.IncidentID, "")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, a := range options.Assets {
		refs[a.ObjectRef] = true
	}
	if !refs["node-1"] || !refs["part-1"] || refs["read-only"] || refs["frozen"] {
		t.Fatalf("unexpected candidates: %+v", options.Assets)
	}
	preview, err := s.PreviewIncidentTicket(ctx, "user:ops", inc.IncidentID, "node-1", "故障描述")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE ops_assets SET revision=revision+1 WHERE object_ref='node-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmIncidentTicket(ctx, "user:ops", inc.IncidentID, preview.PreviewID, p); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale preview accepted: %v", err)
	}
}

func TestIncidentRecheckRequiresIndependentEvidenceAndLeavesEventPending(t *testing.T) {
	s, _, _, inc := incidentFixture(t)
	ctx := context.Background()
	tx, _ := s.db.Begin()
	ticket, err := createTicket(tx, Ticket{ExternalSystem: "fixture", ExternalTicketID: "isolated-1", IncidentID: inc.IncidentID, ObjectRef: "node-1"}, now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tickets SET state='closed' WHERE ticket_id=?`, ticket.TicketID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, nil); err == nil {
		t.Fatal("missing checker accepted")
	}
	var calls atomic.Int32
	checker := func(context.Context, string) (bool, string, error) {
		calls.Add(1)
		return true, "isolated device test: health and test passed", nil
	}
	preview, err := s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, checker)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectRef != "node-1" || result.IncidentAcceptance != "pending" {
		t.Fatalf("wrong scope: %+v", result)
	}
	var recovered int
	var evidence string
	if err = s.db.QueryRow(`SELECT recovery_verified,recovery_evidence FROM incidents WHERE incident_id=?`, inc.IncidentID).Scan(&recovered, &evidence); err != nil {
		t.Fatal(err)
	}
	if recovered != 0 || evidence != "" {
		t.Fatal("single-asset check marked entire incident recovered")
	}
	var receipt string
	if err = s.db.QueryRow(`SELECT evidence FROM ticket_health_verifications WHERE ticket_id=?`, ticket.TicketID).Scan(&receipt); err != nil || receipt == "" {
		t.Fatalf("missing receipt: %q %v", receipt, err)
	}
	again, err := s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker)
	if err != nil || !again.Duplicate || calls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", again, calls.Load(), err)
	}
}

func TestIncidentRecheckWithAssetTransition(t *testing.T) {
	s, _, _, inc := incidentFixture(t)
	ctx := context.Background()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	if _, err = grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "write", ObjectRef: "node-1", ValidFrom: stamp, Owner: "ops"}, stamp); err != nil {
		t.Fatal(err)
	}
	ticket, err := createTicket(tx, Ticket{ExternalSystem: "fixture", ExternalTicketID: "transition-check", IncidentID: inc.IncidentID, ObjectRef: "node-1"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tickets SET state='closed' WHERE ticket_id=?`, ticket.TicketID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	checker := func(context.Context, string) (bool, string, error) {
		calls.Add(1)
		return true, "independent fixture evidence", nil
	}
	preview, err := s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, checker)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker)
	if err != nil || result.IncidentAcceptance != "pending" {
		t.Fatalf("verified asset transition not confirmed: result=%+v err=%v", result, err)
	}
	asset, err := readAsset(s.db, "node-1")
	if err != nil || asset.State != AssetStateRecheck {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}
	replay, err := s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker)
	if err != nil || !replay.Duplicate || calls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, calls.Load(), err)
	}
}

func TestIncidentRecheckRejectsAssetChangeDuringProbe(t *testing.T) {
	s, _, _, inc := incidentFixture(t)
	ctx := context.Background()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := createTicket(tx, Ticket{ExternalSystem: "fixture", ExternalTicketID: "concurrent-check", IncidentID: inc.IncidentID, ObjectRef: "node-1"}, now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tickets SET state='closed' WHERE ticket_id=?`, ticket.TicketID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checker := func(context.Context, string) (bool, string, error) {
		_, err := s.db.Exec(`UPDATE ops_assets SET revision=revision+1,ownership_epoch=ownership_epoch+1 WHERE object_ref='node-1'`)
		return true, "independent fixture evidence", err
	}
	preview, err := s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, checker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent asset change accepted: %v", err)
	}
	var receipts int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM incident_ticket_rechecks`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("unexpected recheck records=%d err=%v", receipts, err)
	}
}

func TestIncidentRecheckRejectsFailedOrEmptyEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ok       bool
		evidence string
	}{{"failed", false, "device still unhealthy"}, {"missing evidence", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, inc := incidentFixture(t)
			ctx := context.Background()
			tx, _ := s.db.Begin()
			ticket, err := createTicket(tx, Ticket{ExternalSystem: "fixture", ExternalTicketID: "isolated-2", IncidentID: inc.IncidentID, ObjectRef: "node-1"}, now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`UPDATE tickets SET state='closed' WHERE ticket_id=?`, ticket.TicketID); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			checker := func(context.Context, string) (bool, string, error) { return tc.ok, tc.evidence, nil }
			preview, err := s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, checker)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, preview.PreviewID, checker); err == nil {
				t.Fatal("unproven recheck passed")
			}
			var count, recovered int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM incident_ticket_rechecks`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRow(`SELECT recovery_verified FROM incidents WHERE incident_id=?`, inc.IncidentID).Scan(&recovered); err != nil {
				t.Fatal(err)
			}
			if count != 0 || recovered != 0 {
				t.Fatalf("unproven recheck recorded: %d %d", count, recovered)
			}
		})
	}
}

func TestIncidentRecheckRechecksRevokedAuthorization(t *testing.T) {
	s, _, _, inc := incidentFixture(t)
	ctx := context.Background()
	tx, _ := s.db.Begin()
	ticket, err := createTicket(tx, Ticket{ExternalSystem: "fixture", ExternalTicketID: "isolated-revoke", IncidentID: inc.IncidentID, ObjectRef: "node-1"}, now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tickets SET state='closed' WHERE ticket_id=?`, ticket.TicketID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checker := func(context.Context, string) (bool, string, error) {
		_, err := s.db.Exec(`UPDATE authorizations SET revoked_at=? WHERE subject='user:ops' AND action='repair' AND object_ref='node-1'`, now())
		return true, "independent fixture evidence", err
	}
	p, err := s.PreviewIncidentRecheck(ctx, "user:ops", inc.IncidentID, ticket.TicketID, checker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmIncidentRecheck(ctx, "user:ops", inc.IncidentID, p.PreviewID, checker); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked permission accepted: %v", err)
	}
	current, err := readTicket(s.db, ticket.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "closed" {
		t.Fatalf("ticket changed after revocation: %s", current.State)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM ticket_health_verifications`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("receipt after revocation: %d %v", count, err)
	}
}

func TestIncidentActionSchemaMigratesFrom132(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v132.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE incident_ticket_rechecks; DROP TABLE ticket_health_verifications; DROP TABLE incident_ticket_operations; DROP TABLE incident_action_previews; PRAGMA user_version=132;`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"incident_action_previews", "incident_ticket_operations", "ticket_health_verifications", "incident_ticket_rechecks"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing %s after migration: %d %v", name, count, err)
		}
	}
}
