package work

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

const (
	teamInitiator = "user:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	teamLeader    = "user:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	teamMember1   = "user:cccccccccccccccccccccccccccccccc"
	teamMember2   = "user:dddddddddddddddddddddddddddddddd"
	teamOutsider  = "user:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func teamTaskFixture(t *testing.T, open bool) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, actor := range []string{teamInitiator, teamLeader, teamMember1, teamMember2, teamOutsider} {
		if _, err := s.db.Exec(`INSERT INTO principals(principal_id,kind,identity_digest,created_at) VALUES(?,'user','fixture',1)`, actor); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO accounts(principal_id,username,display_name,password_hash,status,created_at) VALUES(?,?,?,'fixture-hash','active',1)`, actor, actor, actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO operation_groups(group_id,name,created_at) VALUES('g1','team',1),('g2','other',1)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ group, actor, role string }{
		{"g1", teamInitiator, "member"}, {"g1", teamLeader, "leader"}, {"g1", teamMember1, "member"}, {"g1", teamMember2, "member"}, {"g2", teamOutsider, "leader"},
	} {
		if _, err := s.db.Exec(`INSERT INTO account_group_memberships(group_id,principal_id,role) VALUES(?,?,?)`, m.group, m.actor, m.role); err != nil {
			t.Fatal(err)
		}
	}
	run, err := s.Submit(context.Background(), "fixture", "task", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamTaskCreateResponsibilityTx(context.Background(), tx, run.TaskID, teamInitiator, "g1", open); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s, run.TaskID
}

func TestTeamTaskClaimRaceAndScope(t *testing.T) {
	s, task := teamTaskFixture(t, true)
	ctx := context.Background()
	if _, err := s.ReadTeamTaskResponsibility(ctx, teamOutsider, task); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("other group's leader accessed task: %v", err)
	}
	var wg sync.WaitGroup
	type result struct {
		actor   string
		receipt TeamTaskResponsibilityReceipt
		err     error
	}
	out := make(chan result, 2)
	for _, actor := range []string{teamMember1, teamMember2} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			receipt, err := s.ApplyTeamTaskResponsibility(ctx, actor, task, TeamTaskResponsibilityAction{Action: "claim", ExpectedVersion: 1, RequestKey: "same-key"})
			out <- result{actor, receipt, err}
		}(actor)
	}
	wg.Wait()
	close(out)
	var winner string
	var loserErr error
	for r := range out {
		if r.err == nil {
			if winner != "" {
				t.Fatal("two winners")
			}
			winner = r.actor
			if r.receipt.Responsibility.Version != 2 {
				t.Fatalf("wrong version: %+v", r.receipt)
			}
		} else {
			loserErr = r.err
		}
	}
	var stale *ErrTeamTaskVersion
	if winner == "" || !errors.As(loserErr, &stale) || stale.Current.Owner != winner || stale.Current.Version != 2 {
		t.Fatalf("claim did not report current winner %q: %v", winner, loserErr)
	}
	// Retries are idempotent, and request keys are scoped to each actor.
	in := TeamTaskResponsibilityAction{Action: "claim", ExpectedVersion: 1, RequestKey: "same-key"}
	again, err := s.ApplyTeamTaskResponsibility(ctx, winner, task, in)
	if err != nil || again.Responsibility.Owner != winner {
		t.Fatalf("retry = %+v %v", again, err)
	}
	in.Action = "offer"
	in.Target = teamMember1
	if _, err := s.ApplyTeamTaskResponsibility(ctx, winner, task, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload reused request key: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM team_task_responsibility_events WHERE task_id=? AND action='claim'`, task).Scan(&count); err != nil || count != 1 {
		t.Fatalf("claim audit count=%d: %v", count, err)
	}
}

func TestTeamTaskHandoffAndAuthorizationChanges(t *testing.T) {
	s, task := teamTaskFixture(t, false)
	ctx := context.Background()
	apply := func(actor string, in TeamTaskResponsibilityAction) (TeamTaskResponsibilityReceipt, error) {
		return s.ApplyTeamTaskResponsibility(ctx, actor, task, in)
	}
	if _, err := apply(teamInitiator, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 1, Target: teamMember1, RequestKey: "no-lead"}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("ordinary member assigned: %v", err)
	}
	if _, err := apply(teamLeader, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 1, Target: teamOutsider, Reason: "shift", RequestKey: "wrong-group"}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("outsider assigned: %v", err)
	}
	if _, err := apply(teamLeader, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 1, Target: teamMember1, RequestKey: "no-reason"}); !errors.Is(err, ErrState) {
		t.Fatalf("forced transfer without reason: %v", err)
	}
	offer, err := apply(teamInitiator, TeamTaskResponsibilityAction{Action: "offer", ExpectedVersion: 1, Target: teamMember1, RequestKey: "offer"})
	if err != nil || offer.HandoffID == "" || offer.Responsibility.Owner != teamInitiator || offer.Responsibility.Version != 1 {
		t.Fatalf("offer prematurely changed owner: %+v %v", offer, err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamInitiator, task, 1); err != nil {
		t.Fatalf("current owner lost control before acceptance: %v", err)
	}
	if _, err := apply(teamMember2, TeamTaskResponsibilityAction{Action: "accept", ExpectedVersion: 1, HandoffID: offer.HandoffID, RequestKey: "wrong-recipient"}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("wrong recipient accepted: %v", err)
	}
	accepted, err := apply(teamMember1, TeamTaskResponsibilityAction{Action: "accept", ExpectedVersion: 1, HandoffID: offer.HandoffID, RequestKey: "accept"})
	if err != nil || accepted.Responsibility.Owner != teamMember1 || accepted.Responsibility.Version != 2 {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamInitiator, task, 1); !errors.Is(err, ErrState) {
		t.Fatalf("stale owner control not expired: %v", err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamInitiator, task, 2); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("old owner retained control: %v", err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamLeader, task, 2); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("leader automatically acquired control: %v", err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamMember1, task, 2); err != nil {
		t.Fatalf("new owner denied: %v", err)
	}
	// Disabling or removing a member also hides the task and old notices immediately.
	if _, err := s.db.Exec(`DELETE FROM account_group_memberships WHERE group_id='g1' AND principal_id=?`, teamMember1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadTeamTaskResponsibility(ctx, teamMember1, task); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("removed member retained read access: %v", err)
	}
	notices, err := s.TeamTaskNotices(ctx, teamMember1)
	if err != nil || len(notices) != 0 {
		t.Fatalf("removed member retained notices: %+v %v", notices, err)
	}
	leaderNotices, err := s.TeamTaskNotices(ctx, teamLeader)
	if err != nil || len(leaderNotices) != 1 || leaderNotices[0].Kind != "owner_unavailable" || leaderNotices[0].TaskID != task {
		t.Fatalf("leader not informed of unavailable owner: %+v %v", leaderNotices, err)
	}
	if _, err := apply(teamLeader, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 2, Target: teamMember2, Reason: "member left", RequestKey: "force"}); err != nil {
		t.Fatalf("leader could not reassign: %v", err)
	}
	leaderNotices, err = s.TeamTaskNotices(ctx, teamLeader)
	if err != nil || len(leaderNotices) != 0 {
		t.Fatalf("unavailable-owner notice remained after reassignment: %+v %v", leaderNotices, err)
	}
	if _, err := s.db.Exec(`UPDATE accounts SET status='disabled' WHERE principal_id=?`, teamMember2); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckTeamTaskControl(ctx, teamMember2, task, 3); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("disabled owner retained control: %v", err)
	}
}

func TestTeamTaskTransferPreservesInflightWork(t *testing.T) {
	s, task := teamTaskFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO operations(operation_id,task_id,capability_key,target_refs,params_digest,authorization_ref,idempotency_key,state,attempt_count,current_attempt_id,registered_at,updated_at)
VALUES('operation',?,'cap','["host"]','params','original-grant','once','running',1,'attempt',1,2)`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_jobs(job_id,operation_id,provider_job_id,query_key,state,query_generation,created_at,updated_at)
VALUES('job','operation','remote-id','query','running',7,1,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO work_leases(lease_id,unit_id,task_id,lease_generation,worker_id,token_id,issued_at,lease_until,state)
VALUES('lease','unit',?,3,'worker','token',1,9999999999999,'active')`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTeamTaskResponsibility(ctx, teamLeader, task, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 1, RequestKey: "shift", Target: teamMember1, Reason: "shift"}); err != nil {
		t.Fatal(err)
	}
	var opState, grant, key, attempt, jobState, remote, query, leaseState string
	var opCount, queryGen, leaseGen int
	err := s.db.QueryRow(`SELECT state,authorization_ref,idempotency_key,attempt_count,current_attempt_id FROM operations WHERE operation_id='operation'`).Scan(&opState, &grant, &key, &opCount, &attempt)
	if err != nil || opState != "running" || grant != "original-grant" || key != "once" || opCount != 1 || attempt != "attempt" {
		t.Fatalf("operation altered: %q %q %q %d %q %v", opState, grant, key, opCount, attempt, err)
	}
	err = s.db.QueryRow(`SELECT state,provider_job_id,query_key,query_generation FROM operation_jobs WHERE job_id='job'`).Scan(&jobState, &remote, &query, &queryGen)
	if err != nil || jobState != "running" || remote != "remote-id" || query != "query" || queryGen != 7 {
		t.Fatalf("job altered: %q %q %q %d %v", jobState, remote, query, queryGen, err)
	}
	err = s.db.QueryRow(`SELECT state,lease_generation FROM work_leases WHERE lease_id='lease'`).Scan(&leaseState, &leaseGen)
	if err != nil || leaseState != "active" || leaseGen != 3 {
		t.Fatalf("lease released: %q %d %v", leaseState, leaseGen, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM operations WHERE task_id=?`, task).Scan(&count); err != nil || count != 1 {
		t.Fatalf("operation resent: count=%d %v", count, err)
	}
}

func TestTeamTaskAuditAndNoticeRollBack(t *testing.T) {
	s, task := teamTaskFixture(t, true)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_team_event BEFORE INSERT ON team_task_responsibility_events WHEN NEW.action='assign' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := s.ApplyTeamTaskResponsibility(context.Background(), teamLeader, task, TeamTaskResponsibilityAction{Action: "assign", ExpectedVersion: 1, RequestKey: "failed", Target: teamMember1})
	if err == nil {
		t.Fatal("expected audit failure")
	}
	view, err := s.ReadTeamTaskResponsibility(context.Background(), teamLeader, task)
	if err != nil || view.Owner != "" || view.Version != 1 {
		t.Fatalf("failed transaction changed responsibility: %+v %v", view, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM team_task_responsibility_notices WHERE task_id=?`, task).Scan(&count); err != nil || count != 0 {
		t.Fatalf("notice persisted after rollback: %d %v", count, err)
	}
}

func TestTeamTaskCompetingHandoffs(t *testing.T) {
	s, task := teamTaskFixture(t, false)
	ctx := context.Background()
	offers := make(map[string]string)
	for _, actor := range []string{teamMember1, teamMember2} {
		offer, err := s.ApplyTeamTaskResponsibility(ctx, teamInitiator, task, TeamTaskResponsibilityAction{
			Action: "offer", ExpectedVersion: 1, RequestKey: "offer-" + actor, Target: actor,
		})
		if err != nil {
			t.Fatal(err)
		}
		offers[actor] = offer.HandoffID
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for actor, handoff := range offers {
		wg.Add(1)
		go func(actor, handoff string) {
			defer wg.Done()
			_, err := s.ApplyTeamTaskResponsibility(ctx, actor, task, TeamTaskResponsibilityAction{
				Action: "accept", ExpectedVersion: 1, RequestKey: "accept", HandoffID: handoff,
			})
			errs <- err
		}(actor, handoff)
	}
	wg.Wait()
	close(errs)
	var accepted, conflicts int
	for err := range errs {
		var stale *ErrTeamTaskVersion
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &stale):
			conflicts++
		default:
			t.Fatalf("unexpected acceptance result: %v", err)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("accept=%d conflict=%d", accepted, conflicts)
	}
	var acceptedRows, supersededRows int
	if err := s.db.QueryRow(`SELECT count(*) FROM team_task_handoffs WHERE task_id=? AND state='accepted'`, task).Scan(&acceptedRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM team_task_handoffs WHERE task_id=? AND state='superseded'`, task).Scan(&supersededRows); err != nil {
		t.Fatal(err)
	}
	if acceptedRows != 1 || supersededRows != 1 {
		t.Fatalf("handoff records: accepted=%d superseded=%d", acceptedRows, supersededRows)
	}
	for _, actor := range []string{teamMember1, teamMember2} {
		notices, err := s.TeamTaskNotices(ctx, actor)
		if err != nil || len(notices) != 0 {
			t.Fatalf("resolved offer remained a pending notice: %+v %v", notices, err)
		}
	}
}

func TestTeamTaskEndAndFormerOwnerRechecked(t *testing.T) {
	s, task := teamTaskFixture(t, false)
	ctx := context.Background()
	offer, err := s.ApplyTeamTaskResponsibility(ctx, teamInitiator, task, TeamTaskResponsibilityAction{
		Action: "offer", ExpectedVersion: 1, RequestKey: "offer", Target: teamMember1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE accounts SET status='disabled' WHERE principal_id=?`, teamInitiator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTeamTaskResponsibility(ctx, teamMember1, task, TeamTaskResponsibilityAction{
		Action: "accept", ExpectedVersion: 1, RequestKey: "accept-disabled-owner", HandoffID: offer.HandoffID,
	}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("accepted an invalid owner's handoff: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET status='completed' WHERE id=?`, task); err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadTeamTaskResponsibility(ctx, teamLeader, task)
	if err != nil || view.Capabilities.CanAssign || view.Capabilities.AssignReason == "" {
		t.Fatalf("ended task still assignable: %+v %v", view, err)
	}
	if _, err := s.ApplyTeamTaskResponsibility(ctx, teamLeader, task, TeamTaskResponsibilityAction{
		Action: "assign", ExpectedVersion: 1, RequestKey: "ended", Target: teamMember2, Reason: "shift",
	}); !errors.Is(err, ErrState) {
		t.Fatalf("ended task was reassigned: %v", err)
	}
}

func TestTeamTaskSessionRevocationCheckedInTransaction(t *testing.T) {
	s, task := teamTaskFixture(t, true)
	ctx := context.Background()
	const session = "session-digest"
	if err := s.CreateAccountSession(ctx, teamMember1, "fixture-hash", session, now()+60000); err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadTeamTaskResponsibilityForSession(ctx, session, task)
	if err != nil || !view.Capabilities.CanClaim {
		t.Fatalf("valid session could not read: %+v %v", view, err)
	}
	if err := s.DeleteAccountSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTeamTaskResponsibilityForSession(ctx, session, task, TeamTaskResponsibilityAction{
		Action: "claim", ExpectedVersion: 1, RequestKey: "revoked",
	}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked session changed responsibility: %v", err)
	}
	if _, err := s.TeamTaskNoticesForSession(ctx, session); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("revoked session read notices: %v", err)
	}
	view, err = s.ReadTeamTaskResponsibility(ctx, teamLeader, task)
	if err != nil || view.Owner != "" || view.Version != 1 {
		t.Fatalf("revoked session changed owner: %+v %v", view, err)
	}
}
