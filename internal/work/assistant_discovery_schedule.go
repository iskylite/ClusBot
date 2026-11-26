package work

import (
	"context"
	"database/sql"
	"encoding/json"
)

const assistantDiscoveryScheduleEvent = "assistant.discovery_schedule_requested"

type assistantDiscoveryScheduleAnchor struct {
	ID, Identity, InitialState string
}

type AssistantDiscoveryScheduleCard struct {
	Revision int64
	Current  DiscoverySetupStatus
	Changed  bool
	Proof    string
}

func assistantDiscoveryScheduleIdentity(p *DiscoverySetupPlan) string {
	if p == nil {
		return ""
	}
	return digest(toJSON([]any{p.ID, p.Source, p.Scope, p.PeriodSeconds, p.Revision, p.Scan}))
}

// Only a conversation record is created. The existing discovery projection
// owns plan selection and current read authority; no timer or scan is created.
func (s *Store) OpenAssistantDiscoverySchedule(ctx context.Context, actor, conversation, request, text string, adapter DiscoveryScanSource) (Run, error) {
	allowed, err := ownsPersonalBusiness(s.db, actor)
	if err != nil {
		return Run{}, err
	}
	if !allowed {
		return Run{}, sql.ErrNoRows
	}
	current, err := s.ReadDiscoverySetupStatus(ctx, actor, adapter, "", "")
	if err != nil {
		return Run{}, err
	}
	anchor := assistantDiscoveryScheduleAnchor{InitialState: current.State}
	if current.Plan != nil && current.State != "source_unavailable" {
		anchor.ID, anchor.Identity = current.Plan.ID, assistantDiscoveryScheduleIdentity(current.Plan)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key := "assistant-discovery-schedule:" + digest(toJSON([]string{actor, request}))
	fp := digest(toJSON([]string{assistantDiscoveryScheduleEvent, conversation, text}))
	if old, err := previousSubmission(tx, key, fp); err != nil || old.ID != "" {
		return old, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{
		Kind: "clarify", Question: "我可以核对一项已启用的资产发现计划和它的最近记录。查看不会更改计划或发起探测。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fp, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, assistantDiscoveryScheduleEvent, anchor); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func (s *Store) readAssistantDiscoveryScheduleAnchor(ctx context.Context, actor, conversation, run string) (int64, assistantDiscoveryScheduleAnchor, error) {
	var anchor assistantDiscoveryScheduleAnchor
	var revision int64
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, anchor, err
	}
	defer tx.Rollback()
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return 0, anchor, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantDiscoveryScheduleEvent, conversation, run).Scan(&revision, &raw); err != nil {
		return 0, anchor, err
	}
	if json.Unmarshal([]byte(raw), &anchor) != nil || (anchor.ID != "" && (!validBenchKey(anchor.ID) || anchor.Identity == "")) {
		return 0, anchor, ErrState
	}
	return revision, anchor, nil
}

func (s *Store) ReadAssistantDiscoverySchedule(ctx context.Context, actor, conversation, run string, adapter DiscoveryScanSource) (AssistantDiscoveryScheduleCard, error) {
	var card AssistantDiscoveryScheduleCard
	revision, anchor, err := s.readAssistantDiscoveryScheduleAnchor(ctx, actor, conversation, run)
	if err != nil {
		return card, err
	}
	card.Revision = revision
	if anchor.ID == "" {
		// An empty old reply must not silently attach to a newly enabled plan.
		card.Current.State = anchor.InitialState
		return card, nil
	}
	current, err := s.readDiscoverySetupStatus(ctx, actor, adapter, "", "", anchor.ID)
	if err != nil {
		return card, err
	}
	if current.State == "source_unavailable" || current.Plan == nil || assistantDiscoveryScheduleIdentity(current.Plan) != anchor.Identity {
		card.Changed = true
		return card, nil
	}
	card.Current = current
	card.Proof = digest(toJSON([]any{anchor.Identity, current}))
	return card, nil
}
