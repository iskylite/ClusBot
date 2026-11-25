package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const discoveryA2UISource = "a2ui-discovery"

var (
	ErrA2UIDiscoveryStale = errors.New("候选资产已变化、失效或已纳管，请重新打开候选列表")
	ErrA2UIActionPending  = errors.New("这次操作仍在处理或未能确认结果，不会自动重发给模型")
	ErrA2UIActionFailed   = errors.New("这次操作未完成，请重新发起操作")
)

func discoveryA2UIConversation(ctx context.Context, tx *sql.Tx, actor, conversation string) error {
	allowed, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return err
	}
	if !allowed || conversation == "" {
		return sql.ErrNoRows
	}
	var archived bool
	if err := tx.QueryRowContext(ctx, "SELECT archived FROM conversations WHERE id=?", conversation).Scan(&archived); err != nil {
		return err
	}
	if archived {
		return ErrConversationArchived
	}
	return nil
}

func a2uiCandidateTx(ctx context.Context, tx *sql.Tx, actor, conversation, candidate string, revision int64) (DiscoveryCandidate, error) {
	var out DiscoveryCandidate
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return out, err
	}
	var evidence, fingerprint, plan string
	var schedule, ordinal int64
	err := tx.QueryRowContext(ctx, snapshotDiscoveryCandidate+" WHERE c.candidate_id=?", candidate).
		Scan(&out.ID, &out.Source, &out.Scope, &out.Serial, &out.Address, &out.SeenAt, &out.State, &out.Asset, &out.CheckTask, &evidence, &fingerprint, &plan, &schedule, &ordinal)
	if err != nil {
		return out, err
	}
	nowAt := now()
	allowed, err := discoveryGranted(tx, actor, out.Source, out.Scope, false, nowAt, fingerprint)
	if err != nil {
		return out, err
	}
	if !allowed {
		return DiscoveryCandidate{}, sql.ErrNoRows
	}
	if !DiscoveryA2UICandidateIsCurrent(out, nowAt) || out.SeenAt != revision {
		return DiscoveryCandidate{}, ErrA2UIDiscoveryStale
	}
	out.A2UISnapshot = digestDiscoveryCandidate(out, plan, schedule, ordinal, fingerprint, evidence)
	return out, nil
}

func digestDiscoveryCandidate(c DiscoveryCandidate, plan string, schedule, ordinal int64, connection, evidence string) string {
	return digest(toJSON([]any{c.ID, c.Source, c.Scope, c.Serial, c.Address, c.SeenAt, c.State,
		plan, schedule, ordinal, connection, evidence}))
}

func DiscoveryA2UICandidateIsCurrent(c DiscoveryCandidate, at int64) bool {
	return c.State == "observed" && c.Asset == "" && c.Serial != "" && c.SeenAt > 0 &&
		c.SeenAt <= at && c.SeenAt >= at-int64(24*time.Hour/time.Millisecond)
}

func (s *Store) DiscoveryA2UICandidates(ctx context.Context, actor, conversation string) ([]DiscoveryCandidate, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	err = discoveryA2UIConversation(ctx, tx, actor, conversation)
	_ = tx.Rollback()
	if err != nil {
		return nil, err
	}
	out, err := s.ListDiscoveryCandidates(ctx, actor)
	if err != nil {
		return nil, err
	}
	visible := out[:0]
	nowAt := now()
	for _, item := range out {
		if DiscoveryA2UICandidateIsCurrent(item, nowAt) {
			visible = append(visible, item)
		}
	}
	return visible, nil
}

// DiscoveryA2UICandidate checks the exact receipt again before model egress.
func (s *Store) DiscoveryA2UICandidate(ctx context.Context, actor, conversation, candidate string, revision int64) (DiscoveryCandidate, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	defer tx.Rollback()
	return a2uiCandidateTx(ctx, tx, actor, conversation, candidate, revision)
}

type DiscoveryA2UIAction struct {
	Actor        string
	Conversation string
	ClientKey    string
	Candidate    string
	Revision     int64
	Name         string
	Fingerprint  string
	Snapshot     string
}

type DiscoveryA2UIReceipt struct {
	Run       Run
	Outcome   string
	Action    string
	Admission *DiscoveryA2UIAdmissionResult
}

func (a DiscoveryA2UIAction) key() string {
	return "discovery-a2ui:" + digest(toJSON([]string{a.Actor, a.Conversation, a.ClientKey}))
}

// BeginDiscoveryA2UIAction reserves a bounded model invocation before egress.
// An applying record is not a claim that a model completed. Neither restart
// nor a retry of that request ID re-dispatches an ambiguous invocation.
func (s *Store) BeginDiscoveryA2UIAction(ctx context.Context, a DiscoveryA2UIAction) (DiscoveryCandidate, DiscoveryA2UIReceipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	defer tx.Rollback()
	c, err := a2uiCandidateTx(ctx, tx, a.Actor, a.Conversation, a.Candidate, a.Revision)
	if err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if a.Snapshot == "" || c.A2UISnapshot != a.Snapshot {
		return c, DiscoveryA2UIReceipt{}, ErrA2UIDiscoveryStale
	}
	var seen, state, runID, outcome string
	key := a.key()
	err = tx.QueryRowContext(ctx, `SELECT payload_digest,state,coalesce(run_id,''),source_version FROM inbox_events
WHERE source=? AND event_id=? AND principal=?`, discoveryA2UISource, key, a.Actor).Scan(&seen, &state, &runID, &outcome)
	if err == nil {
		if seen != a.Fingerprint {
			return c, DiscoveryA2UIReceipt{}, ErrConflict
		}
		switch state {
		case "applied":
			run, err := scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=? AND conversation_id=?", runID, a.Conversation))
			return c, DiscoveryA2UIReceipt{Run: run, Outcome: outcome, Action: a.Name}, err
		case "failed":
			return c, DiscoveryA2UIReceipt{}, ErrA2UIActionFailed
		default:
			return c, DiscoveryA2UIReceipt{}, ErrA2UIActionPending
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if err := checkConversationSubmission(tx, a.Conversation); err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_events
WHERE source=? AND principal=? AND source_version=? AND state='applying' AND received_at>?)`,
		discoveryA2UISource, a.Actor, a.Conversation, now()-int64(25*time.Second/time.Millisecond)).Scan(&active); err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if active {
		return c, DiscoveryA2UIReceipt{}, ErrA2UIActionPending
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_events(source,event_id,source_seq,principal,kind,summary,source_version,payload_digest,payload_ref,received_at,state,attempts)
VALUES(?,?,?,?,?,?,?,?,?,?,'applying',1)`, discoveryA2UISource, key, a.Revision, a.Actor, a.Name, a.Candidate, a.Conversation, a.Fingerprint, a.Snapshot, now()); err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	return c, DiscoveryA2UIReceipt{}, tx.Commit()
}

func (s *Store) FinishDiscoveryA2UIAction(ctx context.Context, a DiscoveryA2UIAction, outcome, text, reply string) (DiscoveryCandidate, DiscoveryA2UIReceipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	defer tx.Rollback()
	c, err := a2uiCandidateTx(ctx, tx, a.Actor, a.Conversation, a.Candidate, a.Revision)
	if err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if a.Snapshot == "" || c.A2UISnapshot != a.Snapshot {
		return c, DiscoveryA2UIReceipt{}, ErrA2UIDiscoveryStale
	}
	key := a.key()
	change, err := tx.ExecContext(ctx, `UPDATE inbox_events SET state='applied',applied_at=?,source_version=? WHERE source=? AND event_id=? AND principal=? AND payload_digest=? AND state='applying'`,
		now(), outcome, discoveryA2UISource, key, a.Actor, a.Fingerprint)
	if err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if count, err := change.RowsAffected(); err != nil || count != 1 {
		return c, DiscoveryA2UIReceipt{}, ErrConflict
	}
	classified := submissionClassification{DelegationResult: DelegationResult{
		Requests: 1, Route: &RouteDecision{Kind: "clarify", Question: reply},
	}}
	run, err := submitWorkTxWithMode(ctx, tx, key, a.Fingerprint, text, "", a.Conversation, false, Delivery{}, &classified, nil, true)
	if err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE inbox_events SET run_id=? WHERE source=? AND event_id=?", run.ID, discoveryA2UISource, key); err != nil {
		return c, DiscoveryA2UIReceipt{}, err
	}
	return c, DiscoveryA2UIReceipt{Run: run, Outcome: outcome, Action: a.Name}, tx.Commit()
}

func (s *Store) FailDiscoveryA2UIAction(ctx context.Context, a DiscoveryA2UIAction) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_events SET state='failed',failure='action_not_completed'
WHERE source=? AND event_id=? AND principal=? AND payload_digest=? AND state='applying'`, discoveryA2UISource, a.key(), a.Actor, a.Fingerprint)
	return err
}

// ReadDiscoveryA2UIAction deliberately returns no live confirmation secret.
func (s *Store) ReadDiscoveryA2UIAction(ctx context.Context, actor, conversation, clientKey string) (DiscoveryCandidate, DiscoveryA2UIReceipt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	defer tx.Rollback()
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	request := DiscoveryA2UIAction{Actor: actor, Conversation: conversation, ClientKey: clientKey}
	var state, runID, candidate, payload string
	var revision int64
	var receipt DiscoveryA2UIReceipt
	err = tx.QueryRowContext(ctx, `SELECT state,coalesce(run_id,''),summary,source_seq,source_version,kind,payload_ref FROM inbox_events
WHERE source=? AND event_id=? AND principal=?`, discoveryA2UISource, request.key(), actor).
		Scan(&state, &runID, &candidate, &revision, &receipt.Outcome, &receipt.Action, &payload)
	if err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	if state == "applied" && receipt.Outcome == "admitted" {
		receipt.Admission = &DiscoveryA2UIAdmissionResult{}
		if json.Unmarshal([]byte(payload), receipt.Admission) != nil || receipt.Admission.ID != candidate ||
			receipt.Admission.Revision != revision || receipt.Admission.State != "admitted" {
			return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, ErrState
		}
		if err := authorizeDiscoveryA2UIAdmission(ctx, tx, actor, *receipt.Admission); err != nil {
			return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
		}
		receipt.Run, err = scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=? AND conversation_id=?", runID, conversation))
		return DiscoveryCandidate{}, receipt, err
	}
	current, err := a2uiCandidateTx(ctx, tx, actor, conversation, candidate, revision)
	if err != nil {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, err
	}
	if payload == "" || payload != current.A2UISnapshot {
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, ErrA2UIDiscoveryStale
	}
	switch state {
	case "applied":
		receipt.Run, err = scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=? AND conversation_id=?", runID, conversation))
		return current, receipt, err
	case "failed":
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, ErrA2UIActionFailed
	default:
		return DiscoveryCandidate{}, DiscoveryA2UIReceipt{}, ErrA2UIActionPending
	}
}

type DiscoveryA2UIAdmissionResult struct {
	ID        string `json:"id"`
	Revision  int64  `json:"revision"`
	State     string `json:"state"`
	Asset     string `json:"asset"`
	CheckTask string `json:"check_task_id,omitempty"`
}

func authorizeDiscoveryA2UIAdmission(ctx context.Context, tx *sql.Tx, actor string, receipt DiscoveryA2UIAdmissionResult) error {
	var allowed bool
	stamp := now()
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM discovery_admissions d
JOIN ops_assets a ON a.object_ref=d.object_ref
JOIN authorizations p ON p.object_ref=a.object_ref
WHERE d.candidate_id=? AND d.object_ref=? AND d.admitted_by=?
AND p.subject=? AND p.action='read' AND p.revoked_at=0 AND p.valid_from<=? AND (p.expires_at=0 OR p.expires_at>?))`,
		receipt.ID, receipt.Asset, actor, actor, stamp, stamp).Scan(&allowed)
	if err == nil && !allowed {
		return sql.ErrNoRows
	}
	return err
}

// ConfirmDiscoveryA2UIAction applies the asset write, original-conversation
// result, and durable receipt as one transaction. The HTTP confirmation secret
// is not persisted and is never needed for readback.
func (s *Store) ConfirmDiscoveryA2UIAction(ctx context.Context, a DiscoveryA2UIAction, runID string, admission DiscoveryAdmission) (DiscoveryA2UIReceipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	defer tx.Rollback()
	c, err := a2uiCandidateTx(ctx, tx, a.Actor, a.Conversation, a.Candidate, a.Revision)
	if err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	if c.A2UISnapshot != a.Snapshot || a.Snapshot == "" || admission.Candidate != c.ID ||
		admission.ExpectedSeenAt != c.SeenAt || admission.ConfirmedSerial != c.Serial {
		return DiscoveryA2UIReceipt{}, ErrA2UIDiscoveryStale
	}
	var target string
	if err := tx.QueryRowContext(ctx, `SELECT i.run_id FROM inbox_events i JOIN runs r ON r.id=i.run_id
WHERE i.source=? AND i.event_id=? AND i.principal=? AND i.payload_digest=? AND i.payload_ref=? AND i.kind='request_discovery_admission'
AND i.state='applied' AND i.source_version='show_admission_preview' AND r.conversation_id=? AND r.status='completed'`,
		discoveryA2UISource, a.key(), a.Actor, a.Fingerprint, a.Snapshot, a.Conversation).Scan(&target); err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	if target != runID {
		return DiscoveryA2UIReceipt{}, ErrConflict
	}
	applied, err := admitDiscoveryCandidateTx(ctx, tx, a.Actor, admission)
	if err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	result := DiscoveryA2UIAdmissionResult{ID: applied.ID, Revision: a.Revision, State: "admitted", Asset: applied.Asset, CheckTask: applied.CheckTask}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_events SET source_version='admitted',payload_ref=?,applied_at=?
WHERE source=? AND event_id=?`, toJSON(result), now(), discoveryA2UISource, a.key()); err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)",
		a.Conversation, runID, "已纳管候选资产，正式资产编号："+result.Asset+"。这不等于验收通过。"); err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	if err := addEvent(tx, runID, "discovery.a2ui_admitted", result); err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	run, err := scanRun(tx.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=?", runID))
	if err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return DiscoveryA2UIReceipt{}, err
	}
	s.notifyRunEvent(runID)
	return DiscoveryA2UIReceipt{Run: run, Outcome: "admitted", Action: "request_discovery_admission", Admission: &result}, nil
}

// AcquireDiscoveryA2UIModelSlot shares the account runtime's actual capacity;
// interrupted/busy requests do not dispatch an external model.
func (s *Store) AcquireDiscoveryA2UIModelSlot(ctx context.Context) (func(), error) {
	return s.acquireModelSlot(ctx)
}

// OpenDiscoveryA2UIConversation persists a real first turn for the account.
// No model, cluster scan, or task dispatch is involved in opening a list.
func (s *Store) OpenDiscoveryA2UIConversation(ctx context.Context, actor, clientKey string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	allowed, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return Run{}, err
	}
	if !allowed {
		return Run{}, ErrAuthorizationDenied
	}
	key := "open-discovery-a2ui:" + digest(toJSON([]string{actor, clientKey}))
	fingerprint := digest("open-discovery-a2ui")
	if prior, err := previousSubmission(tx, key, fingerprint); err != nil || prior.ID != "" {
		if err == nil {
			err = discoveryA2UIConversation(ctx, tx, actor, prior.ConversationID)
		}
		return prior, err
	}
	classified := submissionClassification{DelegationResult: DelegationResult{
		Route: &RouteDecision{Kind: "clarify", Question: "已打开候选资产列表。这里显示的是历史发现结果，尚未纳管。"},
	}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, "查看候选资产", "", "", false, Delivery{}, &classified, nil, true)
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}
