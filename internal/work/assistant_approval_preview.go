package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const assistantApprovalPreviewEvent = "assistant.approval_preview_snapshot:"

var approvalPreviewReceipts = assistantReadReceiptPolicy{
	source: "a2ui-approval-preview", kind: "preview_pending_approval", limit: 3 * assistantAttentionCardLimit,
}

type AssistantPendingApproval struct {
	Action, Object                                 string
	ExpiresAt                                      int64
	ContractRevision, PolicyVersion, AssetRevision int
	ConfirmAccess                                  string
	Fingerprint                                    string
	Receipt                                        *assistantApprovalReadReceipt
}

type assistantApprovalReadReceipt struct {
	Fingerprint, Outcome string
	CheckedAt            int64
}

func (a AssistantPendingApproval) Outcome() string {
	if a.Receipt == nil {
		return ""
	}
	return a.Receipt.Outcome
}

func (a AssistantPendingApproval) CheckedAt() int64 {
	if a.Receipt == nil {
		return 0
	}
	return a.Receipt.CheckedAt
}

// The approval owner may review their own proposal. A read neither decides
// the approval nor records an authorization decision. Only the real approval
// service can use the current confirm grant to authorize a later decision.
func readAssistantApprovalPreview(ctx context.Context, tx *sql.Tx, actor, approval, run, slot string, stamp int64) (AssistantPendingApproval, error) {
	var a Approval
	err := tx.QueryRowContext(ctx, `SELECT approval_id,subject,action,object_ref,params_digest,contract_revision,policy_version,
asset_revision,decided_by,expires_at,coalesce(request_key,''),created_at FROM approvals
WHERE approval_id=? AND subject=? AND decision='pending' AND decided_at=0 AND created_at<=? AND (expires_at=0 OR expires_at>?)`,
		approval, actor, stamp, stamp).Scan(&a.ApprovalID, &a.Subject, &a.Action, &a.ObjectRef, &a.ParamsDigest, &a.ContractRevision,
		&a.PolicyVersion, &a.AssetRevision, &a.DecidedBy, &a.ExpiresAt, &a.RequestKey, &a.CreatedAt)
	if err != nil {
		return AssistantPendingApproval{}, err
	}
	out := AssistantPendingApproval{Action: a.Action, Object: a.ObjectRef, ExpiresAt: a.ExpiresAt, ContractRevision: a.ContractRevision,
		PolicyVersion: a.PolicyVersion, AssetRevision: a.AssetRevision, ConfirmAccess: "not_granted"}
	var grant string
	var revision, policyVersion int
	var expiry int64
	if a.DecidedBy != "" && a.DecidedBy != actor {
		out.ConfirmAccess = "other_approver"
	} else {
		err = tx.QueryRowContext(ctx, `SELECT auth_id,revision,policy_version,expires_at FROM authorizations
WHERE subject=? AND action='confirm' AND object_ref=? AND revoked_at=0
AND valid_from<=? AND (expires_at=0 OR expires_at>?) ORDER BY policy_version DESC,auth_id LIMIT 1`,
			actor, a.ObjectRef, stamp, stamp).Scan(&grant, &revision, &policyVersion, &expiry)
		if err == nil {
			out.ConfirmAccess = "can_confirm"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return AssistantPendingApproval{}, err
		}
	}
	// Fold the exact authoritative source (including parameters, expiry and
	// prescribed approver) and current confirm grant into one opaque snapshot.
	out.Fingerprint = digest(toJSON([]any{a, grant, revision, policyVersion, expiry}))
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1`,
		run, assistantApprovalPreviewEvent+slot).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return AssistantPendingApproval{}, err
	}
	var receipt assistantApprovalReadReceipt
	if json.Unmarshal([]byte(raw), &receipt) != nil {
		return AssistantPendingApproval{}, ErrState
	}
	if receipt.Fingerprint == out.Fingerprint {
		out.Receipt = &receipt
	}
	return out, nil
}

func currentApprovalReview(card AssistantAttentionListCard, a AssistantReadCardAction) (string, AssistantPendingApproval, error) {
	slot, fingerprint, ok := strings.Cut(a.Target, ":")
	review, found := card.Reviews[slot]
	if !ok || !found || a.Revision != card.Revision || review.Fingerprint != fingerprint {
		return "", AssistantPendingApproval{}, ErrConversationRevision
	}
	return slot, review, nil
}

func (s *Store) BeginAssistantApprovalPreview(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantAttentionCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	_, current, err := currentApprovalReview(card, a)
	if err != nil {
		return false, err
	}
	done, err := beginAssistantReadReceipt(ctx, tx, approvalPreviewReceipts, a, current.Receipt != nil)
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FinishAssistantApprovalPreview(ctx context.Context, a AssistantReadCardAction, outcome string) error {
	switch outcome {
	case "show_approval_preview", "ask_for_details", "cannot_prepare":
	default:
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
	slot, current, err := currentApprovalReview(card, a)
	if err != nil {
		return err
	}
	if err = finishAssistantReadReceipt(ctx, tx, approvalPreviewReceipts, a); err != nil {
		return err
	}
	if current.Receipt == nil {
		receipt := assistantApprovalReadReceipt{Fingerprint: current.Fingerprint, Outcome: outcome, CheckedAt: now()}
		if err = addEvent(tx, a.Run, assistantApprovalPreviewEvent+slot, receipt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FailAssistantApprovalPreview(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, approvalPreviewReceipts, a)
}
