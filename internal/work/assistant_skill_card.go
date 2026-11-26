package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const assistantSkillCardEvent = "assistant.skill_binding_options"
const assistantSkillPreviewEvent = "assistant.skill_binding_preview:"
const assistantSkillAppliedEvent = "assistant.skill_binding_applied"
const assistantSkillConfirmTTL = 10 * time.Minute

var ErrAssistantSkillPreviewStale = errors.New("这份 Skill 确认已失效、会话正忙或设置已变化。请先重新读取回执，核对当前设置。")

const assistantSkillCardLimit = 8

var skillPreviewReceipts = assistantReadReceiptPolicy{
	source: "a2ui-skill-binding-preview", kind: "preview_conversation_skill", limit: assistantSkillCardLimit * 3,
}

type assistantSkillCardRef struct {
	ID, Fingerprint string
	Version         int
}

type AssistantSkillCard struct {
	Refs                      []assistantSkillCardRef
	SelectionFingerprint      string
	More                      bool
	Revision                  int64                         `json:"-"`
	Current                   ConversationSkillSelection    `json:"-"`
	Rows                      []AssistantSkillCardRow       `json:"-"`
	Applied                   *AssistantSkillBindingReceipt `json:"-"`
	Changed, Suppressed, Busy bool                          `json:"-"`
}

type AssistantSkillCardRow struct {
	Slot, Scope, Fingerprint, Outcome string
	Version                           int
	AlreadySelected                   bool
	PreviewRevision, ConfirmBy        int64
	PreviewSession                    string
}

type assistantSkillPreviewReceipt struct {
	Proof, Outcome string
	Revision       int64
	ConfirmBy      int64
	Session        string
}

type AssistantSkillPreviewConfirmation struct {
	Actor, Conversation, Run, Slot, Session string
	Revision, ConfirmBy                     int64
}

type AssistantSkillBindingReceipt struct {
	Conversation    string `json:"conversation_id"`
	Run             string `json:"run_id"`
	Slot            string `json:"slot"`
	BindingRevision int    `json:"binding_revision"`
	ConfirmedAt     int64  `json:"confirmed_at_unix_ms"`
}

func snapshotAssistantSkillOptions(ctx context.Context, tx *sql.Tx, conversation string) (AssistantSkillCard, error) {
	var out AssistantSkillCard
	current, err := readConversationSkillSelection(ctx, tx, conversation)
	if err != nil {
		return out, err
	}
	out.SelectionFingerprint = digest(toJSON(current))
	rows, err := tx.QueryContext(ctx, `SELECT s.id,v.version
FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id AND v.version=s.active_version
WHERE v.approved_at IS NOT NULL AND length(v.instructions)>0 ORDER BY s.scope_label,s.id LIMIT ?`, assistantSkillCardLimit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var refs []assistantSkillCardRef
	for rows.Next() {
		var a assistantSkillCardRef
		if err := rows.Scan(&a.ID, &a.Version); err != nil {
			return out, err
		}
		refs = append(refs, a)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	out.More = len(refs) > assistantSkillCardLimit
	for _, ref := range refs {
		if len(out.Refs) == assistantSkillCardLimit {
			out.More = true
			break
		}
		candidate, err := readBindableConversationSkill(ctx, tx, ref.ID, ref.Version)
		if errors.Is(err, ErrExperienceSource) || errors.Is(err, ErrState) {
			continue
		}
		if err != nil {
			return out, err
		}
		ref.Fingerprint = digest(toJSON(candidate))
		out.Refs = append(out.Refs, ref)
	}
	return out, nil
}

func (s *Store) OpenAssistantSkillOptions(ctx context.Context, actor, conversation, request, text string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil || !allowed {
		if err == nil {
			err = ErrAuthorizationDenied
		}
		return Run{}, err
	}
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key := "assistant-skill-options:" + digest(toJSON([]string{actor, request}))
	fp := digest(toJSON([]string{assistantSkillCardEvent, conversation, text}))
	if old, err := previousSubmission(tx, key, fp); err != nil || old.ID != "" {
		return old, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "clarify",
		Question: "可以预览已启用且可选的 Skill。预览不会修改本会话，也不会运行 Skill 正文。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fp, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	card, err := snapshotAssistantSkillOptions(ctx, tx, run.ConversationID)
	if err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, run.ID, assistantSkillCardEvent, card); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func assistantSkillCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (AssistantSkillCard, error) {
	var card AssistantSkillCard
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return card, err
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantSkillCardEvent, conversation, run).Scan(&card.Revision, &raw)
	if err != nil {
		return card, err
	}
	if err = json.Unmarshal([]byte(raw), &card); err != nil || len(card.Refs) > assistantSkillCardLimit {
		return card, ErrState
	}
	current, err := readConversationSkillSelection(ctx, tx, conversation)
	if err != nil {
		return card, err
	}
	card.Current = current
	card.Applied, err = assistantSkillAppliedReceipt(ctx, tx, conversation, run)
	if err != nil {
		return card, err
	}
	card.Changed = digest(toJSON(current)) != card.SelectionFingerprint
	if card.Changed {
		return card, nil
	}
	if err = checkConversationSubmission(tx, conversation); errors.Is(err, ErrActive) {
		card.Busy = true
	} else if err != nil {
		return card, err
	}
	for n, ref := range card.Refs {
		item, err := readBindableConversationSkill(ctx, tx, ref.ID, ref.Version)
		if errors.Is(err, ErrExperienceSource) || errors.Is(err, ErrState) {
			card.Suppressed = true
			continue
		}
		if err != nil {
			return card, err
		}
		if digest(toJSON(item)) != ref.Fingerprint {
			card.Suppressed = true
			continue
		}
		slot := strconv.Itoa(n + 1)
		row := AssistantSkillCardRow{Slot: slot, Scope: item.Scope, Version: item.Version,
			Fingerprint:     digest(toJSON([]string{ref.Fingerprint, card.SelectionFingerprint})),
			AlreadySelected: current.Binding != nil && current.Binding.SkillID == item.ID && current.Binding.Version == item.Version && current.Binding.Validity == "valid"}
		receipt, err := readAssistantSkillPreview(ctx, tx, run, slot)
		if err == nil {
			if receipt.Proof != row.Fingerprint {
				return card, ErrState
			}
			row.Outcome, row.PreviewSession = receipt.Outcome, receipt.Session
			row.PreviewRevision, row.ConfirmBy = receipt.Revision, receipt.ConfirmBy
		} else if !errors.Is(err, sql.ErrNoRows) {
			return card, err
		}
		card.Rows = append(card.Rows, row)
	}
	return card, nil
}

func (s *Store) AssistantSkillOptions(ctx context.Context, actor, conversation, run string) (AssistantSkillCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssistantSkillCard{}, err
	}
	defer tx.Rollback()
	return assistantSkillCardTx(ctx, tx, actor, conversation, run)
}

func (c AssistantSkillCard) Selectable(slot string) (AssistantSkillCardRow, bool) {
	if c.Busy || c.Changed {
		return AssistantSkillCardRow{}, false
	}
	for _, row := range c.Rows {
		if row.Slot == slot && !row.AlreadySelected {
			return row, true
		}
	}
	return AssistantSkillCardRow{}, false
}

func skillCardActionRow(card AssistantSkillCard, a AssistantReadCardAction) (AssistantSkillCardRow, error) {
	slot, proof, ok := strings.Cut(a.Target, ":")
	row, found := card.Selectable(slot)
	if !ok || !found || a.Revision != card.Revision || row.Fingerprint != proof {
		return row, ErrConversationRevision
	}
	return row, nil
}

func (s *Store) BeginAssistantSkillPreview(ctx context.Context, a AssistantReadCardAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := assistantSkillCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return false, err
	}
	row, err := skillCardActionRow(card, a)
	if err != nil {
		return false, err
	}
	done, err := beginAssistantReadReceipt(ctx, tx, skillPreviewReceipts, a, row.Outcome != "")
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FinishAssistantSkillPreview(ctx context.Context, a AssistantReadCardAction, outcome, previewSession string) error {
	switch outcome {
	case "show_binding_preview", "ask_for_details", "cannot_prepare":
	default:
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := assistantSkillCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return err
	}
	row, err := skillCardActionRow(card, a)
	if err != nil {
		return err
	}
	if err = finishAssistantReadReceipt(ctx, tx, skillPreviewReceipts, a); err != nil {
		return err
	}
	if row.Outcome == "" {
		receipt := assistantSkillPreviewReceipt{Proof: row.Fingerprint, Outcome: outcome}
		if outcome == "show_binding_preview" && previewSession != "" {
			receipt.Revision, receipt.Session = a.Revision, previewSession
			receipt.ConfirmBy = now() + assistantSkillConfirmTTL.Milliseconds()
		}
		if err = addEvent(tx, a.Run, assistantSkillPreviewEvent+row.Slot, receipt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FailAssistantSkillPreview(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, skillPreviewReceipts, a)
}

func readAssistantSkillPreview(ctx context.Context, tx *sql.Tx, run, slot string) (assistantSkillPreviewReceipt, error) {
	var out assistantSkillPreviewReceipt
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1`,
		run, assistantSkillPreviewEvent+slot).Scan(&raw); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, ErrState
	}
	return out, nil
}

func assistantSkillAppliedReceipt(ctx context.Context, tx *sql.Tx, conversation, run string) (*AssistantSkillBindingReceipt, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1`,
		run, assistantSkillAppliedEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out AssistantSkillBindingReceipt
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Conversation != conversation || out.Run != run || out.BindingRevision < 1 || out.ConfirmedAt < 1 {
		return nil, ErrState
	}
	return &out, nil
}

// ConfirmAssistantSkillPreview binds a proven catalog version for future runs
// only. HTTP verifies the form signature; this transaction rechecks its
// session/revision against the saved preview and applies the existing CAS.
func (s *Store) ConfirmAssistantSkillPreview(ctx context.Context, a AssistantSkillPreviewConfirmation) (AssistantSkillBindingReceipt, error) {
	var out AssistantSkillBindingReceipt
	n, err := strconv.Atoi(a.Slot)
	if err != nil || n < 1 || n > assistantSkillCardLimit || strconv.Itoa(n) != a.Slot || a.Session == "" || a.ConfirmBy <= now() {
		return out, ErrAssistantSkillPreviewStale
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	card, err := assistantSkillCardTx(ctx, tx, a.Actor, a.Conversation, a.Run)
	if err != nil {
		return out, err
	}
	preview, err := readAssistantSkillPreview(ctx, tx, a.Run, a.Slot)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (preview.Outcome != "show_binding_preview" ||
		preview.Session != a.Session || preview.Revision != a.Revision || preview.ConfirmBy != a.ConfirmBy || preview.ConfirmBy <= now()) {
		return out, ErrAssistantSkillPreviewStale
	}
	if err != nil {
		return out, err
	}
	if card.Applied != nil {
		if card.Applied.Slot != a.Slot {
			return out, ErrAssistantSkillPreviewStale
		}
		// Read the original committed outcome, not the current selection. Never
		// reinstate the old binding if it was later changed or withdrawn.
		return *card.Applied, nil
	}
	row, ok := card.Selectable(a.Slot)
	if !ok || card.Revision != a.Revision || row.Fingerprint != preview.Proof || n > len(card.Refs) {
		return out, ErrAssistantSkillPreviewStale
	}
	ref := card.Refs[n-1]
	bindingRevision := card.Current.BindingRevision
	selection, err := setConversationSkillSelectionTx(ctx, tx, a.Actor, a.Conversation, ConversationSkillBindingInput{
		Action: "bind_skill", SkillID: ref.ID, Version: ref.Version, ExpectedBindingRevision: &bindingRevision,
	})
	if err != nil {
		return out, err
	}
	if selection.BindingRevision != bindingRevision+1 || selection.Binding == nil || selection.Binding.Validity != "valid" {
		return out, ErrAssistantSkillPreviewStale
	}
	out = AssistantSkillBindingReceipt{Conversation: a.Conversation, Run: a.Run, Slot: a.Slot, BindingRevision: selection.BindingRevision, ConfirmedAt: now()}
	if err := addEvent(tx, a.Run, assistantSkillAppliedEvent, out); err != nil {
		return AssistantSkillBindingReceipt{}, err
	}
	return out, tx.Commit()
}
