package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

const assistantAssetCardLimit = 10
const assistantAssetListEvent = "assistant.asset_list_snapshot"
const assistantAssetStatusEvent = "assistant.asset_status_snapshot:"

var assetCardReceiptPolicy = assistantReadReceiptPolicy{"assistant-asset-status", "query_asset_ledger", 3 * assistantAssetCardLimit}

type AssistantAssetCardRow struct {
	Slot, Ref, Kind, State, Binding string
}

type AssistantAssetCard struct {
	Rows       []AssistantAssetCardRow
	More       bool
	Revision   int64                           `json:"-"`
	Suppressed bool                            `json:"-"`
	Status     map[string]AssistantAssetStatus `json:"-"`
}

type AssistantAssetStatus struct {
	Decision, State, Health, Reachability, Occupancy string
	Revision, CheckedAt, ObservedAt                  int64
	Frozen                                           bool
}

func assetCardBinding(kind, identity string, created, epoch int64) string {
	return digest(toJSON([]any{kind, identity, created, epoch}))
}

// OpenAssistantAssetList stores a bounded reference snapshot alongside the
// request. It reads only the signed-in account's private ledger, not a source
// connection, and starts no business Task.
func (s *Store) OpenAssistantAssetList(ctx context.Context, actor, conversation, request, text string) (Run, error) {
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
	key := "assistant-asset-list:" + digest(toJSON([]string{actor, request}))
	fingerprint := digest(toJSON([]string{assistantAssetListEvent, conversation, text}))
	if prior, err := previousSubmission(tx, key, fingerprint); err != nil || prior.ID != "" {
		return prior, err
	}
	card, err := snapshotAssistantAssets(ctx, tx, actor)
	if err != nil {
		return Run{}, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{
		Kind: "clarify", Question: "我列出了当前账号的资产台账。你可以选一项核对记录，这不会连接设备。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, assistantAssetListEvent, card); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

// Like decideAuthorization with an empty scope, this list and the individual
// read require an exact object grant. The check has no business writes: a GET
// replay cannot create audit writes or dispatch work just to project a card.
const assistantAssetReadGrant = `EXISTS(SELECT 1 FROM authorizations g WHERE g.object_ref=a.object_ref
 AND g.subject=? AND g.action='read' AND g.revoked_at=0 AND g.valid_from<=? AND (g.expires_at=0 OR g.expires_at>?))`

func snapshotAssistantAssets(ctx context.Context, tx *sql.Tx, actor string) (AssistantAssetCard, error) {
	var card AssistantAssetCard
	stamp := now()
	rows, err := tx.QueryContext(ctx, `SELECT a.object_ref,a.kind,a.state,a.identity,a.created_at,a.ownership_epoch
FROM ops_assets a WHERE `+assistantAssetReadGrant+` ORDER BY a.object_ref LIMIT ?`, actor, stamp, stamp, assistantAssetCardLimit+1)
	if err != nil {
		return card, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(card.Rows) == assistantAssetCardLimit {
			card.More = true
			break
		}
		var row AssistantAssetCardRow
		var identity string
		var created, epoch int64
		if err := rows.Scan(&row.Ref, &row.Kind, &row.State, &identity, &created, &epoch); err != nil {
			return card, err
		}
		row.Slot = strconv.Itoa(len(card.Rows) + 1)
		row.Binding = assetCardBinding(row.Kind, identity, created, epoch)
		card.Rows = append(card.Rows, row)
	}
	return card, rows.Err()
}

func assistantAssetCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantAssetCard, error) {
	var card AssistantAssetCard
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return card, err
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantAssetListEvent, conversation, run).Scan(&card.Revision, &snapshot)
	if err != nil {
		return card, err
	}
	if err := json.Unmarshal([]byte(snapshot), &card); err != nil {
		return card, err
	}
	if len(card.Rows) > assistantAssetCardLimit {
		return card, ErrState
	}
	kept := card.Rows[:0]
	stamp := now()
	for ordinal, row := range card.Rows {
		if row.Slot != strconv.Itoa(ordinal+1) {
			return card, ErrState
		}
		var kind, identity string
		var created, epoch int64
		err := tx.QueryRowContext(ctx, `SELECT a.kind,a.identity,a.created_at,a.ownership_epoch FROM ops_assets a
WHERE a.object_ref=? AND `+assistantAssetReadGrant, row.Ref, actor, stamp, stamp).Scan(&kind, &identity, &created, &epoch)
		if errors.Is(err, sql.ErrNoRows) || err == nil && row.Binding != assetCardBinding(kind, identity, created, epoch) {
			card.Suppressed = true
			continue
		}
		if err != nil {
			return card, err
		}
		kept = append(kept, row)
	}
	card.Rows = kept
	card.Status = make(map[string]AssistantAssetStatus, len(kept))
	for _, row := range kept {
		var raw string
		err := tx.QueryRowContext(ctx, "SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1",
			run, assistantAssetStatusEvent+row.Slot).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return card, err
		}
		var result AssistantAssetStatus
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return card, err
		}
		card.Status[row.Slot] = result
	}
	return card, nil
}

func (s *Store) AssistantAssetCard(ctx context.Context, actor, conversation, run string) (AssistantAssetCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantAssetCard{}, err
	}
	defer tx.Rollback()
	return assistantAssetCardTx(ctx, tx, actor, conversation, run)
}

func (a AssistantAssetCard) Row(slot string) (AssistantAssetCardRow, bool) {
	for _, row := range a.Rows {
		if row.Slot == slot {
			return row, true
		}
	}
	return AssistantAssetCardRow{}, false
}

func (s *Store) BeginAssistantAssetStatus(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantAssetCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	if _, visible := card.Row(a.Target); a.Revision != card.Revision || !visible {
		return false, ErrConversationRevision
	}
	_, restored := card.Status[a.Target]
	done, err := beginAssistantReadReceipt(ctx, tx, assetCardReceiptPolicy, a, restored)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FailAssistantAssetStatus(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, assetCardReceiptPolicy, a)
}

func (s *Store) FinishAssistantAssetStatus(ctx context.Context, a AssistantReadCardAction, decision string) error {
	if decision != "show_ledger" && decision != "ask_for_details" && decision != "cannot_prepare" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantAssetCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	row, visible := card.Row(a.Target)
	if a.Revision != card.Revision || !visible {
		return ErrConversationRevision
	}
	if err := finishAssistantReadReceipt(ctx, tx, assetCardReceiptPolicy, a); err != nil {
		return err
	}
	if _, exists := card.Status[row.Slot]; exists {
		return tx.Commit()
	}
	result := AssistantAssetStatus{Decision: decision, CheckedAt: now()}
	if decision == "show_ledger" {
		if err := tx.QueryRowContext(ctx, `SELECT a.state,a.revision,a.frozen_at>0,
 coalesce(t.health,'unknown'),coalesce(t.reachability,'unknown'),coalesce(t.business_occupancy,''),coalesce(t.occurred_at,0)
FROM ops_assets a LEFT JOIN asset_transitions t ON t.transition_id=(
 SELECT e.transition_id FROM asset_transitions e WHERE e.object_ref=a.object_ref
 ORDER BY e.occurred_at DESC,e.created_at DESC,e.transition_id DESC LIMIT 1)
WHERE a.object_ref=?`, row.Ref).Scan(&result.State, &result.Revision, &result.Frozen,
			&result.Health, &result.Reachability, &result.Occupancy, &result.ObservedAt); err != nil {
			return err
		}
	}
	if err := addEvent(tx, a.Run, assistantAssetStatusEvent+row.Slot, result); err != nil {
		return err
	}
	return tx.Commit()
}
