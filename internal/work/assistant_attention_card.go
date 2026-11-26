package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

const assistantAttentionCardLimit = 10
const assistantAttentionListEvent = "assistant.attention_list_snapshot"
const assistantAttentionReasonEvent = "assistant.attention_reason_snapshot:"

var attentionCardReceiptPolicy = assistantReadReceiptPolicy{"assistant-attention-reason", "query_attention_reason", 3 * assistantAttentionCardLimit}

type AssistantAttentionListCard struct {
	IDs        []string
	More       bool
	Revision   int64                               `json:"-"`
	Rows       []AssistantAttentionCardRow         `json:"-"`
	Suppressed bool                                `json:"-"`
	Reasons    map[string]AssistantAttentionReason `json:"-"`
	Reviews    map[string]AssistantPendingApproval `json:"-"`
}

type AssistantAttentionCardRow struct {
	Slot string
	Item AttentionDetail
}

type AssistantAttentionReason struct {
	Decision, State, Kind string
	RecordedAt, CheckedAt int64
}

func (s *Store) OpenAssistantAttentionList(ctx context.Context, actor, conversation, request, text string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil {
		return Run{}, err
	} else if !allowed {
		return Run{}, ErrAuthorizationDenied
	}
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key := "assistant-attention-list:" + digest(toJSON([]string{actor, request}))
	fp := digest(toJSON([]string{assistantAttentionListEvent, conversation, text}))
	if prior, err := previousSubmission(tx, key, fp); err != nil || prior.ID != "" {
		return prior, err
	}
	page, err := listAttention(ctx, tx, actor, now(), assistantAttentionCardLimit, AttentionCursor{})
	if err != nil {
		return Run{}, err
	}
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{
		Kind: "clarify", Question: "我把你可查看的待处理事项列在下面。选一项可以核对原因，不会替你作决定。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fp, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, run.ID, assistantAttentionListEvent, AssistantAttentionListCard{IDs: ids, More: page.Next != nil}); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func assistantAttentionCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantAttentionListCard, error) {
	var card AssistantAttentionListCard
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return card, err
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantAttentionListEvent, conversation, run).Scan(&card.Revision, &snapshot)
	if err != nil {
		return card, err
	}
	if err := json.Unmarshal([]byte(snapshot), &card); err != nil {
		return card, err
	}
	if len(card.IDs) > assistantAttentionCardLimit {
		return card, ErrState
	}
	stamp := now()
	card.Reasons = make(map[string]AssistantAttentionReason)
	card.Reviews = make(map[string]AssistantPendingApproval)
	for i, itemID := range card.IDs {
		item, err := readAttention(ctx, tx, actor, itemID, stamp)
		if errors.Is(err, sql.ErrNoRows) {
			card.Suppressed = true
			continue
		}
		if err != nil {
			return card, err
		}
		slot := strconv.Itoa(i + 1)
		card.Rows = append(card.Rows, AssistantAttentionCardRow{Slot: slot, Item: item})
		if item.Kind == "approval" && item.State == "decision_required" {
			review, err := readAssistantApprovalPreview(ctx, tx, actor, item.ApprovalID, run, slot, stamp)
			if err == nil {
				card.Reviews[slot] = review
			} else if !errors.Is(err, sql.ErrNoRows) {
				return card, err
			}
		}
		var raw string
		err = tx.QueryRowContext(ctx, "SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1", run, assistantAttentionReasonEvent+slot).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return card, err
		}
		var reason AssistantAttentionReason
		if err := json.Unmarshal([]byte(raw), &reason); err != nil {
			return card, err
		}
		card.Reasons[slot] = reason
	}
	return card, nil
}

func (s *Store) AssistantAttentionCard(ctx context.Context, actor, conversation, run string) (AssistantAttentionListCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantAttentionListCard{}, err
	}
	defer tx.Rollback()
	return assistantAttentionCardTx(ctx, tx, actor, conversation, run)
}

func (card AssistantAttentionListCard) Row(target string) (AssistantAttentionCardRow, bool) {
	for _, row := range card.Rows {
		if target == row.Slot {
			return row, true
		}
	}
	return AssistantAttentionCardRow{}, false
}

func (s *Store) BeginAssistantAttentionReason(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantAttentionCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	if _, visible := card.Row(a.Target); a.Revision != card.Revision || !visible {
		return false, ErrConversationRevision
	}
	_, restored := card.Reasons[a.Target]
	done, err := beginAssistantReadReceipt(ctx, tx, attentionCardReceiptPolicy, a, restored)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FailAssistantAttentionReason(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, attentionCardReceiptPolicy, a)
}

func (s *Store) FinishAssistantAttentionReason(ctx context.Context, a AssistantReadCardAction, decision string) error {
	if decision != "show_reason" && decision != "ask_for_details" && decision != "cannot_prepare" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantAttentionCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	row, visible := card.Row(a.Target)
	if a.Revision != card.Revision || !visible {
		return ErrConversationRevision
	}
	if err := finishAssistantReadReceipt(ctx, tx, attentionCardReceiptPolicy, a); err != nil {
		return err
	}
	if _, exists := card.Reasons[a.Target]; exists {
		return tx.Commit()
	}
	reason := AssistantAttentionReason{Decision: decision, CheckedAt: now()}
	if decision == "show_reason" {
		reason.Kind, reason.State, reason.RecordedAt = row.Item.Source.Kind, row.Item.State, row.Item.Source.RecordedAt
	}
	if err := addEvent(tx, a.Run, assistantAttentionReasonEvent+row.Slot, reason); err != nil {
		return err
	}
	return tx.Commit()
}
