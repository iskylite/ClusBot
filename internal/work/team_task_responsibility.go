package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Task responsibility is separate from worker assignments and execution
// generations. Changing a person never starts, stops, releases or retries work.
func migrateTeamTaskResponsibilities(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS team_task_responsibilities (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
 initiator TEXT NOT NULL REFERENCES accounts(principal_id),
 scope TEXT NOT NULL CHECK(scope IN ('private','team')),
 group_id TEXT NOT NULL DEFAULT '',
 owner TEXT NOT NULL DEFAULT '',
 version INTEGER NOT NULL CHECK(version>0),
 created_at INTEGER NOT NULL,
 CHECK((scope='private' AND group_id='' AND owner=initiator) OR (scope='team' AND group_id<>''))
);
CREATE TABLE IF NOT EXISTS team_task_handoffs (
 handoff_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES team_task_responsibilities(task_id) ON DELETE CASCADE,
 from_owner TEXT NOT NULL,
 to_owner TEXT NOT NULL,
 responsibility_version INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','accepted','superseded')),
 created_at INTEGER NOT NULL,
 decided_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS team_task_responsibility_events (
 event_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES team_task_responsibilities(task_id) ON DELETE CASCADE,
 actor TEXT NOT NULL,
 action TEXT NOT NULL,
 handoff_id TEXT NOT NULL DEFAULT '',
 old_owner TEXT NOT NULL,
 new_owner TEXT NOT NULL,
 responsibility_version INTEGER NOT NULL,
 reason TEXT NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS team_task_responsibility_requests (
 actor TEXT NOT NULL,
 request_key TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 receipt TEXT NOT NULL,
 PRIMARY KEY(actor,request_key)
);
CREATE TABLE IF NOT EXISTS team_task_responsibility_notices (
 notice_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES team_task_responsibilities(task_id) ON DELETE CASCADE,
 recipient TEXT NOT NULL,
 kind TEXT NOT NULL,
 handoff_id TEXT NOT NULL DEFAULT '',
 responsibility_version INTEGER NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS team_task_notices_recipient ON team_task_responsibility_notices(recipient,created_at,notice_id);
`)
	return err
}

// TeamTaskResponsibility identifies who initiated and currently owns a task.
// The immutable initiator and scope must be established while creating the task,
// never inferred from the ambiguous historical user:session identity.
type TeamTaskResponsibility struct {
	TaskID    string `json:"task_id"`
	Initiator string `json:"initiator"`
	Scope     string `json:"scope"`
	GroupID   string `json:"group_id,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Version   int    `json:"responsibility_version"`
}

// Ownership is only one prerequisite; approval, dispatch and individual
// control actions still need their own current authorization and state checks.
type TeamTaskCapabilities struct {
	IsOwner      bool   `json:"is_owner"`
	CanClaim     bool   `json:"can_claim"`
	ClaimReason  string `json:"claim_disabled_reason,omitempty"`
	CanAssign    bool   `json:"can_assign"`
	AssignReason string `json:"assign_disabled_reason,omitempty"`
	CanOffer     bool   `json:"can_offer_handoff"`
	OfferReason  string `json:"offer_disabled_reason,omitempty"`
}

type TeamTaskResponsibilityView struct {
	TeamTaskResponsibility
	Capabilities TeamTaskCapabilities `json:"capabilities"`
}

// teamTaskCreateResponsibilityTx is deliberately transaction-only. Its caller
// must create the Task in the same transaction after authenticating initiator;
// it cannot be used to claim arbitrary pre-existing or legacy tasks.
func teamTaskCreateResponsibilityTx(ctx context.Context, tx *sql.Tx, taskID, initiator, groupID string, open bool) (TeamTaskResponsibility, error) {
	if taskID == "" || initiator == "" || (groupID == "" && open) {
		return TeamTaskResponsibility{}, ErrState
	}
	if err := requireActiveAccount(ctx, tx, initiator); err != nil {
		return TeamTaskResponsibility{}, err
	}
	responsibility := TeamTaskResponsibility{TaskID: taskID, Initiator: initiator, Scope: "private", Owner: initiator, Version: 1}
	if groupID != "" {
		if _, err := teamTaskMemberRole(ctx, tx, groupID, initiator); err != nil {
			return TeamTaskResponsibility{}, err
		}
		responsibility.Scope, responsibility.GroupID = "team", groupID
		if open {
			responsibility.Owner = ""
		}
	}
	stamp := now()
	_, err := tx.ExecContext(ctx, `INSERT INTO team_task_responsibilities(task_id,initiator,scope,group_id,owner,version,created_at)
VALUES(?,?,?,?,?,?,?)`, taskID, initiator, responsibility.Scope, groupID, responsibility.Owner, 1, stamp)
	if err != nil {
		return TeamTaskResponsibility{}, err
	}
	if err := appendTeamTaskEvent(ctx, tx, responsibility, initiator, "created", "", "", responsibility.Owner, "", stamp); err != nil {
		return TeamTaskResponsibility{}, err
	}
	return responsibility, nil
}

func requireActiveAccount(ctx context.Context, tx *sql.Tx, actor string) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE principal_id=? AND status='active')`, actor).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrAuthorizationDenied
	}
	return nil
}

func teamTaskMemberRole(ctx context.Context, tx *sql.Tx, groupID, actor string) (string, error) {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT m.role FROM account_group_memberships m JOIN accounts a ON a.principal_id=m.principal_id
WHERE m.group_id=? AND m.principal_id=? AND a.status='active'`, groupID, actor).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAuthorizationDenied
	}
	return role, err
}

// Trusted internal callers supply actor directly; HTTP calls must supply a
// session digest, whose validity is checked under the same database transaction.
func teamTaskPrincipal(ctx context.Context, tx *sql.Tx, actor, sessionDigest string) (string, error) {
	if sessionDigest == "" {
		if actor == "" {
			return "", ErrAuthorizationDenied
		}
		return actor, nil
	}
	a, err := sessionAccount(ctx, tx, sessionDigest)
	if errors.Is(err, ErrAccountDenied) {
		return "", ErrAuthorizationDenied
	}
	if err != nil {
		return "", err
	}
	return a.PrincipalID, nil
}

func readTeamTaskResponsibility(ctx context.Context, tx *sql.Tx, taskID, actor string) (TeamTaskResponsibility, string, error) {
	if err := requireActiveAccount(ctx, tx, actor); err != nil {
		return TeamTaskResponsibility{}, "", err
	}
	var r TeamTaskResponsibility
	err := tx.QueryRowContext(ctx, `SELECT task_id,initiator,scope,group_id,owner,version
FROM team_task_responsibilities WHERE task_id=?`, taskID).Scan(&r.TaskID, &r.Initiator, &r.Scope, &r.GroupID, &r.Owner, &r.Version)
	if err != nil {
		return TeamTaskResponsibility{}, "", err
	}
	if r.Scope == "private" {
		if r.Initiator != actor {
			return TeamTaskResponsibility{}, "", ErrAuthorizationDenied
		}
		return r, "", nil
	}
	role, err := teamTaskMemberRole(ctx, tx, r.GroupID, actor)
	if err != nil {
		return TeamTaskResponsibility{}, "", err
	}
	return r, role, nil
}

// ReadTeamTaskResponsibility returns only an authorized task's responsibility,
// not the private conversation or material behind that task.
func (s *Store) ReadTeamTaskResponsibility(ctx context.Context, actor, taskID string) (TeamTaskResponsibilityView, error) {
	return s.readTeamTaskResponsibilityForIdentity(ctx, actor, "", taskID)
}

// ReadTeamTaskResponsibilityForSession revalidates the session inside the same
// transaction that reads the role and responsibility.
func (s *Store) ReadTeamTaskResponsibilityForSession(ctx context.Context, sessionDigest, taskID string) (TeamTaskResponsibilityView, error) {
	if sessionDigest == "" {
		return TeamTaskResponsibilityView{}, ErrAuthorizationDenied
	}
	return s.readTeamTaskResponsibilityForIdentity(ctx, "", sessionDigest, taskID)
}

func (s *Store) readTeamTaskResponsibilityForIdentity(ctx context.Context, actor, sessionDigest, taskID string) (TeamTaskResponsibilityView, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TeamTaskResponsibilityView{}, err
	}
	defer tx.Rollback()
	actor, err = teamTaskPrincipal(ctx, tx, actor, sessionDigest)
	if err != nil {
		return TeamTaskResponsibilityView{}, err
	}
	r, role, err := readTeamTaskResponsibility(ctx, tx, taskID, actor)
	if err != nil {
		return TeamTaskResponsibilityView{}, err
	}
	active, err := teamTaskCanChangeResponsibility(ctx, tx, taskID)
	if err != nil {
		return TeamTaskResponsibilityView{}, err
	}
	c := TeamTaskCapabilities{IsOwner: r.Owner == actor}
	switch {
	case !active:
		c.ClaimReason, c.AssignReason, c.OfferReason = "任务已结束", "任务已结束", "任务已结束"
	case r.Scope != "team":
		c.ClaimReason, c.AssignReason, c.OfferReason = "仅团队任务可领取", "仅团队任务可指派", "仅团队任务可交接"
	default:
		c.CanClaim, c.CanAssign, c.CanOffer = r.Owner == "", role == "leader", r.Owner == actor
		if !c.CanClaim {
			c.ClaimReason = "任务已有负责人"
		}
		if !c.CanAssign {
			c.AssignReason = "仅本组负责人可指派"
		}
		if !c.CanOffer {
			c.OfferReason = "仅当前负责人可发起交接"
		}
	}
	return TeamTaskResponsibilityView{TeamTaskResponsibility: r, Capabilities: c}, nil
}

func teamTaskCanChangeResponsibility(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, taskID).Scan(&status); err != nil {
		return false, err
	}
	return status == "active" || status == "waiting_user" || status == "interrupted", nil
}

// ErrTeamTaskVersion contains the current responsibility only after the caller
// has passed the current visibility check, so a losing claimant can refresh.
type ErrTeamTaskVersion struct{ Current TeamTaskResponsibility }

func (e *ErrTeamTaskVersion) Error() string { return "任务负责人已变化，请刷新后重试" }
func (e *ErrTeamTaskVersion) Unwrap() error { return ErrState }

// CheckTeamTaskControl must be repeated within the same transaction as any
// control mutation; this exported read check is only a preview. In particular,
// a team leader, initiator or superadmin is not automatically the controller.
func (s *Store) CheckTeamTaskControl(ctx context.Context, actor, taskID string, expectedVersion int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return checkTeamTaskControlTx(ctx, tx, actor, taskID, expectedVersion)
}

func checkTeamTaskControlTx(ctx context.Context, tx *sql.Tx, actor, taskID string, expectedVersion int) error {
	r, _, err := readTeamTaskResponsibility(ctx, tx, taskID, actor)
	if err != nil {
		return err
	}
	if r.Version != expectedVersion {
		return &ErrTeamTaskVersion{Current: r}
	}
	if r.Owner != actor {
		return ErrAuthorizationDenied
	}
	return nil
}

type TeamTaskResponsibilityAction struct {
	Action          string `json:"action"` // claim, offer, accept, assign
	ExpectedVersion int    `json:"expected_version"`
	RequestKey      string `json:"request_key"`
	Target          string `json:"target,omitempty"`
	HandoffID       string `json:"handoff_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type TeamTaskResponsibilityReceipt struct {
	Responsibility TeamTaskResponsibility `json:"responsibility"`
	Action         string                 `json:"action"`
	HandoffID      string                 `json:"handoff_id,omitempty"`
}

// ApplyTeamTaskResponsibility runs identity, role and target checks, ownership,
// request deduplication, audit and delivery in one serialized SQLite transaction.
// actor is the authenticated principal, never a value taken from request JSON.
func (s *Store) ApplyTeamTaskResponsibility(ctx context.Context, actor, taskID string, in TeamTaskResponsibilityAction) (TeamTaskResponsibilityReceipt, error) {
	return s.applyTeamTaskResponsibilityForIdentity(ctx, actor, "", taskID, in)
}

// ApplyTeamTaskResponsibilityForSession requires a live session in the same
// transaction as the role, ownership, audit and notice changes.
func (s *Store) ApplyTeamTaskResponsibilityForSession(ctx context.Context, sessionDigest, taskID string, in TeamTaskResponsibilityAction) (TeamTaskResponsibilityReceipt, error) {
	if sessionDigest == "" {
		return TeamTaskResponsibilityReceipt{}, ErrAuthorizationDenied
	}
	return s.applyTeamTaskResponsibilityForIdentity(ctx, "", sessionDigest, taskID, in)
}

func (s *Store) applyTeamTaskResponsibilityForIdentity(ctx context.Context, actor, sessionDigest, taskID string, in TeamTaskResponsibilityAction) (TeamTaskResponsibilityReceipt, error) {
	if taskID == "" || (actor == "" && sessionDigest == "") || in.ExpectedVersion < 1 || len(in.RequestKey) == 0 || len(in.RequestKey) > 128 ||
		len(in.Reason) > 1024 || len(in.Target) > 128 || len(in.HandoffID) > 128 {
		return TeamTaskResponsibilityReceipt{}, ErrState
	}
	switch in.Action {
	case "claim":
		if in.Target != "" || in.HandoffID != "" || in.Reason != "" {
			return TeamTaskResponsibilityReceipt{}, ErrState
		}
	case "offer":
		if in.Target == "" || in.HandoffID != "" || in.Reason != "" {
			return TeamTaskResponsibilityReceipt{}, ErrState
		}
	case "accept":
		if in.HandoffID == "" || in.Target != "" || in.Reason != "" {
			return TeamTaskResponsibilityReceipt{}, ErrState
		}
	case "assign":
		if in.Target == "" || in.HandoffID != "" {
			return TeamTaskResponsibilityReceipt{}, ErrState
		}
	default:
		return TeamTaskResponsibilityReceipt{}, ErrState
	}
	payload, err := json.Marshal(struct {
		TaskID string
		Input  TeamTaskResponsibilityAction
	}{taskID, in})
	if err != nil {
		return TeamTaskResponsibilityReceipt{}, err
	}
	fingerprint := digest(string(payload))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TeamTaskResponsibilityReceipt{}, err
	}
	defer tx.Rollback()
	actor, err = teamTaskPrincipal(ctx, tx, actor, sessionDigest)
	if err != nil {
		return TeamTaskResponsibilityReceipt{}, err
	}
	r, role, err := readTeamTaskResponsibility(ctx, tx, taskID, actor)
	if err != nil {
		return TeamTaskResponsibilityReceipt{}, err
	}
	if r.Scope != "team" {
		return TeamTaskResponsibilityReceipt{}, ErrAuthorizationDenied
	}
	var oldFingerprint, saved string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,receipt FROM team_task_responsibility_requests WHERE actor=? AND request_key=?`, actor, in.RequestKey).Scan(&oldFingerprint, &saved)
	if err == nil {
		if oldFingerprint != fingerprint {
			return TeamTaskResponsibilityReceipt{}, ErrConflict
		}
		var receipt TeamTaskResponsibilityReceipt
		if err := json.Unmarshal([]byte(saved), &receipt); err != nil {
			return TeamTaskResponsibilityReceipt{}, err
		}
		return receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TeamTaskResponsibilityReceipt{}, err
	}
	if in.ExpectedVersion != r.Version {
		return TeamTaskResponsibilityReceipt{}, &ErrTeamTaskVersion{Current: r}
	}
	active, err := teamTaskCanChangeResponsibility(ctx, tx, taskID)
	if err != nil {
		return TeamTaskResponsibilityReceipt{}, err
	}
	if !active {
		return TeamTaskResponsibilityReceipt{}, ErrState
	}

	stamp := now()
	before := r.Owner
	receipt := TeamTaskResponsibilityReceipt{Action: in.Action}
	switch in.Action {
	case "claim":
		if r.Owner != "" {
			return receipt, ErrState
		}
		r.Owner = actor
	case "offer":
		if r.Owner != actor || in.Target == actor {
			return receipt, ErrAuthorizationDenied
		}
		if _, err := teamTaskMemberRole(ctx, tx, r.GroupID, in.Target); err != nil {
			return receipt, err
		}
		receipt.HandoffID = id()
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_task_handoffs(handoff_id,task_id,from_owner,to_owner,responsibility_version,state,created_at)
VALUES(?,?,?,?,?,'pending',?)`, receipt.HandoffID, taskID, actor, in.Target, r.Version, stamp); err != nil {
			return receipt, err
		}
	case "accept":
		var from, to, state string
		var version int
		err := tx.QueryRowContext(ctx, `SELECT from_owner,to_owner,responsibility_version,state FROM team_task_handoffs WHERE handoff_id=? AND task_id=?`, in.HandoffID, taskID).Scan(&from, &to, &version, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return receipt, ErrState
		}
		if err != nil {
			return receipt, err
		}
		if to != actor {
			return receipt, ErrAuthorizationDenied
		}
		if state != "pending" || from != r.Owner || version != r.Version {
			return receipt, ErrState
		}
		if _, err := teamTaskMemberRole(ctx, tx, r.GroupID, from); err != nil {
			return receipt, err
		}
		r.Owner, receipt.HandoffID = actor, in.HandoffID
		if _, err := tx.ExecContext(ctx, `UPDATE team_task_handoffs SET state='accepted',decided_at=? WHERE handoff_id=?`, stamp, in.HandoffID); err != nil {
			return receipt, err
		}
	case "assign":
		if role != "leader" {
			return receipt, ErrAuthorizationDenied
		}
		if in.Target == r.Owner {
			return receipt, ErrState
		}
		if r.Owner != "" && strings.TrimSpace(in.Reason) == "" {
			return receipt, ErrState
		}
		if _, err := teamTaskMemberRole(ctx, tx, r.GroupID, in.Target); err != nil {
			return receipt, err
		}
		r.Owner = in.Target
	}
	if in.Action != "offer" {
		result, err := tx.ExecContext(ctx, `UPDATE team_task_responsibilities SET owner=?,version=version+1 WHERE task_id=? AND version=? AND owner=?`, r.Owner, taskID, r.Version, before)
		if err != nil {
			return receipt, err
		}
		if n, err := result.RowsAffected(); err != nil {
			return receipt, err
		} else if n != 1 {
			return receipt, ErrState
		}
		r.Version++
		if _, err := tx.ExecContext(ctx, `UPDATE team_task_handoffs SET state='superseded',decided_at=? WHERE task_id=? AND state='pending'`, stamp, taskID); err != nil {
			return receipt, err
		}
	}
	if err := appendTeamTaskEvent(ctx, tx, r, actor, in.Action, receipt.HandoffID, before, r.Owner, in.Reason, stamp); err != nil {
		return receipt, err
	}
	noticeRecipient := r.Owner
	if in.Action == "offer" {
		noticeRecipient = in.Target
	} else if in.Action == "accept" {
		noticeRecipient = before
	}
	if noticeRecipient != "" && noticeRecipient != actor {
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_task_responsibility_notices(notice_id,task_id,recipient,kind,handoff_id,responsibility_version,created_at)
VALUES(?,?,?,?,?,?,?)`, id(), taskID, noticeRecipient, in.Action, receipt.HandoffID, r.Version, stamp); err != nil {
			return receipt, err
		}
	}
	receipt.Responsibility = r
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_task_responsibility_requests(actor,request_key,fingerprint,receipt) VALUES(?,?,?,?)`, actor, in.RequestKey, fingerprint, string(encoded)); err != nil {
		return receipt, err
	}
	if err := tx.Commit(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func appendTeamTaskEvent(ctx context.Context, tx *sql.Tx, r TeamTaskResponsibility, actor, action, handoffID, oldOwner, newOwner, reason string, stamp int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO team_task_responsibility_events(event_id,task_id,actor,action,handoff_id,old_owner,new_owner,responsibility_version,reason,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`, id(), r.TaskID, actor, action, handoffID, oldOwner, newOwner, r.Version, reason, stamp)
	return err
}

type TeamTaskNotice struct {
	NoticeID  string `json:"notice_id"`
	TaskID    string `json:"task_id"`
	Kind      string `json:"kind"`
	HandoffID string `json:"handoff_id,omitempty"`
	Version   int    `json:"responsibility_version"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// TeamTaskNotices rechecks current group membership before returning any task
// identifier, including old notices; removing a member immediately hides them.
func (s *Store) TeamTaskNotices(ctx context.Context, actor string) ([]TeamTaskNotice, error) {
	return s.teamTaskNoticesForIdentity(ctx, actor, "")
}

func (s *Store) TeamTaskNoticesForSession(ctx context.Context, sessionDigest string) ([]TeamTaskNotice, error) {
	if sessionDigest == "" {
		return nil, ErrAuthorizationDenied
	}
	return s.teamTaskNoticesForIdentity(ctx, "", sessionDigest)
}

func (s *Store) teamTaskNoticesForIdentity(ctx context.Context, actor, sessionDigest string) ([]TeamTaskNotice, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	actor, err = teamTaskPrincipal(ctx, tx, actor, sessionDigest)
	if err != nil {
		return nil, err
	}
	if err := requireActiveAccount(ctx, tx, actor); err != nil {
		return nil, err
	}
	// The unavailable-owner item is derived from current membership and account
	// state, so newly appointed leaders see it too and it disappears on recovery
	// or reassignment. No withdrawal timestamp is invented.
	rows, err := tx.QueryContext(ctx, `SELECT notice_id,task_id,kind,handoff_id,responsibility_version,created_at FROM (
SELECT n.notice_id,n.task_id,n.kind,n.handoff_id,n.responsibility_version,n.created_at
FROM team_task_responsibility_notices n JOIN team_task_responsibilities r ON r.task_id=n.task_id
JOIN account_group_memberships m ON m.group_id=r.group_id AND m.principal_id=?
WHERE n.recipient=? AND r.scope='team'
AND (n.kind<>'offer' OR EXISTS (
 SELECT 1 FROM team_task_handoffs h
 JOIN accounts a ON a.principal_id=h.from_owner AND a.status='active'
 JOIN account_group_memberships source ON source.principal_id=h.from_owner AND source.group_id=r.group_id
 JOIN tasks t ON t.id=r.task_id
 WHERE h.handoff_id=n.handoff_id AND h.task_id=r.task_id AND h.to_owner=n.recipient
 AND h.state='pending' AND h.from_owner=r.owner AND h.responsibility_version=r.version
 AND t.status IN ('active','waiting_user','interrupted')
))
UNION ALL
SELECT 'owner-unavailable:'||r.task_id||':'||r.version,r.task_id,'owner_unavailable','',r.version,0
FROM team_task_responsibilities r
JOIN account_group_memberships leader ON leader.group_id=r.group_id AND leader.principal_id=? AND leader.role='leader'
JOIN tasks t ON t.id=r.task_id AND t.status IN ('active','waiting_user','interrupted')
LEFT JOIN accounts owner ON owner.principal_id=r.owner AND owner.status='active'
LEFT JOIN account_group_memberships membership ON membership.principal_id=r.owner AND membership.group_id=r.group_id
WHERE r.scope='team' AND r.owner<>'' AND (owner.principal_id IS NULL OR membership.principal_id IS NULL)
) ORDER BY CASE WHEN kind='owner_unavailable' THEN 0 ELSE 1 END,created_at DESC,notice_id DESC LIMIT 100`, actor, actor, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []TeamTaskNotice
	for rows.Next() {
		var n TeamTaskNotice
		if err := rows.Scan(&n.NoticeID, &n.TaskID, &n.Kind, &n.HandoffID, &n.Version, &n.CreatedAt); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}
